package ruleset

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestSpec_ExactJSON pins the exact wire body rein sends. A silent field-name
// or value drift here could produce a ruleset GitHub accepts (201) but that
// enforces nothing — the one failure mode that fails OPEN.
func TestSpec_ExactJSON(t *testing.T) {
	branch, err := json.Marshal(branchFloorSpec())
	if err != nil {
		t.Fatalf("marshal branch: %v", err)
	}
	wantBranch := `{"name":"rein-agent-branch-floor","target":"branch","enforcement":"active","bypass_actors":[],"conditions":{"ref_name":{"include":["~ALL"],"exclude":["refs/heads/agent/**"]}},"rules":[{"type":"creation"},{"type":"update"},{"type":"deletion"}]}`
	if string(branch) != wantBranch {
		t.Errorf("branch floor JSON:\n got %s\nwant %s", branch, wantBranch)
	}

	tag, err := json.Marshal(tagFloorSpec())
	if err != nil {
		t.Fatalf("marshal tag: %v", err)
	}
	wantTag := `{"name":"rein-tag-floor","target":"tag","enforcement":"active","bypass_actors":[],"conditions":{"ref_name":{"include":["~ALL"],"exclude":[]}},"rules":[{"type":"creation"},{"type":"update"},{"type":"deletion"}]}`
	if string(tag) != wantTag {
		t.Errorf("tag floor JSON:\n got %s\nwant %s", tag, wantTag)
	}
}

// fakeGH is a stateful in-memory stand-in for GitHub's ruleset endpoints.
// transform, if set, mutates each stored ruleset on create/update — used to
// simulate a malformed-but-accepted (no-op) ruleset or drift.
type fakeGH struct {
	mu        sync.Mutex
	byID      map[int64]map[string]any
	nextID    int64
	posts     int
	puts      int
	createErr int // if non-zero, POST returns this status
	transform func(map[string]any)
}

func newFakeGH() *fakeGH { return &fakeGH{byID: map[int64]map[string]any{}, nextID: 100} }

func (f *fakeGH) seed(body map[string]any) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	f.nextID++
	body["id"] = id
	f.byID[id] = body
	return id
}

func (f *fakeGH) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// /repos/{owner}/{repo}/rulesets[/{id}]
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && len(parts) == 4: // list
			out := make([]map[string]any, 0, len(f.byID))
			for _, v := range f.byID {
				out = append(out, v)
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && len(parts) == 4: // create
			f.posts++
			if f.createErr != 0 {
				w.WriteHeader(f.createErr)
				_, _ = io.WriteString(w, `{"message":"boom"}`)
				return
			}
			body := decode(r)
			if f.transform != nil {
				f.transform(body)
			}
			id := f.nextID
			f.nextID++
			body["id"] = id
			f.byID[id] = body
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && len(parts) == 5: // get by id
			id, _ := strconv.ParseInt(parts[4], 10, 64)
			v, ok := f.byID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(v)
		case r.Method == http.MethodPut && len(parts) == 5: // update
			f.puts++
			id, _ := strconv.ParseInt(parts[4], 10, 64)
			body := decode(r)
			if f.transform != nil {
				f.transform(body)
			}
			body["id"] = id
			f.byID[id] = body
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func decode(r *http.Request) map[string]any {
	var m map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &m)
	return m
}

func TestEnsure_CreatesBothWhenAbsent(t *testing.T) {
	f := newFakeGH()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	if err := Ensure(context.Background(), srv.Client(), srv.URL, "tok", "octo", "repo"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 2 {
		t.Errorf("POST count = %d, want 2 (branch + tag floor created)", f.posts)
	}
	if len(f.byID) != 2 {
		t.Fatalf("stored rulesets = %d, want 2", len(f.byID))
	}
	// Both must be Active with empty bypass — the whole point.
	names := map[string]bool{}
	for _, v := range f.byID {
		names[v["name"].(string)] = true
		if v["enforcement"] != "active" {
			t.Errorf("ruleset %v not active", v["name"])
		}
		if ba, _ := v["bypass_actors"].([]any); len(ba) != 0 {
			t.Errorf("ruleset %v has bypass_actors %v, want empty (App must be bound)", v["name"], ba)
		}
	}
	if !names[branchFloorName] || !names[tagFloorName] {
		t.Errorf("created names = %v, want both %q and %q", names, branchFloorName, tagFloorName)
	}
}

func TestEnsure_IdempotentWhenPresentAndCorrect(t *testing.T) {
	f := newFakeGH()
	f.seed(specToMap(branchFloorSpec()))
	f.seed(specToMap(tagFloorSpec()))
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	if err := Ensure(context.Background(), srv.Client(), srv.URL, "tok", "octo", "repo"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || f.puts != 0 {
		t.Errorf("posts=%d puts=%d, want 0/0 (already correct, nothing to change)", f.posts, f.puts)
	}
}

func TestEnsure_RepairsDrift(t *testing.T) {
	f := newFakeGH()
	// Seed a DRIFTED branch floor: flipped to Evaluate (logs, does not block).
	bad := specToMap(branchFloorSpec())
	bad["enforcement"] = "evaluate"
	f.seed(bad)
	f.seed(specToMap(tagFloorSpec()))
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	if err := Ensure(context.Background(), srv.Client(), srv.URL, "tok", "octo", "repo"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != 1 {
		t.Errorf("PUT count = %d, want 1 (the drifted branch floor repaired)", f.puts)
	}
	if f.posts != 0 {
		t.Errorf("POST count = %d, want 0 (both already existed)", f.posts)
	}
	for _, v := range f.byID {
		if v["name"] == branchFloorName && v["enforcement"] != "active" {
			t.Errorf("branch floor still %v after repair, want active", v["enforcement"])
		}
	}
}

func TestEnsure_FailsClosedOnCreateError(t *testing.T) {
	f := newFakeGH()
	f.createErr = http.StatusInternalServerError
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	err := Ensure(context.Background(), srv.Client(), srv.URL, "tok", "octo", "repo")
	if err == nil {
		t.Fatal("Ensure returned nil on a create failure; must fail closed")
	}
	if !strings.Contains(err.Error(), "create ruleset") {
		t.Errorf("error = %v, want it to mention the failed create", err)
	}
}

// TestEnsure_FailsClosedOnNoOpAccept is the load-bearing case: GitHub accepts
// the create (201) but the stored ruleset does NOT enforce (Evaluate). The
// read-back verify must catch it and Ensure must error rather than trust a 201.
func TestEnsure_FailsClosedOnNoOpAccept(t *testing.T) {
	f := newFakeGH()
	f.transform = func(m map[string]any) { m["enforcement"] = "evaluate" }
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	err := Ensure(context.Background(), srv.Client(), srv.URL, "tok", "octo", "repo")
	if err == nil {
		t.Fatal("Ensure returned nil though the accepted ruleset does not enforce; must fail closed on read-back")
	}
	if !strings.Contains(err.Error(), "read-back") {
		t.Errorf("error = %v, want it to mention the read-back verification", err)
	}
}

func TestEnsure_FailsClosedOnEmptyToken(t *testing.T) {
	if err := Ensure(context.Background(), http.DefaultClient, "", "", "octo", "repo"); err == nil {
		t.Fatal("Ensure with empty token returned nil; must fail closed")
	}
}

func TestMatches_BypassActorDefeatsFloor(t *testing.T) {
	// A bypass actor (e.g. the App added to the list) means the token is NOT
	// bound — matches must reject it so Ensure repairs it.
	live := liveRuleset{
		Name:        branchFloorName,
		Target:      "branch",
		Enforcement: "active",
		BypassActors: []json.RawMessage{
			json.RawMessage(`{"actor_type":"Integration","actor_id":42}`),
		},
		Conditions: &conditions{RefName: refNameCond{Include: []string{"~ALL"}, Exclude: []string{agentRefGlob}}},
		Rules:      denyRefOps(),
	}
	if matches(live, branchFloorSpec()) {
		t.Error("matches accepted a ruleset with a bypass actor; the App would not be bound")
	}
}

// TestMatches_RejectsDrift pins matches() — the fail-closed authority — against
// every server-drift shape that would silently open the floor. A regression
// here would let verify() bless an unenforcing ruleset.
func TestMatches_RejectsDrift(t *testing.T) {
	base := func() liveRuleset {
		return liveRuleset{
			Name:        branchFloorName,
			Target:      "branch",
			Enforcement: "active",
			Conditions:  &conditions{RefName: refNameCond{Include: []string{"~ALL"}, Exclude: []string{agentRefGlob}}},
			Rules:       denyRefOps(),
		}
	}
	// Sanity: the pristine shape MUST match.
	if !matches(base(), branchFloorSpec()) {
		t.Fatal("matches rejected a correct branch floor")
	}

	cases := []struct {
		name   string
		mutate func(*liveRuleset)
	}{
		{"evaluate not active", func(l *liveRuleset) { l.Enforcement = "evaluate" }},
		{"disabled", func(l *liveRuleset) { l.Enforcement = "disabled" }},
		{"wrong target", func(l *liveRuleset) { l.Target = "tag" }},
		{"nil conditions", func(l *liveRuleset) { l.Conditions = nil }},
		{"include not ~ALL", func(l *liveRuleset) { l.Conditions.RefName.Include = []string{"refs/heads/main"} }},
		{"over-broad exclude opens all branches", func(l *liveRuleset) {
			l.Conditions.RefName.Exclude = []string{"refs/heads/**"}
		}},
		{"empty exclude (agent/** would be locked too, but not our spec)", func(l *liveRuleset) {
			l.Conditions.RefName.Exclude = []string{}
		}},
		{"missing a deny rule", func(l *liveRuleset) { l.Rules = []rule{{Type: "creation"}, {Type: "update"}} }},
		{"bypass actor present", func(l *liveRuleset) {
			l.BypassActors = []json.RawMessage{json.RawMessage(`{"actor_type":"Integration"}`)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := base()
			tc.mutate(&l)
			if matches(l, branchFloorSpec()) {
				t.Errorf("matches accepted a drifted ruleset (%s) — floor would be a no-op", tc.name)
			}
		})
	}
}

// specToMap round-trips a spec through JSON into the generic map the fake
// stores, so seeded rulesets read back byte-identical to a real create.
func specToMap(s spec) map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		panic(fmt.Sprintf("specToMap: %v", err))
	}
	return m
}
