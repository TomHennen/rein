package appsetup

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

// browserOpenerOverride lets tests inject a fake opener that records
// the URL and returns immediately. Production sets this to nil and
// uses the platform default.
var browserOpenerOverride func(url string) error

// openBrowser opens url in the user's default browser, best-effort.
// On Linux: xdg-open. On macOS: open. On other platforms or when the
// helper is absent, prints a fallback line on w and returns nil.
// Never blocks the caller — the URL is already printed to w by the
// caller BEFORE this is called, so even if launch hangs or fails
// silently the user has the URL.
func openBrowser(url string, w io.Writer) error {
	if browserOpenerOverride != nil {
		return browserOpenerOverride(url)
	}
	var bin string
	switch runtime.GOOS {
	case "linux":
		bin = "xdg-open"
	case "darwin":
		bin = "open"
	default:
		fmt.Fprintln(w, "  (no auto-open helper for this platform; please open the URL above manually)")
		return nil
	}
	if _, err := exec.LookPath(bin); err != nil {
		fmt.Fprintf(w, "  (%s not found on PATH; please open the URL above manually)\n", bin)
		return nil
	}
	// Detach: don't wait for the browser process. xdg-open / open
	// typically exec into the registered handler, but even when they
	// don't, returning quickly keeps the caller's progress flow tight.
	cmd := exec.Command(bin, url)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "  (%s failed to launch: %v; please open the URL above manually)\n", bin, err)
		return nil
	}
	// Release the process so it isn't reparented to the wrapped agent.
	go func() { _ = cmd.Wait() }()
	return nil
}
