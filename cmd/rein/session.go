// `rein session show | add-repo <owner/name>` — the human-side session
// commands (issue #69, mocks §2). The session YAML stays the standing scope
// ceiling, but it stops being hand-edited: these commands VALIDATE AT WRITE
// TIME (same-owner rule + install-coverage probe with the deep-link) instead
// of letting a typo surface as a mid-run 404 inside the agent (#53/#59).
//
// These run OUTSIDE the sandbox and are the HUMAN's path. The sandboxed
// agent cannot reach them: `session` never rides the declare virtual host —
// the agent's only way to widen scope is `rein declare --repo`, which always
// goes through the human's approval prompt. (`remove-repo` is deliberately
// not built: deferred, mocks §5.4.)
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/runscope"
	"github.com/TomHennen/rein/internal/session"
)

// runSession dispatches `rein session <sub>`. args is os.Args[2:].
func runSession(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rein session show | rein session add-repo <owner/name>")
		os.Exit(2)
	}
	switch args[0] {
	case "show":
		return sessionShow()
	case "add-repo":
		if len(args) < 2 {
			return errors.New("usage: rein session add-repo <owner/name>")
		}
		return sessionAddRepo(args[1])
	default:
		fmt.Fprintf(os.Stderr, "rein session: unknown subcommand %q (want show|add-repo)\n", args[0])
		os.Exit(2)
	}
	return nil
}

// sessionShow answers "what can the agent touch right now?" — the STANDING
// ceiling (the yaml) and the LIVE runs' deltas (confirmed issues and this
// run's scope expansions) in one place (mocks §2.2).
func sessionShow() error {
	sess, source, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	path := session.SourceFilePath(source)
	if path == "" {
		fmt.Printf("session: %s   (no session file — env fallback; `rein init` writes one)\n", sess.ID)
	} else {
		fmt.Printf("session: %s   (%s)\n", sess.ID, path)
	}
	fmt.Printf("  role:      %s\n", sess.Role)

	// Install coverage per repo: the #53 failure class, made visible BEFORE
	// launch rather than as a mid-run 404 inside the agent. Best-effort — a
	// probe outage annotates, it does not fail the command.
	covered := probeCoverage(sess.Repos)
	for i, r := range sess.Repos {
		label := "repos:    "
		if i > 0 {
			label = "          "
		}
		fmt.Printf("  %s %-34s %s\n", label, r, covered[r])
	}
	fmt.Println("  issue:     agent-declared at runtime (confirmed per run; #35 model)")
	if len(sess.AllowDomains) > 0 {
		fmt.Printf("  egress:    +%s   (allow_domains)\n", strings.Join(sess.AllowDomains, ", +"))
	}
	if !sess.Created.IsZero() {
		fmt.Printf("  created:   %s  (no TTL enforced in Phase 1)\n", sess.Created.UTC().Format("2006-01-02 15:04 UTC"))
	}
	sess.WarnIgnoredIssue(os.Stdout)

	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	return showLiveRuns(stateDir, sess)
}

// showLiveRuns prints the per-run deltas: confirmed issues, and the repos
// that joined a run's ceiling through an approved scope expansion (which
// live ONLY in the approval record — the yaml above is unchanged unless the
// human persisted them).
func showLiveRuns(stateDir string, sess session.Session) error {
	list, err := approvals.List(stateDir)
	if err != nil {
		return nil // status is a nicety; never fail `show` on it
	}
	var live []approvals.RunStatus
	for _, st := range list {
		if st.Live {
			live = append(live, st)
		}
	}
	if len(live) == 0 {
		fmt.Println("\nlive runs: none")
		return nil
	}
	fmt.Println("\nlive runs:")
	for _, st := range live {
		age := ""
		if !st.Context.WrittenAt.IsZero() {
			age = fmt.Sprintf("  started %s ago", time.Since(st.Context.WrittenAt).Round(time.Minute))
		}
		fmt.Printf("  run %s…%s\n", truncID(st.RunID), age)
		// The run's OWN session (from its snapshot) is the ceiling it
		// launched with — not necessarily the one on disk now.
		runSess := st.Context.Session
		if runSess.ID == "" {
			runSess = sess
		}
		rs := runscope.New(runSess, stateDir, st.RunID)
		issues := st.Approval.Issues
		if len(issues) == 0 {
			fmt.Println("    confirmed: none (writes LOCKED until the agent declares an issue)")
		}
		for _, ci := range issues {
			fmt.Printf("    confirmed: #%d %q in %s\n", ci.Number, ci.Title, ci.Repo)
		}
		if exp := rs.Expansions(); len(exp) > 0 {
			for _, r := range exp {
				fmt.Printf("    expansion: %s (approved for THIS RUN", r)
				if sess.Contains(r) {
					fmt.Print("; also now in the session file")
				}
				fmt.Println(")")
			}
		}
	}
	return nil
}

func truncID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sessionAddRepo widens the STANDING ceiling: validate (owner + install
// coverage), then write. Nothing is written unless both checks pass — a
// durable widening gets the strict rule, not the launch path's
// warn-and-continue (mocks §2.1).
func sessionAddRepo(repo string) error {
	sess, source, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	path := session.SourceFilePath(source)
	if path == "" {
		return fmt.Errorf("this session has no file to add to (it came from the env fallback).\n      Run `rein init` to write a session file first")
	}

	// (1) Structural: owner/name shape + the single-owner rule. No network.
	norm, err := session.CheckAddRepo(sess, repo)
	if errors.Is(err, session.ErrRepoAlreadyInSession) {
		fmt.Printf("rein: %s is already in the session. Nothing to do.\n", norm)
		return nil
	}
	if err != nil {
		return fmt.Errorf("rein: %w", err)
	}

	fmt.Printf("rein: checking %s...\n", norm)
	fmt.Printf("  owner:    %s — matches session owner            OK\n", session.OwnerOf(sess))

	// (2) Install coverage. A definitive 404 refuses the add with the
	// deep-link; a transient error refuses it too ("could not verify") —
	// this is a durable widening, so it fails closed either way. NOTHING is
	// written in either case.
	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		return fmt.Errorf("resolve App config: %w (run `rein init` / `rein doctor`)", err)
	}
	appName, installURL := appInstallHints(appCfg)
	owner, name, _ := strings.Cut(norm, "/")
	ctx, cancel := context.WithTimeout(context.Background(), installIDTimeout)
	_, perr := fetchRepoInstallationID(ctx, appCfg.ClientID, ks, config.AppKeystoreRole, owner, name)
	cancel()
	switch {
	case errors.Is(perr, githubapp.ErrAppNotInstalled):
		app := appName
		if app == "" {
			app = "rein's GitHub App"
		}
		return fmt.Errorf("rein: App %s is not installed on %s.\n      Install it at %s\n      then re-run this command. (Nothing was changed.)", app, norm, installURL)
	case perr != nil:
		return fmt.Errorf("rein: could not verify that the App covers %s (%v); retry. (Nothing was changed.)", norm, perr)
	}
	app := appName
	if app == "" {
		app = "the App"
	}
	fmt.Printf("  install:  %s covers it  OK\n", app)

	// (3) Write.
	updated, err := session.AddRepoToFile(path, norm)
	if errors.Is(err, session.ErrRepoAlreadyInSession) {
		fmt.Printf("rein: %s is already in the session. Nothing to do.\n", norm)
		return nil
	}
	if err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	fmt.Println("rein: added. Session repos are now:")
	for _, r := range updated.Repos {
		fmt.Printf("  - %s\n", r)
	}
	fmt.Println("Takes effect on the NEXT `rein run`. A live run keeps its launch-time scope —")
	fmt.Println("inside a run, the agent requests expansion with `rein declare <n> --repo <owner/name>`.")
	return nil
}

// probeCoverage annotates each repo with its install-coverage status for
// `session show`. Best-effort and sequential (sessions hold a handful of
// repos); an unresolvable App yields a single honest "unknown" for all.
func probeCoverage(repos []string) map[string]string {
	out := make(map[string]string, len(repos))
	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		for _, r := range repos {
			out[r] = "[App not configured]"
		}
		return out
	}
	for _, r := range repos {
		owner, name, ok := strings.Cut(r, "/")
		if !ok {
			out[r] = "[not owner/name]"
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), installIDTimeout)
		_, perr := fetchRepoInstallationID(ctx, appCfg.ClientID, ks, config.AppKeystoreRole, owner, name)
		cancel()
		switch {
		case perr == nil:
			out[r] = "[App installed]"
		case errors.Is(perr, githubapp.ErrAppNotInstalled):
			out[r] = "[App NOT installed — the agent will fail on it]"
		default:
			out[r] = "[coverage unknown]"
		}
	}
	return out
}
