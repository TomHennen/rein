// Validated writes to the session file (issue #69). The session YAML stays
// the STANDING scope ceiling, but it stops being hand-maintained: `rein
// session add-repo` and the in-prompt "also save this repo?" answer both
// land here, so every widening is validated AT WRITE TIME instead of
// discovering the mistake at the next launch (the #53/#59 lesson).
//
// # What this file validates, and what it deliberately does NOT
//
// AddRepoToFile enforces the STRUCTURAL rules — owner/name shape, the
// single-owner rule (Validate), and dedupe — and nothing else. It performs
// NO network I/O.
//
// Install-coverage (is the App installed on the new repo?) is a NETWORK
// probe and lives with the callers that have a GitHub client and can act on
// the answer:
//
//   - `rein session add-repo` probes BEFORE calling this, and refuses the
//     add with the install deep-link on a 404 (nothing is written).
//   - the in-prompt persist path is reached only AFTER the declare already
//     probed coverage (a 404 never reaches a prompt at all — it becomes the
//     install NOTICE), so the repo is known-covered by the time the human
//     answers `y`.
//
// Keeping the probe out of the writer is what lets the out-of-process grant
// surface (the tmux popup, which has no App client and must never make a
// network call — issue #35 §4 "the popup never fetches") persist the repo.
//
// # Comment preservation
//
// The edit is performed on the yaml NODE tree, not by re-marshalling the
// Session struct: a developer's `dev-session.yaml` may carry comments and
// keys rein doesn't model, and a struct round-trip would silently delete
// them. The result is re-parsed and Validate()d before it is written, so a
// node edit can never produce a file that fails to load.
package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TomHennen/rein/internal/brokercore"
	"gopkg.in/yaml.v3"
)

// ErrRepoAlreadyInSession is returned by AddRepoToFile when the repo is
// already in the session's repos list. The file is left untouched; callers
// report it as a friendly no-op, not a failure.
var ErrRepoAlreadyInSession = errors.New("repo is already in the session")

// OwnerOf returns the session's single owner (sessions are single-owner by
// Validate). Empty for an empty/invalid session.
func OwnerOf(s Session) string {
	for _, r := range s.Repos {
		if n := normalizeRepo(r); n != "" {
			owner, _, _ := strings.Cut(n, "/")
			return owner
		}
	}
	return ""
}

// SourceFilePath maps the source tag LoadOrFallback returns ("file:<path>"
// or "env-fallback") back to a session FILE path, or "" when the session did
// not come from a file. A session with no file behind it cannot be persisted
// to — callers use "" to suppress the persist offer entirely.
func SourceFilePath(source string) string {
	path, ok := strings.CutPrefix(source, "file:")
	if !ok {
		return ""
	}
	return path
}

// CheckAddRepo validates a candidate repo against a session WITHOUT writing
// anything: owner/name shape and the single-owner rule. It is the structural
// half of the add — callers run their install-coverage probe separately.
// Returns the NORMALIZED-case repo string to record (the caller's spelling,
// trimmed of ".git"/slashes), or an error whose text is user-facing.
func CheckAddRepo(s Session, repo string) (string, error) {
	norm := brokercore.RepoFromPath(strings.TrimSpace(repo))
	if norm == "" || strings.Count(norm, "/") != 1 {
		suggestion := ""
		if owner := OwnerOf(s); owner != "" && !strings.Contains(strings.TrimSpace(repo), "/") && strings.TrimSpace(repo) != "" {
			suggestion = fmt.Sprintf(" Did you mean %s/%s?", owner, strings.TrimSpace(repo))
		}
		return "", fmt.Errorf("%q is not owner/name-shaped.%s", repo, suggestion)
	}
	owner, _, _ := strings.Cut(norm, "/")
	sessOwner := OwnerOf(s)
	if sessOwner != "" && !strings.EqualFold(owner, sessOwner) {
		// The single-owner rule (session.Validate; BareRepoNames minting).
		// Structural, never a human decision — see the mocks' security notes.
		return "", fmt.Errorf("cannot add %s — session %s is scoped to owner %s, and a session must stay single-owner (the App installation is single-owner; a mixed list would mint ambiguous token scopes)",
			norm, s.ID, sessOwner)
	}
	if s.Contains(norm) {
		return norm, ErrRepoAlreadyInSession
	}
	return norm, nil
}

// AddRepoToFile appends repo to the `repos:` list of the session file at
// path, atomically. It validates structurally (CheckAddRepo) against the
// session ON DISK — not against a caller-supplied snapshot — so a concurrent
// edit can't be clobbered by a stale view, and re-validates the resulting
// document before replacing the file.
//
// Returns ErrRepoAlreadyInSession (and writes nothing) if the repo is
// already there.
func AddRepoToFile(path, repo string) (Session, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Session{}, err
	}
	var current Session
	if err := yaml.Unmarshal(body, &current); err != nil {
		return Session{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := current.Validate(); err != nil {
		return Session{}, fmt.Errorf("invalid session %s: %w (fix it before adding a repo)", path, err)
	}
	norm, err := CheckAddRepo(current, repo)
	if err != nil {
		return current, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return Session{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := appendRepoNode(&doc, norm); err != nil {
		return Session{}, fmt.Errorf("edit %s: %w", path, err)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return Session{}, fmt.Errorf("render %s: %w", path, err)
	}
	// Re-parse + Validate the rendered document BEFORE it replaces the file:
	// a node edit must never be able to produce a session that won't load.
	var updated Session
	if err := yaml.Unmarshal(out, &updated); err != nil {
		return Session{}, fmt.Errorf("re-parse edited session: %w", err)
	}
	if err := updated.Validate(); err != nil {
		return Session{}, fmt.Errorf("edited session would be invalid: %w (nothing written)", err)
	}
	if !updated.Contains(norm) {
		return Session{}, fmt.Errorf("edited session does not contain %s (nothing written)", norm)
	}
	if err := writeAtomic(path, out); err != nil {
		return Session{}, err
	}
	return updated, nil
}

// appendRepoNode appends a scalar to the document's top-level `repos:`
// sequence, preserving every other key and comment in the file.
func appendRepoNode(doc *yaml.Node, repo string) error {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return errors.New("not a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return errors.New("session file root is not a mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "repos" {
			continue
		}
		seq := root.Content[i+1]
		if seq.Kind != yaml.SequenceNode {
			return errors.New("`repos:` is not a list")
		}
		seq.Content = append(seq.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: repo,
		})
		return nil
	}
	return errors.New("no `repos:` key in the session file")
}

// writeAtomic replaces path with body via a same-dir temp file + rename, so
// a crash mid-write can never leave a truncated session file (which would
// fail closed at the next launch, but noisily and for no reason). Mode 0600.
func writeAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp next to %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
