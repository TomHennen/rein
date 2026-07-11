// `rein declare <n> [--repo owner/name]` — the agent declares which
// issue its work is for (issue #35 §3). The human confirms on their
// terminal (Form A); on approval the issue joins the run's confirmed
// set and writes unlock for the rest of the run.
//
// One subcommand, two transports, selected by environment:
//
//   - REIN_RUN_ID present  ⇒ DIRECT path: this process is inside a
//     `rein run` (direct mode) wrapped shell — same uid, network and
//     keystore in hand — so it fetches the issue and fires the grant
//     machinery itself (internal/declare.Run in-process).
//   - REIN_RUN_ID absent   ⇒ SANDBOXED path: the strict env allowlist
//     never passes REIN_RUN_ID into the sandbox, so absence means "in
//     the sandbox (or outside any run)". The declaration rides the
//     sandbox's only channel to rein — the per-run proxy socket — as
//     the declare.rein.internal virtual host; the broker side performs
//     the identical fetch + prompt + record steps out-of-sandbox. If
//     that host is unreachable too, we are outside any run: fail with
//     the launch instruction (§6).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/declare"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/grant"
)

// DeclareHost is the local-only virtual host the sandboxed declare
// rides (issue #35 §3). Kept here as the single client-side constant;
// the proxy-side handler and srt domain list use proxy.DeclareHost —
// see internal/proxy/hosts.go (the two are asserted equal in tests).
const declareHostURL = "https://declare.rein.internal/v1/declare"

// declareRequestTimeout bounds the in-sandbox declare call. Generous:
// the request BLOCKS while the human decides (prompt timeout 60s +
// popup layering), and a hung socket should still fail eventually.
const declareRequestTimeout = 5 * time.Minute

// issueArgPattern is the strict CLI shape for the declared number:
// decimal, no leading zeros, bounded — the same number grammar the
// push-ref convention accepts (§5.1), so a declare that succeeds can
// always be pushed.
var issueArgPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// runDeclare is the `rein declare` entry point. args is os.Args[2:].
// Returns (exitCode, error) so the caller owns process exit — no
// os.Exit() inside, which would skip the deferred log close.
func runDeclare(args []string) (int, error) {
	number, repoFlag, err := parseDeclareArgs(args)
	if err != nil {
		return 2, err
	}
	if runID := os.Getenv("REIN_RUN_ID"); runID != "" {
		return declareDirect(number, repoFlag, runID)
	}
	return declareViaProxy(number, repoFlag)
}

// parseDeclareArgs validates `rein declare <n> [--repo owner/name]`.
func parseDeclareArgs(args []string) (number int, repoFlag string, err error) {
	var numArg string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--repo":
			if i+1 >= len(args) {
				return 0, "", fmt.Errorf("usage: rein declare <issue-number> [--repo owner/name]")
			}
			repoFlag = args[i+1]
			i++
		case strings.HasPrefix(a, "--repo="):
			repoFlag = strings.TrimPrefix(a, "--repo=")
		case strings.HasPrefix(a, "-"):
			return 0, "", fmt.Errorf("rein declare: unknown flag %q (usage: rein declare <issue-number> [--repo owner/name])", a)
		case numArg == "":
			numArg = a
		default:
			return 0, "", fmt.Errorf("rein declare: unexpected argument %q", a)
		}
	}
	if numArg == "" {
		return 0, "", fmt.Errorf("usage: rein declare <issue-number> [--repo owner/name]")
	}
	if !issueArgPattern.MatchString(numArg) {
		return 0, "", fmt.Errorf("rein declare: %q is not a valid issue number (positive decimal, no leading zeros)", numArg)
	}
	number, err = strconv.Atoi(numArg)
	if err != nil {
		return 0, "", fmt.Errorf("rein declare: parse %q: %w", numArg, err)
	}
	return number, repoFlag, nil
}

// declareDirect runs the declaration fully in-process (direct mode).
func declareDirect(number int, repoFlag, runID string) (int, error) {
	logger, closeLog, err := openLog()
	if err != nil {
		return 1, err
	}
	defer closeLog()

	stateDir, err := config.StateDir()
	if err != nil {
		return 1, err
	}
	sess, sessSource, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return 1, fmt.Errorf("load session: %w", err)
	}
	logger.Printf("declare (direct): issue=%d repo=%q run=%s session=%s source=%s", number, repoFlag, runID, sess.ID, sessSource)

	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		return 1, fmt.Errorf("resolve App config: %w (run `rein init` / `rein doctor`)", err)
	}
	appCfg.RepoNames = sess.BareRepoNames()

	deps := declare.Deps{
		StateDir: stateDir,
		RunID:    runID,
		RunPID:   envInt("REIN_RUN_PID"),
		Session:  sess,
		Fetch: func(ctx context.Context, repo string, n int) (issuemeta.Meta, error) {
			client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
			if err != nil {
				return issuemeta.Meta{}, err
			}
			mctx, cancel := context.WithTimeout(ctx, mintTimeout)
			token, _, err := client.MintGhReadOnlyToken(mctx)
			cancel()
			if err != nil {
				return issuemeta.Meta{}, fmt.Errorf("mint read token for issue fetch: %w", err)
			}
			return issuemeta.Fetch(ctx, os.Getenv("REIN_GITHUB_API_BASE"), token, repo, n)
		},
		Grant: grant.Config{
			TTL:           approvalTTL,
			PromptTimeout: 60 * time.Second,
			PreferPopup:   grant.PopupPreferenceFromEnv(),
			Logger:        logger,
		},
		Logger: logger,
	}

	out := declare.Run(context.Background(), deps, number, repoFlag)
	fmt.Println(out.Message)
	if !out.Confirmed {
		return 1, nil // message already printed; not an internal error
	}
	return 0, nil
}

// declareViaProxy sends the declaration to the declare.rein.internal
// virtual host through the sandbox's proxy (srt routes the CONNECT to
// rein's per-run socket; SSL_CERT_FILE already trusts rein's CA — both
// are set by the sandbox launch). Blocks while the human decides.
func declareViaProxy(number int, repoFlag string) (int, error) {
	u, err := url.Parse(declareHostURL)
	if err != nil {
		return 1, err
	}
	q := u.Query()
	q.Set("issue", strconv.Itoa(number))
	if repoFlag != "" {
		q.Set("repo", repoFlag)
	}
	u.RawQuery = q.Encode()

	client := &http.Client{
		Timeout: declareRequestTimeout,
		// srt exposes its proxy via the standard env vars in-sandbox;
		// ProxyFromEnvironment is what routes this CONNECT to rein's
		// socket. Outside any sandbox there is no route to the virtual
		// host and the request fails — handled below.
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
		// The endpoint never redirects; refuse to follow anything.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(u.String())
	if err != nil {
		return 1, fmt.Errorf("not inside a rein run (no REIN_RUN_ID and the declare endpoint is unreachable: %v). Launch your agent via `rein run -- <cmd>` and declare from within it", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var parsed struct {
		Confirmed int    `json:"confirmed"`
		Message   string `json:"message"`
	}
	_ = json.Unmarshal(body, &parsed)

	switch {
	case resp.StatusCode == http.StatusOK && parsed.Confirmed == number:
		if parsed.Message != "" {
			fmt.Println(parsed.Message)
		} else {
			fmt.Printf("issue #%d confirmed — writes are unlocked for this run (push to agent/%d/<nonce>)\n", number, number)
		}
		return 0, nil
	case parsed.Message != "":
		fmt.Fprintln(os.Stderr, parsed.Message)
		return 1, nil // the broker already explained why; not an internal error
	default:
		fmt.Fprintf(os.Stderr, "rein declare: denied (status %d)\n", resp.StatusCode)
		return 1, nil
	}
}
