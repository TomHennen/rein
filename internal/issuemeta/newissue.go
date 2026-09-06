// newissue.go — filing a NEW issue on the agent's behalf (issue #180).
//
// The agent supplies the title and body, so both are validated here
// (fail closed, never sanitized-and-accepted: a title carrying an escape
// sequence is a refusal, not something to quietly rewrite) and the
// human's Form A confirmation token is derived from the title's FIRST
// WORD — FirstWord is the single authority for it, so validation and the
// prompt can never disagree about what counts as a word.

package issuemeta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
)

// Bounds on the agent-supplied fields. Checked in the CLI and RE-checked
// host-side (the sandbox is untrusted).
const (
	MaxTitleChars = 200
	MaxBodyChars  = 4000

	// bodyExcerptRunes bounds what the human sees of the proposed body.
	bodyExcerptRunes = 200
)

// ValidateTitle normalizes and checks a proposed issue title: control
// characters and unicode format runes are refused outright, surrounding
// and repeated whitespace collapses, and the result must be 1..200 chars
// containing at least one word (FirstWord != "").
func ValidateTitle(raw string) (string, error) {
	if err := rejectUnsafe(raw, false); err != nil {
		return "", fmt.Errorf("title: %w", err)
	}
	t := strings.Join(strings.Fields(raw), " ")
	if t == "" {
		return "", fmt.Errorf("title: must not be empty")
	}
	if n := len([]rune(t)); n > MaxTitleChars {
		return "", fmt.Errorf("title: %d characters exceeds the %d limit", n, MaxTitleChars)
	}
	if FirstWord(t) == "" {
		return "", fmt.Errorf("title: must contain at least one word (letters or digits)")
	}
	return t, nil
}

// ValidateBody normalizes and checks a proposed issue body. Newlines and
// tabs survive (a body is prose); every other control character, DEL, and
// unicode format rune is refused. Empty is valid — the body is optional.
func ValidateBody(raw string) (string, error) {
	b := strings.ReplaceAll(raw, "\r\n", "\n")
	if err := rejectUnsafe(b, true); err != nil {
		return "", fmt.Errorf("body: %w", err)
	}
	if n := len([]rune(b)); n > MaxBodyChars {
		return "", fmt.Errorf("body: %d characters exceeds the %d limit", n, MaxBodyChars)
	}
	return b, nil
}

// rejectUnsafe fails on any rune that could forge terminal output or hide
// text. multiline allows \n and \t; otherwise only \t (which the caller
// collapses to a space).
func rejectUnsafe(s string, multiline bool) error {
	for _, r := range s {
		switch {
		case r == '\t':
		case r == '\n' && multiline:
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("control character %q is not allowed", r)
		case unicode.Is(unicode.Cf, r):
			return fmt.Errorf("format character %q (bidi/zero-width) is not allowed", r)
		}
	}
	return nil
}

// FirstWord returns the title's first word — the token the human types to
// approve filing it. Leading punctuation is stripped, so "[bug] crash"
// and "Fix: the thing" both yield a word the human can actually type
// ("bug", "Fix"). Returns "" when the title has no letters or digits at
// all, which is exactly the case ValidateTitle refuses.
func FirstWord(title string) string {
	for _, field := range strings.Fields(title) {
		w := strings.TrimFunc(field, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if w != "" {
			return w
		}
	}
	return ""
}

// BodyExcerpt renders a proposed body for the human's prompt: sanitized
// like a title (no escapes, no bidi), all whitespace collapsed to single
// spaces so it cannot forge prompt lines, and truncated with a marker.
func BodyExcerpt(body string) string {
	flat := strings.Join(strings.Fields(SanitizeTitle(body)), " ")
	runes := []rune(flat)
	if len(runes) <= bodyExcerptRunes {
		return flat
	}
	return string(runes[:bodyExcerptRunes]) + "… (truncated)"
}

// Create files a new issue via POST /repos/{repo}/issues and returns the
// created issue's Meta — including CanonicalURL, the TM-G6 transfer
// anchor the per-write-mint re-check needs.
//
// token must carry issues:write (the MintGhSessionToken shape; the plain
// write mint is contents-only and 403s here). It never leaves the broker.
func Create(ctx context.Context, apiBase, token, repo, title, body string) (Meta, error) {
	if repo == "" || !strings.Contains(repo, "/") {
		return Meta{}, fmt.Errorf("issuemeta: repo %q is not owner/name", repo)
	}
	base := strings.TrimSuffix(apiBase, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body,omitempty"`
	}{title, body})
	if err != nil {
		return Meta{}, fmt.Errorf("issuemeta: marshal new issue: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/repos/"+repo+"/issues", bytes.NewReader(payload))
	if err != nil {
		return Meta{}, fmt.Errorf("issuemeta: build create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return Meta{}, fmt.Errorf("issuemeta: create issue in %s: %w", repo, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusCreated {
		return Meta{}, fmt.Errorf("issuemeta: create issue in %s: status %d (%s)", repo, resp.StatusCode, apiErrorMessage(raw))
	}
	var out struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Meta{}, fmt.Errorf("issuemeta: parse created issue in %s: %w", repo, err)
	}
	if out.Number <= 0 {
		return Meta{}, fmt.Errorf("issuemeta: create issue in %s: response carried no issue number", repo)
	}
	canonical := out.URL
	if canonical == "" {
		// The TM-G6 re-check GETs this on every write mint; an empty anchor
		// would silently exempt the issue rein just filed.
		canonical = fmt.Sprintf("%s/repos/%s/issues/%d", base, repo, out.Number)
	}
	state := out.State
	if state == "" {
		state = "open"
	}
	return Meta{
		Number:       out.Number,
		Repo:         repo,
		Title:        SanitizeTitle(out.Title),
		State:        SanitizeTitle(state),
		CanonicalURL: canonical,
	}, nil
}

// apiErrorMessage extracts GitHub's "message" from an error response so a
// 403 reads as "Resource not accessible by integration" rather than a bare
// status. Sanitized and bounded — it reaches the human's terminal.
func apiErrorMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Message == "" {
		return "no message"
	}
	return BodyExcerpt(e.Message)
}
