// Package issuemeta fetches the metadata of a declared GitHub issue so
// the human can confirm it (issue #35 §4): title, state, whether the
// number is actually a pull request, and the canonical REST URL — the
// TM-G6 transfer anchor (design.md:753).
//
// # Fail-closed contract (§6)
//
// Every failure is a distinguishable error and the caller DENIES the
// declare: no prompt ever fires without a fetched title (decision E).
//
//   - 404/410            → ErrNotFound ("issue #N not found in o/r")
//   - any 3xx            → ErrTransferred (the canonical URL moved — the
//     issue was transferred; TM-G6 says deny + surface loudly, never
//     silently follow)
//   - network/5xx/limits → a plain error ("could not verify; retry")
//
// The HTTP client NEVER follows redirects: following one would silently
// re-anchor the confirmation to a different repo than the one displayed.
//
// # Display hygiene
//
// Titles are agent-editable in-scope (TM-G7) and are rendered onto the
// human's terminal by the Form A prompt, so Fetch sanitizes them: all C0
// control characters (CR/LF/ESC/NUL/backspace…) and DEL are replaced
// with a space — a hostile title cannot inject terminal escapes or forge
// extra prompt lines — and over-long titles are truncated. The title
// informs the human; it never authorizes (the number the human types is
// GitHub-assigned).
package issuemeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotFound: the issue does not exist in the declared repo (404/410).
// S4 (right number, wrong repo) resolves here structurally: the number is
// looked up ONLY against the declared/push-target repo.
var ErrNotFound = errors.New("issue not found in this repo")

// ErrTransferred: the canonical URL answered with a redirect — the issue
// was transferred to another repo (TM-G6). Deny + surface loudly; the
// human must re-declare against the issue's new home.
var ErrTransferred = errors.New("issue was transferred (canonical URL moved)")

// Meta is the fetched snapshot of a declared issue at confirm time.
type Meta struct {
	Number       int
	Repo         string // owner/name the fetch resolved against
	Title        string // sanitized for terminal display
	State        string // "open" | "closed"
	IsPR         bool   // GitHub shares the number space with PRs (§9)
	CanonicalURL string // canonical REST URL — the TM-G6 anchor
}

// maxTitleRunes bounds the displayed title. Truncation is display-only:
// the snapshot exists to inform the human, not to authorize.
const maxTitleRunes = 140

// fetchTimeout bounds one metadata GET when the caller's ctx has no
// earlier deadline.
const fetchTimeout = 10 * time.Second

// noRedirectClient never follows redirects (see the package doc).
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Fetch GETs /repos/{repo}/issues/{number} from apiBase (empty ⇒ the real
// api.github.com; tests point it at an httptest server, mirroring
// REIN_GITHUB_API_BASE elsewhere) using token (a read-tier token whose
// permission set includes issues:read — the MintGhReadOnlyToken shape;
// the plain read mint lacks issues:read and would 403).
func Fetch(ctx context.Context, apiBase, token, repo string, number int) (Meta, error) {
	if repo == "" || !strings.Contains(repo, "/") {
		return Meta{}, fmt.Errorf("issuemeta: repo %q is not owner/name", repo)
	}
	if number <= 0 {
		return Meta{}, fmt.Errorf("issuemeta: issue number %d is not positive", number)
	}
	base := strings.TrimSuffix(apiBase, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/issues/%d", base, repo, number)

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Meta{}, fmt.Errorf("issuemeta: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return Meta{}, fmt.Errorf("issuemeta: fetch issue #%d in %s: %w", number, repo, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		io.Copy(io.Discard, resp.Body)
		return Meta{}, fmt.Errorf("issue #%d in %s: %w", number, repo, ErrTransferred)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		io.Copy(io.Discard, resp.Body)
		return Meta{}, fmt.Errorf("issue #%d in %s: %w", number, repo, ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		io.Copy(io.Discard, resp.Body)
		return Meta{}, fmt.Errorf("issuemeta: fetch issue #%d in %s: unexpected status %d", number, repo, resp.StatusCode)
	}

	var body struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		State       string `json:"state"`
		URL         string `json:"url"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	// Cap the decode: a well-formed issue object is tiny; anything huge is
	// not what we asked for.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Meta{}, fmt.Errorf("issuemeta: parse issue #%d in %s: %w", number, repo, err)
	}
	if body.Number != number {
		// Defensive: a 200 for a different number would mis-anchor the
		// confirmation. Fail closed.
		return Meta{}, fmt.Errorf("issuemeta: response number %d != requested %d (refusing)", body.Number, number)
	}
	return Meta{
		Number:       number,
		Repo:         repo,
		Title:        SanitizeTitle(body.Title),
		State:        SanitizeTitle(body.State),
		IsPR:         body.PullRequest != nil,
		CanonicalURL: body.URL,
	}, nil
}

// CheckCanonical re-GETs a confirmed issue's canonical URL and reports
// whether it still answers 200 in place (TM-G6 re-check, §6: performed on
// each write-token mint). Any 3xx ⇒ ErrTransferred: the confirmation must
// be invalidated and the issue re-declared. Other failures return an
// error the caller treats as "could not verify" (fail closed for mints).
func CheckCanonical(ctx context.Context, token, canonicalURL string) error {
	if canonicalURL == "" {
		return errors.New("issuemeta: confirmed issue has no canonical URL")
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, canonicalURL, nil)
	if err != nil {
		return fmt.Errorf("issuemeta: build canonical re-check: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return fmt.Errorf("issuemeta: canonical re-check: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return ErrTransferred
	case resp.StatusCode == http.StatusOK:
		return nil
	default:
		return fmt.Errorf("issuemeta: canonical re-check: unexpected status %d", resp.StatusCode)
	}
}

// SanitizeTitle makes an agent-editable string safe for terminal display:
// every C0 control character and DEL becomes a space (no ANSI escapes, no
// forged prompt lines), and the result is truncated to maxTitleRunes.
func SanitizeTitle(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		out = append(out, r)
		if len(out) >= maxTitleRunes {
			out = append(out, '…')
			break
		}
	}
	return string(out)
}
