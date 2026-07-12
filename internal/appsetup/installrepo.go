package appsetup

// Install-on-repo (onboarding-ux-design.md §5).
//
// After init scaffolds the session, it offers to install the App on the
// session's repo via a deep-link. Unlike App CREATION, installing needs no
// loopback callback — the user just visits a URL to grant the install — so
// there is NO `ssh -L` dance; any browser on any machine works. The printed
// link is the baseline; auto-open is a bonus when a local display exists
// (reusing rein's existing headless detection). This is the step that makes
// doctor's "install-id not cached" go away.

import (
	"fmt"
	"io"
	"strings"
)

// installGenericURL is the fallback when rein doesn't know the App's slug
// (the env-managed path records only client_id/installation_id, no slug/URL).
// The user picks their App from the installations list.
const installGenericURL = "https://github.com/settings/installations"

// InstallOnRepoURL resolves the best install deep-link rein can offer.
//
// When state.json carries a primary App record with a slug, the precise
// per-App deep-link is returned (<html_url>/installations/new, i.e.
// https://github.com/apps/<slug>/installations/new) and haveSlug is true.
// Otherwise the generic installations URL is returned and haveSlug is false
// — still a real, visitable link, just not App-specific.
//
// configDir is the caller's resolved config.ConfigDir(); passed in to keep
// this package decoupled from XDG resolution (same pattern as the hints/
// doctor helpers).
func InstallOnRepoURL(configDir string) (url string, haveSlug bool) {
	s, err := ReadState(configDir)
	if err != nil {
		return installGenericURL, false
	}
	if s.Primary == nil || s.Primary.Slug == "" {
		return installGenericURL, false
	}
	base := s.Primary.HTMLURL
	if base == "" {
		base = "https://github.com/apps/" + s.Primary.Slug
	}
	return strings.TrimRight(base, "/") + "/installations/new", true
}

// OfferInstallOnRepo prints the install-on-repo step to w and, when
// allowAutoOpen is set AND a local browser is available (not a headless SSH
// session), best-effort auto-opens the link. It NEVER blocks: on a headless
// session or when auto-open is disallowed it prints the link and returns.
//
// allowAutoOpen lets the caller suppress the browser launch when it would be
// noise — e.g. on the env-managed path the App is already installed, so init
// prints the link as an informational "install on more repos" pointer rather
// than popping a browser on every run.
//
// The message always contains the literal phrase "visit this URL" and a real
// link, so a headless run still surfaces something actionable.
func OfferInstallOnRepo(w io.Writer, configDir, repo string, allowAutoOpen bool) {
	url, haveSlug := InstallOnRepoURL(configDir)

	fmt.Fprintln(w, "  install on repo:")
	if repo != "" {
		fmt.Fprintf(w, "    to let the agent work on %s, install the App on it.\n", repo)
	} else {
		fmt.Fprintln(w, "    install the App on the repos you want it to broker tokens for.")
	}
	if !haveSlug {
		fmt.Fprintln(w, "    (env-managed App: pick it from your installations list)")
	}
	fmt.Fprintln(w, "    visit this URL in your browser:")
	fmt.Fprintf(w, "      %s\n", url)

	hi := detectHeadless()
	if hi.headless {
		fmt.Fprintln(w, "    (no local browser detected; open the link above on any machine — no ssh -L needed)")
		return
	}
	if !allowAutoOpen {
		return
	}
	fmt.Fprintln(w, "    opening your browser…")
	// openBrowser is best-effort and non-blocking (Start, not Wait); the URL
	// is already printed above so a launch failure is harmless.
	_ = openBrowser(url, w)
}
