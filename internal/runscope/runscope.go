// Package runscope computes a RUN's effective scope ceiling — the
// session's standing repos UNION the repos the human approved as
// scope EXPANSIONS during this run (issue #69).
//
// The design of record (docs/session-scope-ux-mocks.md §1.3): an
// expansion is literally a `ConfirmedIssue` whose Repo is outside
// sess.Repos; the run's effective ceiling becomes
// `sess.Repos ∪ {approved expansion repos}` and every credential mint
// scopes to that union. The session YAML remains the STANDING ceiling
// (unchanged unless the human persists the repo); the expansion lives
// only in the run's approval record and evaporates with the run.
//
// # Why a resolver and not a captured slice
//
// The ceiling GROWS mid-run. Every surface that consults it must
// re-read the run's approval record per request, exactly as the write
// gate (approvals.ConfirmedIssues) already does:
//
//  1. the proxy/brokercore scope check (InScope),
//  2. the read + write token mints (RepoNames == the token's scope),
//  3. the proxy's memoized write token and read-token cache — a token
//     minted BEFORE the expansion is scoped narrow and must be dropped
//     when the ceiling changes (that is what Key is for).
//
// # Fail-closed properties
//
//   - A record whose signature doesn't match the live session yields NO
//     expansions (approvals.ConfirmedIssues already returns nil), so a
//     mid-run session edit collapses the ceiling back to the standing
//     repos rather than widening it.
//   - Cross-owner expansions are structurally impossible: the declare
//     path rejects them before any prompt, and Repos() ALSO drops any
//     recorded repo whose owner differs from the session owner. This set
//     feeds BareNames (the installation-token mint drops the owner), so a
//     foreign-owner entry could otherwise silently scope a token against
//     the session owner's identically-named repo.
package runscope

import (
	"sort"
	"strings"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/session"
)

// Resolver answers "what may this run touch right now?".
//
// It is safe for concurrent use: every method re-reads the approval
// record from disk and holds no mutable state.
type Resolver struct {
	sess     session.Session
	stateDir string
	runID    string
	sig      string
}

// New builds a resolver for one run. A zero runID (no run context) makes
// the resolver report exactly the session's standing repos — no run, no
// expansions.
func New(sess session.Session, stateDir, runID string) *Resolver {
	return &Resolver{
		sess:     sess,
		stateDir: stateDir,
		runID:    runID,
		sig:      approvals.SignatureOf(sess),
	}
}

// Session returns the standing session this resolver was built from.
func (r *Resolver) Session() session.Session { return r.sess }

// Expansions returns the repos approved for THIS run that are outside the
// session's standing ceiling, in the order they were confirmed (deduped).
// Cross-owner entries are dropped (see the package doc).
func (r *Resolver) Expansions() []string {
	if r == nil || r.runID == "" {
		return nil
	}
	owner := session.OwnerOf(r.sess)
	var out []string
	seen := map[string]bool{}
	for _, ci := range approvals.ConfirmedIssues(r.stateDir, r.runID, r.sig) {
		repo := brokercore.RepoFromPath(ci.Repo)
		if repo == "" || r.sess.Contains(repo) {
			continue
		}
		if !strings.EqualFold(ownerOf(repo), owner) {
			continue // structurally impossible via declare; defense in depth
		}
		key := strings.ToLower(repo)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, repo)
	}
	return out
}

// Repos returns the effective ceiling: the session's standing repos first
// (stable order, as the human wrote them), then this run's expansions.
func (r *Resolver) Repos() []string {
	out := append([]string{}, r.sess.Repos...)
	return append(out, r.Expansions()...)
}

// Contains is the scope-ceiling predicate the proxy and brokercore use in
// place of session.Contains. Case- and ".git"-insensitive, like
// session.Contains (it delegates for the standing repos).
func (r *Resolver) Contains(repo string) bool {
	if r.sess.Contains(repo) {
		return true
	}
	want := strings.ToLower(brokercore.RepoFromPath(repo))
	if want == "" {
		return false
	}
	for _, e := range r.Expansions() {
		if strings.ToLower(e) == want {
			return true
		}
	}
	return false
}

// BareNames returns the "name" halves of the effective ceiling — the shape
// the installation-token mint accepts (see session.BareRepoNames). This is
// the token's scope: it MUST track Contains, or an in-scope repo would get
// a token that doesn't cover it (a silent 403 inside the agent).
func (r *Resolver) BareNames() []string {
	names := make([]string, 0, len(r.sess.Repos))
	for _, repo := range r.Repos() {
		norm := brokercore.RepoFromPath(repo)
		if _, name, ok := strings.Cut(norm, "/"); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// Key is a stable fingerprint of the effective ceiling, used to INVALIDATE
// scope-sensitive token caches: a token minted under one Key is scoped to
// that Key's repo set and must not be served after the ceiling grows.
// Order-insensitive and case-folded so a re-ordered record can't cause a
// spurious re-mint.
func (r *Resolver) Key() string {
	repos := r.Repos()
	keys := make([]string, 0, len(repos))
	for _, repo := range repos {
		keys = append(keys, strings.ToLower(brokercore.RepoFromPath(repo)))
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// ownerOf returns the owner half of a normalized owner/name.
func ownerOf(repo string) string {
	o, _, _ := strings.Cut(repo, "/")
	return o
}
