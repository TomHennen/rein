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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
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

// declareArgs is one parsed `rein declare` invocation: either an
// EXISTING issue number, or (isNew) a proposed new issue.
type declareArgs struct {
	number int
	repo   string
	isNew  bool
	title  string // normalized by issuemeta.ValidateTitle
	body   string // normalized by issuemeta.ValidateBody
}

const declareUsage = "usage: rein declare <issue-number> [--repo owner/name]\n" +
	"       rein declare --new \"<title>\" [--body \"<text>\"] [--repo owner/name]"

// runDeclare is the `rein declare` entry point. args is os.Args[2:].
// Returns (exitCode, error) so the caller owns process exit — no
// os.Exit() inside, which would skip the deferred log close.
func runDeclare(args []string) (int, error) {
	a, err := parseDeclareArgs(args)
	if err != nil {
		return 2, err
	}
	if runID := os.Getenv("REIN_RUN_ID"); runID != "" {
		return declareDirect(a, runID)
	}
	return declareViaProxy(a)
}

// parseDeclareArgs validates both forms of `rein declare`. Title and body
// are validated HERE so a malformed proposal fails fast in the sandbox,
// and again broker-side (the sandbox is untrusted).
func parseDeclareArgs(args []string) (declareArgs, error) {
	var a declareArgs
	var numArg, rawTitle, rawBody string
	sawBody := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func(flag string) (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("rein declare: %s needs a value\n%s", flag, declareUsage)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case arg == "--repo":
			if a.repo, err = next("--repo"); err != nil {
				return declareArgs{}, err
			}
		case strings.HasPrefix(arg, "--repo="):
			a.repo = strings.TrimPrefix(arg, "--repo=")
		case arg == "--new":
			a.isNew = true
			if rawTitle, err = next("--new"); err != nil {
				return declareArgs{}, err
			}
		case strings.HasPrefix(arg, "--new="):
			a.isNew = true
			rawTitle = strings.TrimPrefix(arg, "--new=")
		case arg == "--body":
			sawBody = true
			if rawBody, err = next("--body"); err != nil {
				return declareArgs{}, err
			}
		case strings.HasPrefix(arg, "--body="):
			sawBody = true
			rawBody = strings.TrimPrefix(arg, "--body=")
		case strings.HasPrefix(arg, "-"):
			return declareArgs{}, fmt.Errorf("rein declare: unknown flag %q\n%s", arg, declareUsage)
		case numArg == "":
			numArg = arg
		default:
			return declareArgs{}, fmt.Errorf("rein declare: unexpected argument %q", arg)
		}
	}

	if a.isNew {
		if numArg != "" {
			return declareArgs{}, fmt.Errorf("rein declare: --new files a NEW issue and takes no issue number (got %q)", numArg)
		}
		title, err := issuemeta.ValidateTitle(rawTitle)
		if err != nil {
			return declareArgs{}, fmt.Errorf("rein declare --new: %w", err)
		}
		body, err := issuemeta.ValidateBody(rawBody)
		if err != nil {
			return declareArgs{}, fmt.Errorf("rein declare --new: %w", err)
		}
		a.title, a.body = title, body
		return a, nil
	}

	if sawBody {
		return declareArgs{}, fmt.Errorf("rein declare: --body only applies to --new\n%s", declareUsage)
	}
	if numArg == "" {
		return declareArgs{}, errors.New(declareUsage)
	}
	if !issueArgPattern.MatchString(numArg) {
		return declareArgs{}, fmt.Errorf("rein declare: %q is not a valid issue number (positive decimal, no leading zeros)", numArg)
	}
	n, err := strconv.Atoi(numArg)
	if err != nil {
		return declareArgs{}, fmt.Errorf("rein declare: parse %q: %w", numArg, err)
	}
	a.number = n
	return a, nil
}

// declareDirect runs the declaration fully in-process (direct mode).
func declareDirect(a declareArgs, runID string) (int, error) {
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
	logger.Printf("declare (direct): issue=%d new=%t repo=%q run=%s session=%s source=%s", a.number, a.isNew, a.repo, runID, sess.ID, sessSource)

	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		return 1, fmt.Errorf("resolve App config: %w (run `rein init` / `rein doctor`)", err)
	}
	appCfg.RepoNames = sess.BareRepoNames()

	appName, installURL := appInstallHints(appCfg)
	sessionFile := session.SourceFilePath(sessSource)
	oldSig := approvals.SignatureOf(sess)

	gcfg := grant.Config{
		TTL:         approvalTTL,
		PreferPopup: grant.PopupPreferenceFromEnv(),
		StateDir:    stateDir,
		RunID:       runID,
		RunPID:      envInt("REIN_RUN_PID"),
		SessionFile: sessionFile,
		// Direct mode: the credential helper reloads the session file every
		// git op, so an in-prompt persist must re-sign this run's approval
		// (below, and — for the out-of-process grant surface — via the
		// snapshot's Direct flag). Sandboxed mode leaves this false.
		Direct: true,
		// DIRECT-MODE ONLY (issue #69): the credential helper re-loads the
		// session file on EVERY git operation, so persisting the approved
		// repo would change the session signature under the live run and
		// invalidate the approval the human just gave — re-locking their own
		// run. Re-sign the record for the session rein itself just wrote.
		// (Sandboxed mode holds its launch session in-process and needs
		// nothing here; a HAND-edited yaml still invalidates, as designed.)
		OnPersist: func(newSess session.Session) {
			if err := approvals.Resign(stateDir, runID, oldSig, approvals.SignatureOf(newSess), newSess.ID); err != nil {
				logger.Printf("declare: re-sign of the approval record after persist failed: %v", err)
				fmt.Fprintln(os.Stderr, "rein: WARNING: the repo was saved, but this run's approval could not be re-keyed to the wider session.")
				fmt.Fprintln(os.Stderr, "      Re-run `rein declare <n>` if writes stop working.")
				return
			}
			logger.Printf("declare: approval record re-signed for the persisted session (%v)", newSess.Repos)
		},
		Logger: logger,
	}

	deps := declare.Deps{
		StateDir:   stateDir,
		RunID:      runID,
		RunPID:     envInt("REIN_RUN_PID"),
		Session:    sess,
		AppName:    appName,
		InstallURL: installURL,
		Fetch: func(ctx context.Context, repo string, n int) (issuemeta.Meta, error) {
			// Scope the fetch token to the session PLUS the requested repo:
			// a scope expansion targets a repo outside the standing ceiling,
			// and a token that doesn't cover it 404s on the issue (which
			// would look like "issue not found" instead of "not in scope").
			// See declare.Deps.Fetch's security note.
			cfg := appCfg
			cfg.RepoNames = sess.BareRepoNames()
			if !sess.Contains(repo) {
				cfg.RepoNames = append(cfg.RepoNames, bareRepoName(repo))
			}
			client, err := githubapp.NewClient(cfg, ks, config.AppKeystoreRole)
			if err != nil {
				return issuemeta.Meta{}, err
			}
			mctx, cancel := context.WithTimeout(ctx, mintTimeout)
			token, _, err := client.MintGhReadOnlyToken(mctx)
			cancel()
			if err != nil {
				return issuemeta.Meta{}, fmt.Errorf("mint read token for issue fetch: %w", err)
			}
			if !sess.Contains(repo) {
				// Not cached anywhere, and revoked right after use: a DENIED
				// expansion leaves no credential covering the candidate repo.
				defer func() {
					rctx, rcancel := context.WithTimeout(context.Background(), mintTimeout)
					defer rcancel()
					if rerr := client.RevokeToken(rctx, token); rerr != nil {
						logger.Printf("declare: revoke of the candidate-scoped read token failed: %v", rerr)
					}
				}()
			}
			return issuemeta.Fetch(ctx, os.Getenv("REIN_GITHUB_API_BASE"), token, repo, n)
		},
		ProbeInstall: func(ctx context.Context, repo string) error {
			owner, name, _ := strings.Cut(repo, "/")
			_, err := fetchRepoInstallationID(ctx, appCfg.ClientID, ks, config.AppKeystoreRole, owner, name)
			return err
		},
		Notice: func(ctx context.Context, n declare.Notice) {
			grant.ShowInstallNotice(ctx, gcfg, grant.InstallNotice{
				Repo: n.Repo, Issue: n.Issue, InstallURL: n.InstallURL, AppName: n.AppName,
			})
		},
		CreateIssue: createIssueFunc(func() githubapp.Config { return appCfg }, ks, session.OwnerOf(sess), logger),
		Grant:       gcfg,
		Logger:      logger,
	}

	ctx := context.Background()
	var out declare.Outcome
	if a.isNew {
		out = declare.RunNew(ctx, deps, declare.NewIssue{Repo: a.repo, Title: a.title, Body: a.body})
	} else {
		out = declare.Run(ctx, deps, a.number, a.repo)
	}
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
func declareViaProxy(a declareArgs) (int, error) {
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
	var resp *http.Response
	var err error
	if a.isNew {
		payload, merr := json.Marshal(struct {
			Title string `json:"title"`
			Body  string `json:"body,omitempty"`
			Repo  string `json:"repo,omitempty"`
		}{a.title, a.body, a.repo})
		if merr != nil {
			return 1, merr
		}
		resp, err = client.Post(declareHostURL+"/new", "application/json", bytes.NewReader(payload))
	} else {
		u, perr := url.Parse(declareHostURL)
		if perr != nil {
			return 1, perr
		}
		q := u.Query()
		q.Set("issue", strconv.Itoa(a.number))
		if a.repo != "" {
			q.Set("repo", a.repo)
		}
		u.RawQuery = q.Encode()
		resp, err = client.Get(u.String())
	}
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

	// A NEW issue's number is assigned by GitHub, so success is "the broker
	// confirmed SOME issue"; a declared number must come back unchanged.
	confirmed := parsed.Confirmed > 0
	if !a.isNew {
		confirmed = parsed.Confirmed == a.number
	}
	switch {
	case resp.StatusCode == http.StatusOK && confirmed:
		if parsed.Message != "" {
			fmt.Println(parsed.Message)
		} else {
			fmt.Printf("issue #%d confirmed — writes are unlocked for this run (push to agent/%d/<nonce>)\n", parsed.Confirmed, parsed.Confirmed)
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
