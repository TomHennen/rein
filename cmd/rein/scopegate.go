// Launch-time scope gate: a working tree whose repo is outside the session
// ceiling no longer launches with a warning (a run that can edit but never push —
// Tom, 2026-08-30). Offer to add the repo on a tty; otherwise fail closed with
// the remedies.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/TomHennen/rein/internal/session"
)

// EnvAllowUnscopedWorktree opts one launch out of the scope gate (edit-only use).
const EnvAllowUnscopedWorktree = "REIN_ALLOW_UNSCOPED_WORKTREE"

// ensureWorkTreeInScope gates the launch when dir is a checkout of a repo not in
// the session. On acceptance the repo is added (owner + install-coverage
// validated) and *sess updated so THIS run can push it. confirm is injected for
// tests; production passes ttyYesNo. A non-checkout dir passes untouched (the
// ephemeral-tree flows are legitimate).
func ensureWorkTreeInScope(sess *session.Session, sessSource, dir string, confirm func(string) bool) error {
	repo := detectRepoFromGit(dir)
	if repo == "" || sess.Contains(repo) {
		return nil
	}
	// Loud opt-out for the edit-only case (no push wanted, or the App cannot
	// cover the repo). Operator env, set before launch — not agent-reachable.
	if os.Getenv(EnvAllowUnscopedWorktree) != "" {
		fmt.Fprintf(os.Stderr, "rein: WARNING: %s=1 — working tree repo %s is outside the session scope; the agent can edit it but NEVER push it.\n",
			EnvAllowUnscopedWorktree, repo)
		return nil
	}
	fmt.Fprintf(os.Stderr, "rein: working tree %s is a checkout of %s, which is NOT in this session's scope (%s).\n",
		dir, repo, strings.Join(sess.Repos, ", "))
	if confirm(fmt.Sprintf("rein: add %s to the session now? [y/N] ", repo)) {
		updated, err := addRepoValidated(*sess, sessSource, repo)
		if err != nil {
			return fmt.Errorf("add %s to the session: %w", repo, err)
		}
		*sess = updated
		fmt.Fprintf(os.Stderr, "rein: added — this run can push %s.\n", repo)
		return nil
	}
	return fmt.Errorf("refusing to launch: the agent could edit %s but never push it.\n"+
		"      Add it to the session first:  rein session add-repo %s\n"+
		"      (or re-run on a terminal and answer y at the prompt)", repo, repo)
}

// ttyYesNo asks on /dev/tty; no tty (or any read error) is a No — fail closed.
func ttyYesNo(question string) bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer tty.Close()
	fmt.Fprint(tty, question)
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}
