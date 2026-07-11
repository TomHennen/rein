package grant

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// InstallNotice is an interactive message shown on the human's approval
// surface that carries NO APPROVAL AUTHORITY (issue #69, the 404-at-
// expansion case).
//
// # Why a notice and not a prompt
//
// The agent asked for a repo the GitHub App is not installed on. There is
// nothing approvable: no ceremony the human performs can make rein mint a
// token for a repo outside its installation. Firing the Form A prompt
// anyway — and leaving it open for the minutes a browser install takes —
// would train the habit this whole design exists to prevent: answering a
// long-lived prompt without re-reading it. So the human gets the deep-link
// and an acknowledgement, the declare is REFUSED, and the real
// scope-expansion approval fires FRESH when the agent retries.
//
// Concretely, this type can only ever cause a run-context write and some
// terminal output. It never touches the approval record.
type InstallNotice struct {
	// Repo is the repo the App does not cover.
	Repo string

	// Issue is the number the agent declared (context only — it was never
	// fetched; no credential can read an issue in an uncovered repo).
	Issue int

	// InstallURL is the install deep-link.
	InstallURL string

	// AppName names the App, when known.
	AppName string
}

// ShowInstallNotice displays the notice on the best available surface and
// BLOCKS until the human acknowledges (enter = "installed, I'm done";
// anything else = skip). The return value is deliberately VOID of meaning
// for authorization: the caller refuses the declare either way.
//
// Surfaces, in the same order as the approval prompt:
//
//  1. tmux popup (when PreferPopup — the agent TUI owns the terminal), via
//     `rein approval notice --run-id X`, which renders from the run-context
//     snapshot written here;
//  2. the inline /dev/tty;
//  3. plain stderr on the run terminal (the mocks' one-line fallback, §1.4)
//     — non-interactive, but the human still sees the link.
func ShowInstallNotice(ctx context.Context, cfg Config, n InstallNotice) {
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Prompter == nil {
		cfg.Prompter = prompt.TTYPrompter{}
	}
	if cfg.TmuxRunner == nil {
		cfg.TmuxRunner = DefaultTmuxRunner
	}

	// Persist the notice for the out-of-process surface, exactly as a
	// declare persists its PendingIssue: the popup renders from disk and
	// never fetches. Best-effort — a snapshot failure only costs us the
	// popup, not the notice.
	if cfg.RunID != "" {
		if rc, err := approvals.ReadRunContext(cfg.StateDir, cfg.RunID); err == nil {
			rc.PendingNotice = &approvals.PendingNotice{
				Repo:       n.Repo,
				Issue:      n.Issue,
				InstallURL: n.InstallURL,
				AppName:    n.AppName,
				WrittenAt:  time.Now(),
			}
			if err := approvals.WriteRunContext(cfg.StateDir, cfg.RunID, rc); err != nil {
				cfg.Logger.Printf("notice: run-context snapshot write failed: %v", err)
			}
		}
	}

	if cfg.PreferPopup && os.Getenv("TMUX") != "" && cfg.RunID != "" {
		reinCmd := resolveReinCmd()
		ctxPopup, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "notice", "--run-id", cfg.RunID}); err == nil {
			cfg.Logger.Printf("notice: shown via tmux popup for %s", n.Repo)
			return
		} else {
			cfg.Logger.Printf("notice: tmux popup failed (%v); falling back", err)
		}
	}

	if AcknowledgeInstallNotice(ctx, cfg, n) == nil {
		return
	}
	// No interactive surface: print the run-terminal line (mocks §1.4) so
	// the human still sees the link even if the agent buries its own copy.
	WriteInstallNotice(cfg.Stderr, n)
	fmt.Fprintln(cfg.Stderr, "      (the agent will retry after that)")
}

// AcknowledgeInstallNotice renders the notice on /dev/tty and waits for the
// human. Returns an error if there is no tty (the caller falls back).
//
// It is also the body of the `rein approval notice` subcommand: inside a
// tmux popup, the popup's pty IS the /dev/tty, so the same code runs there.
func AcknowledgeInstallNotice(ctx context.Context, cfg Config, n InstallNotice) error {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%w: %v", prompt.ErrNoTTY, err)
	}
	defer tty.Close()

	WriteInstallNotice(tty, n)
	fmt.Fprint(tty, "\nPress ENTER when the install is done (the agent will retry), or 's' to skip.\n> ")
	// The read is a pure acknowledgement: NOTHING is granted by any answer,
	// so unlike Form A there is no token to match and no denial to record.
	line, rerr := prompt.ReadLine(ctx, tty)
	if rerr != nil {
		fmt.Fprintln(tty, "\n  [notice dismissed]")
		return nil
	}
	_ = line
	fmt.Fprintln(tty, "  [acknowledged — the agent must re-run its declare; you will be asked to approve the repo then]")
	return nil
}

// WriteInstallNotice renders the notice block (no input).
func WriteInstallNotice(w io.Writer, n InstallNotice) {
	app := n.AppName
	if app == "" {
		app = "rein's GitHub App"
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "=== rein: NOTICE — App not installed (nothing to approve) ===")
	fmt.Fprintf(w, "   the agent asked for:  %s", n.Repo)
	if n.Issue > 0 {
		fmt.Fprintf(w, "  (for issue #%d)", n.Issue)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "   but the GitHub App %s is NOT installed on it,\n", app)
	fmt.Fprintln(w, "   so rein cannot mint any credential for it — there is no approval to give.")
	if n.InstallURL != "" {
		fmt.Fprintf(w, "   Install it here:  %s\n", n.InstallURL)
	}
	fmt.Fprintln(w, "   Then the agent re-runs its declare, and you will be asked to APPROVE the repo.")
}

// NoticeFromRunContext loads a pending notice snapshot for the
// `rein approval notice --run-id X` subcommand (the popup surface).
func NoticeFromRunContext(stateDir, runID string) (InstallNotice, error) {
	rc, err := approvals.ReadRunContext(stateDir, runID)
	if err != nil {
		return InstallNotice{}, err
	}
	if rc.PendingNotice == nil {
		return InstallNotice{}, fmt.Errorf("run %s has no pending notice", runID)
	}
	return InstallNotice{
		Repo:       rc.PendingNotice.Repo,
		Issue:      rc.PendingNotice.Issue,
		InstallURL: rc.PendingNotice.InstallURL,
		AppName:    rc.PendingNotice.AppName,
	}, nil
}
