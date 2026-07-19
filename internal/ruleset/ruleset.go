// Package ruleset installs and verifies the GitHub repository rulesets that
// enforce rein's `agent/**` branch floor SERVER-SIDE.
//
// Why this exists: rein's per-run push token carries contents:write but is NOT
// on these rulesets' bypass list, so GitHub itself rejects (GH013) any ref
// create/update/delete outside refs/heads/agent/** — even though the token
// could otherwise push anywhere. This is the server-authoritative enforcement
// that lets rein eventually drop the client-side receive-pack parser (a later
// step; this package only ADDS the floor).
//
// Two rulesets are ensured per repo:
//   - branch floor: target=branch, all branches EXCEPT refs/heads/agent/**
//     may not be created/updated/deleted.
//   - tag floor:    target=tag, no tag may be created/updated/deleted
//     ("never main/tags", decision-rein-broker.md).
//
// Enforcement is Active (not Evaluate — Evaluate logs but does not block).
// bypass_actors is empty, so the App's own tokens are bound.
//
// FUTURE WORK (not built here): a per-issue floor (refs/heads/agent/<n>/**)
// needs dynamic per-run rulesets with concurrency caveats; this package ships
// only the coarse agent/** floor.
package ruleset

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const (
	branchFloorName = "rein-agent-branch-floor"
	tagFloorName    = "rein-tag-floor"

	// agentRefGlob is the ONE ref pattern excluded from the branch floor: only
	// these refs may be created/updated. Kept in sync with the client-side
	// receive-pack check (the parser, still present this cycle).
	agentRefGlob = "refs/heads/agent/**"

	enforcementActive = "active"
)

// Doer is the minimal HTTP surface Ensure needs; *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// refNameCond is a ruleset's ref_name include/exclude condition. Slices are
// always non-nil so they serialize as [] (GitHub rejects null here).
type refNameCond struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type conditions struct {
	RefName refNameCond `json:"ref_name"`
}

type rule struct {
	Type string `json:"type"`
}

// spec is both the create/update request body and the expected shape Ensure
// verifies a live ruleset against.
type spec struct {
	Name         string     `json:"name"`
	Target       string     `json:"target"`
	Enforcement  string     `json:"enforcement"`
	BypassActors []any      `json:"bypass_actors"` // always [] — the App is bound
	Conditions   conditions `json:"conditions"`
	Rules        []rule     `json:"rules"`
}

// denyRefOps are the operations the floor blocks on its targeted refs. All
// three together mean "the token may not touch these refs at all."
func denyRefOps() []rule {
	return []rule{{Type: "creation"}, {Type: "update"}, {Type: "deletion"}}
}

// branchFloorSpec: every branch EXCEPT refs/heads/agent/** is locked.
func branchFloorSpec() spec {
	return spec{
		Name:         branchFloorName,
		Target:       "branch",
		Enforcement:  enforcementActive,
		BypassActors: []any{},
		Conditions:   conditions{RefName: refNameCond{Include: []string{"~ALL"}, Exclude: []string{agentRefGlob}}},
		Rules:        denyRefOps(),
	}
}

// tagFloorSpec: every tag is locked (agents never push tags).
func tagFloorSpec() spec {
	return spec{
		Name:         tagFloorName,
		Target:       "tag",
		Enforcement:  enforcementActive,
		BypassActors: []any{},
		Conditions:   conditions{RefName: refNameCond{Include: []string{"~ALL"}, Exclude: []string{}}},
		Rules:        denyRefOps(),
	}
}

// liveRuleset is the subset of GitHub's ruleset object Ensure reads back to
// verify the floor is real (not a malformed-but-accepted no-op).
type liveRuleset struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	Target       string            `json:"target"`
	Enforcement  string            `json:"enforcement"`
	BypassActors []json.RawMessage `json:"bypass_actors"`
	Conditions   *conditions       `json:"conditions"`
	Rules        []rule            `json:"rules"`
}

// Ensure makes both the branch and tag floor exist and match spec on
// owner/repo, using an installation token with administration:write. It is
// idempotent (create if absent, verify/repair if present) and FAILS CLOSED:
// any create/update/verify failure returns an error and the caller must NOT
// let the agent push.
//
// apiBase defaults to https://api.github.com; pass a non-empty override (e.g.
// REIN_GITHUB_API_BASE) to point at a fake in tests.
func Ensure(ctx context.Context, doer Doer, apiBase, token, owner, repo string) error {
	if token == "" {
		return fmt.Errorf("ruleset: empty token")
	}
	base := strings.TrimSuffix(apiBase, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	for _, s := range []spec{branchFloorSpec(), tagFloorSpec()} {
		if err := ensureOne(ctx, doer, base, token, owner, repo, s); err != nil {
			return fmt.Errorf("ruleset %q on %s/%s: %w", s.Name, owner, repo, err)
		}
	}
	return nil
}

func ensureOne(ctx context.Context, doer Doer, base, token, owner, repo string, want spec) error {
	existing, err := findByName(ctx, doer, base, token, owner, repo, want.Name)
	if err != nil {
		return err
	}
	if existing == nil {
		id, err := create(ctx, doer, base, token, owner, repo, want)
		if err != nil {
			return err
		}
		return verify(ctx, doer, base, token, owner, repo, id, want)
	}
	// Present: check it, repair on drift, then re-verify. A ruleset that has
	// drifted (someone flipped it to Evaluate, added a bypass) is repaired, not
	// trusted.
	live, err := get(ctx, doer, base, token, owner, repo, existing.ID)
	if err != nil {
		return err
	}
	if matches(live, want) {
		return nil
	}
	if err := update(ctx, doer, base, token, owner, repo, existing.ID, want); err != nil {
		return err
	}
	return verify(ctx, doer, base, token, owner, repo, existing.ID, want)
}

// findByName lists the repo's rulesets and returns the one named want, or nil.
// The list endpoint is enough to find the id (it omits rules/conditions on some
// responses — verify re-fetches the full object).
//
// Single page only (no pagination): a repo carries a handful of rulesets, well
// under one page. If our ruleset ever landed on a later page we'd miss it and
// re-create — which is fail-CLOSED (GitHub errors on a duplicate name, or the
// second Active ruleset only adds restriction), never fail-open.
func findByName(ctx context.Context, doer Doer, base, token, owner, repo, name string) (*liveRuleset, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/rulesets", base, owner, repo)
	body, status, err := do(ctx, doer, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiErr("list rulesets", status, body)
	}
	var list []liveRuleset
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse ruleset list: %w", err)
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

func create(ctx context.Context, doer Doer, base, token, owner, repo string, want spec) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/rulesets", base, owner, repo)
	reqBody, err := json.Marshal(want)
	if err != nil {
		return 0, fmt.Errorf("marshal ruleset: %w", err)
	}
	body, status, err := do(ctx, doer, http.MethodPost, url, token, reqBody)
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return 0, apiErr("create ruleset", status, body)
	}
	var out liveRuleset
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("parse create response: %w", err)
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("create returned id 0")
	}
	return out.ID, nil
}

func update(ctx context.Context, doer Doer, base, token, owner, repo string, id int64, want spec) error {
	url := fmt.Sprintf("%s/repos/%s/%s/rulesets/%d", base, owner, repo, id)
	reqBody, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("marshal ruleset: %w", err)
	}
	body, status, err := do(ctx, doer, http.MethodPut, url, token, reqBody)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return apiErr("update ruleset", status, body)
	}
	return nil
}

func get(ctx context.Context, doer Doer, base, token, owner, repo string, id int64) (liveRuleset, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/rulesets/%d", base, owner, repo, id)
	body, status, err := do(ctx, doer, http.MethodGet, url, token, nil)
	if err != nil {
		return liveRuleset{}, err
	}
	if status != http.StatusOK {
		return liveRuleset{}, apiErr("get ruleset", status, body)
	}
	var out liveRuleset
	if err := json.Unmarshal(body, &out); err != nil {
		return liveRuleset{}, fmt.Errorf("parse ruleset: %w", err)
	}
	return out, nil
}

// verify re-fetches the ruleset by id and asserts it matches want. This is the
// load-bearing fail-closed check: GitHub can return 201 for a malformed body
// while enforcing nothing, so "create succeeded" is NOT proof — a read-back
// that is Active, bypass-free, and correctly targeted is.
func verify(ctx context.Context, doer Doer, base, token, owner, repo string, id int64, want spec) error {
	live, err := get(ctx, doer, base, token, owner, repo, id)
	if err != nil {
		return err
	}
	if !matches(live, want) {
		return fmt.Errorf("ruleset read-back does not enforce the floor "+
			"(enforcement=%q target=%q bypass_actors=%d rules=%v conditions=%+v); refusing to trust it",
			live.Enforcement, live.Target, len(live.BypassActors), ruleTypes(live.Rules), condOf(live))
	}
	return nil
}

// matches reports whether a live ruleset actually enforces want: Active, no
// bypass actors, correct target, and exactly want's rule types and ref_name
// include/exclude sets.
func matches(live liveRuleset, want spec) bool {
	if live.Enforcement != enforcementActive {
		return false
	}
	if len(live.BypassActors) != 0 {
		return false
	}
	if live.Target != want.Target {
		return false
	}
	if !sameSet(ruleTypes(live.Rules), ruleTypes(want.Rules)) {
		return false
	}
	if live.Conditions == nil {
		return false
	}
	if !sameSet(live.Conditions.RefName.Include, want.Conditions.RefName.Include) {
		return false
	}
	if !sameSet(live.Conditions.RefName.Exclude, want.Conditions.RefName.Exclude) {
		return false
	}
	return true
}

func ruleTypes(rs []rule) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Type)
	}
	return out
}

func condOf(l liveRuleset) refNameCond {
	if l.Conditions == nil {
		return refNameCond{}
	}
	return l.Conditions.RefName
}

// sameSet reports set-equality (order-independent, duplicates collapsed) so a
// GitHub-side reordering of rules or patterns is not read as drift.
func sameSet(a, b []string) bool {
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	as = dedupSorted(as)
	bs = dedupSorted(bs)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func dedupSorted(s []string) []string {
	out := s[:0]
	var last string
	for i, v := range s {
		if i == 0 || v != last {
			out = append(out, v)
		}
		last = v
	}
	return out
}

// do issues one authenticated request and returns the body + status.
func do(ctx context.Context, doer Doer, method, url, token string, reqBody []byte) ([]byte, int, error) {
	var rdr io.Reader
	if reqBody != nil {
		rdr = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	return body, resp.StatusCode, nil
}

func apiErr(op string, status int, body []byte) error {
	excerpt := string(body)
	if len(excerpt) > 512 {
		excerpt = excerpt[:512] + "...(truncated)"
	}
	return fmt.Errorf("%s: HTTP %d: %s", op, status, excerpt)
}
