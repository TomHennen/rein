package appsetup

import (
	"bytes"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"regexp"
	"strings"
)

// Role identifies which App is being created. Drives both the
// manifest's `name` and `description` and the keystore entry name
// ("primary" / "audit").
type Role string

const (
	RolePrimary Role = "primary"
	RoleAudit   Role = "audit"
)

// Manifest is the JSON shape rein POSTs (via the browser auto-form) to
// https://github.com/settings/apps/new. JSON tags match GitHub's
// expected wire field names. `metadata:read` is included explicitly
// even though GitHub grants it implicitly.
type Manifest struct {
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	URL                string            `json:"url"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// BuildManifest assembles the per-role Manifest using the supplied port
// for redirect_url, an optional human machine label, and a freshly-
// generated random10 uniqueness suffix. `public` is hard-coded false.
// `default_events` is an empty (non-nil) slice so it serializes as [].
//
// The App name is `rein-<role>-<label>-<shortrand>` when a (sanitized,
// non-empty) label is supplied, else `rein-<role>-<shortrand>` — the
// pre-label shape, preserved so an unlabeled/headless-default run still
// gets a valid globally-unique name (onboarding-ux-design.md §4).
//
// GitHub App names are globally unique across all of GitHub (the name is
// the public github.com/apps/<slug> URL), so the random10 suffix is the
// uniqueness guard and is ALWAYS present even with a label. A collision
// (GitHub 422 "name taken") surfaces browser-side at the settings/apps/new
// POST, not on rein's HTTP path, so it can't be auto-retried here; the
// 40-bit guard makes a collision astronomically unlikely regardless.
func BuildManifest(r Role, port int, label string) (Manifest, error) {
	if r != RolePrimary && r != RoleAudit {
		return Manifest{}, fmt.Errorf("unknown manifest role %q", r)
	}
	suffix, err := randomSuffix()
	if err != nil {
		return Manifest{}, err
	}
	name := fmt.Sprintf("rein-%s-%s", r, suffix)
	if lbl := SanitizeMachineLabel(label); lbl != "" {
		name = fmt.Sprintf("rein-%s-%s-%s", r, lbl, suffix)
	}
	return Manifest{
		Name:               name,
		Description:        manifestDescription(r),
		URL:                "https://github.com/TomHennen/rein",
		RedirectURL:        fmt.Sprintf("http://127.0.0.1:%d/callback", port),
		Public:             false,
		DefaultPermissions: manifestPermissions(r),
		DefaultEvents:      []string{},
	}, nil
}

// GitHub App names are capped at 34 characters (the name is the public
// github.com/apps/<slug> URL). The name is rein-<role>-<label>-<guard>, so
// the label's budget is 34 minus the fixed parts. maxLabelLen is COMPUTED
// from that budget (not a magic number) using the LONGEST role, so it stays
// correct if the prefix, role names, or guard length ever change — and so a
// common distinctive hostname like `toms-macbook` isn't truncated into a
// name GitHub rejects browser-side at App creation.
const (
	githubAppNameMaxLen = 34
	appNameGuardLen     = 10 // hex(randomSuffix): 5 bytes => 10 chars
)

// maxLabelLen = 34 - len("rein-") - len("primary") - 2 hyphens (role|label,
// label|guard) - 10 guard = 10. Spelled out as a constant expression (all
// operands are constants) so the compiler recomputes it if any part moves.
const maxLabelLen = githubAppNameMaxLen - len("rein-") - len(string(RolePrimary)) - len("--") - appNameGuardLen

var (
	apostropheRe = regexp.MustCompile(`['’` + "`" + `]`)
	labelStripRe = regexp.MustCompile(`[^a-z0-9]+`)
)

// SanitizeMachineLabel lowercases s, replaces every run of non
// [a-z0-9] characters with a single hyphen, trims leading/trailing
// hyphens, and caps the result at maxLabelLen. Returns "" when nothing
// usable remains (caller then falls back to the label-less name shape).
//
// Exported so the CLI's hostname default and the manifest builder run the
// SAME normalization — the name the user sees at the prompt is exactly the
// name that reaches GitHub.
func SanitizeMachineLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Drop apostrophes so a possessive collapses into the word (Tom's -> toms,
	// matching design §4's `toms-laptop`), rather than splitting it. Every
	// OTHER run of non-[a-z0-9] becomes a single hyphen.
	s = apostropheRe.ReplaceAllString(s, "")
	s = labelStripRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > maxLabelLen {
		s = strings.Trim(s[:maxLabelLen], "-")
	}
	return s
}

// manifestPermissions returns the design-§Manifest-schemas permissions
// map for the given role. Pure function; tested directly.
func manifestPermissions(r Role) map[string]string {
	switch r {
	case RolePrimary:
		return map[string]string{
			"contents":      "write",
			"issues":        "write",
			"pull_requests": "write",
			"metadata":      "read",
		}
	case RoleAudit:
		return map[string]string{
			"issues":   "write",
			"metadata": "read",
		}
	default:
		panic(fmt.Sprintf("manifestPermissions: unknown role %q", r))
	}
}

func manifestDescription(r Role) string {
	switch r {
	case RolePrimary:
		return "rein credential broker — primary identity (mints scoped tokens for AI coding agents)"
	case RoleAudit:
		return "rein credential broker — audit identity (posts audit comments the agent cannot prune)"
	default:
		panic(fmt.Sprintf("manifestDescription: unknown role %q", r))
	}
}

// randomSuffix returns hex(crypto/rand.Read(5)) — exactly 10 characters,
// 40 bits of entropy. Different suffix per role; never share between
// the two manifests.
func randomSuffix() (string, error) {
	var b [5]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", fmt.Errorf("manifest suffix entropy: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// autoPostTemplate is the local landing page the user's browser is
// opened to. It contains a single hidden input with the JSON-stringified
// manifest, plus an inline script that submits the form on load.
// html/template's contextual escaping handles the manifest JSON
// (which contains double quotes); on submit the browser decodes it
// back to clean JSON in the multipart form value.
var autoPostTemplate = template.Must(template.New("autoPost").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>rein - creating GitHub App</title>
</head>
<body>
<p>Opening GitHub to create the <strong>{{.Role}}</strong> App ({{.Step}}/2)...</p>
<form id="f" method="post" action="https://github.com/settings/apps/new?state={{.State}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<noscript><button type="submit">Continue to GitHub</button></noscript>
</form>
<script>document.getElementById('f').submit();</script>
</body>
</html>
`))

type autoPostData struct {
	Role     Role
	Step     int
	State    string
	Manifest string
}

// renderAutoPostHTML produces the local landing page bytes. The
// manifest field is the JSON-serialized manifest; html/template
// escapes it for the HTML attribute context so the browser decodes
// clean JSON when it POSTs the form to GitHub.
func renderAutoPostHTML(m Manifest, state string, role Role, step int) ([]byte, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	var buf bytes.Buffer
	if err := autoPostTemplate.Execute(&buf, autoPostData{
		Role:     role,
		Step:     step,
		State:    state,
		Manifest: string(body),
	}); err != nil {
		return nil, fmt.Errorf("render auto-post html: %w", err)
	}
	return buf.Bytes(), nil
}
