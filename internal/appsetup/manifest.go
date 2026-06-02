package appsetup

import (
	"bytes"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
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

// BuildManifest assembles the per-role Manifest using the supplied
// port for redirect_url and a freshly-generated random10 suffix.
// `public` is hard-coded false. `default_events` is an empty (non-nil)
// slice so it serializes as [].
func BuildManifest(r Role, port int) (Manifest, error) {
	if r != RolePrimary && r != RoleAudit {
		return Manifest{}, fmt.Errorf("unknown manifest role %q", r)
	}
	suffix, err := randomSuffix()
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{
		Name:               fmt.Sprintf("rein-%s-%s", r, suffix),
		Description:        manifestDescription(r),
		URL:                "https://github.com/TomHennen/rein",
		RedirectURL:        fmt.Sprintf("http://127.0.0.1:%d/callback", port),
		Public:             false,
		DefaultPermissions: manifestPermissions(r),
		DefaultEvents:      []string{},
	}, nil
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
