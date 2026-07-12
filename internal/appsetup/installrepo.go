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

// OfferInstallOnRepo PRINTS the install-on-repo step to w. It is print-only
// by design — it does NOT auto-open a browser.
//
// Why no auto-open: the App-creation step already auto-opens its own browser
// (runOneStep), and init only reaches THIS step on paths where the App is
// already installed (the env/state path resolves an installation id, and the
// fresh-manifest path is handled by printPostFlowSummary and skipped here).
// rein can't cheaply tell whether the App is installed on THIS session's
// specific repo without a network call, so auto-opening the install page on
// every local init would be noise. The printed link is the safe default; the
// user visits it if they still need to grant the install (no ssh -L needed —
// §5). A headless session gets an extra "open on any machine" hint.
//
// The message always contains the literal phrase "visit this URL" and a real
// link, so even a headless run surfaces something actionable.
func OfferInstallOnRepo(w io.Writer, configDir, repo string) {
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
	if detectHeadless().headless {
		fmt.Fprintln(w, "    (no local browser detected; open the link above on any machine — no ssh -L needed)")
	}
}
