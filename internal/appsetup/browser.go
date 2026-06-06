package appsetup

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// browserOpenerOverride lets tests inject a fake opener that records
// the URL and returns immediately. Production sets this to nil and
// uses the platform default.
var browserOpenerOverride func(url string) error

// headlessInfo describes whether this looks like a remote/headless
// session with no local browser that could reach the loopback callback,
// plus the best-guess ssh target to suggest for port-forwarding.
type headlessInfo struct {
	headless bool
	sshUser  string // best-effort; "" if unknown
	sshHost  string // best-effort; "" if unknown
}

// detectHeadless reports whether the loopback callback is unlikely to be
// reachable by a browser on this machine — i.e. we're in an SSH session
// with no local display. It is best-effort and only drives an additive
// hint; the callback URL is always printed regardless.
//
// Tests that inject browserOpenerOverride are never treated as headless,
// so the existing flow tests keep exercising the auto-open path.
func detectHeadless() headlessInfo {
	if browserOpenerOverride != nil {
		return headlessInfo{}
	}
	ssh := os.Getenv("SSH_CONNECTION")
	hasDisplay := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	if ssh == "" || hasDisplay {
		// Local, or SSH with X/Wayland forwarding where xdg-open can work.
		return headlessInfo{}
	}
	info := headlessInfo{headless: true}
	// SSH_CONNECTION = "client_ip client_port server_ip server_port";
	// server_ip is the address the user most likely ssh's back to.
	if f := strings.Fields(ssh); len(f) >= 3 {
		info.sshHost = f[2]
	}
	info.sshUser = os.Getenv("USER")
	return info
}

// printBrowserInstructions tells the user how to reach the callback URL.
// On a normal desktop it prints the URL (the caller then tries to
// auto-open it). On a detected headless/remote session it prints a
// ready-to-paste `ssh -L` port-forward recipe instead, since "open this
// URL" is useless when no local browser can reach 127.0.0.1.
func printBrowserInstructions(w io.Writer, port int, pinnedPort bool, hi headlessInfo) {
	url := localURL(port)
	if !hi.headless {
		fmt.Fprintf(w, "  open this URL in your browser if it doesn't open automatically:\n    %s\n", url)
		return
	}
	target := "<user>@<host>"
	switch {
	case hi.sshHost != "" && hi.sshUser != "":
		target = hi.sshUser + "@" + hi.sshHost
	case hi.sshHost != "":
		target = hi.sshHost
	}
	fmt.Fprintln(w, "  No local browser detected (headless SSH session).")
	fmt.Fprintln(w, "  Complete setup by forwarding the callback port to a machine with a browser:")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "    1. On your laptop, run:\n         ssh -L %d:127.0.0.1:%d %s\n", port, port, target)
	fmt.Fprintf(w, "    2. Then open this in your laptop browser:\n         %s\n", url)
	fmt.Fprintln(w)
	if !pinnedPort {
		fmt.Fprintf(w, "  Tip: pin the port with `rein init --port %d` so you can set up the\n", port)
		fmt.Fprintln(w, "  tunnel before running init (avoids the see-port-then-tunnel race).")
	}
	fmt.Fprintln(w, "  If port-forwarding is blocked on your network, see the headless")
	fmt.Fprintln(w, "  fallback in docs/init-manifest-design.md.")
}

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
