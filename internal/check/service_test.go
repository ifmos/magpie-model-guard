package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateway implements a minimal /v1/models + /v1/chat/completions.
type fakeGateway struct {
	mu       sync.Mutex
	answer   string
	status   int
	requests int
	models   []string // models seen in probe bodies
}

func (f *fakeGateway) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.URL.Path == "/v1/models" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "prov/a", "owned_by": "prov"}, {"id": "prov/b", "owned_by": "prov"}},
		})
		return
	}
	if r.URL.Path == "/v1/chat/completions" {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.models = append(f.models, body.Model)
		if f.status != 0 {
			http.Error(w, "boom", f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": f.answer},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"total_tokens": 42},
		})
		return
	}
	http.NotFound(w, r)
}

func newTestService(t *testing.T, gw *fakeGateway) (*Service, *Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gate := filepath.Join(dir, "gate.middleware.js")
	svc, err := newService(store, gate, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc, store, gate
}

func TestProbeRoundEndToEnd(t *testing.T) {
	// passing gateway
	gw := &fakeGateway{answer: "最少需要取出 21 个糖果。"}
	ts := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer ts.Close()

	svc, store, _ := newTestService(t, gw)
	cfg, _ := store.LoadConfig()
	cfg.GatewayURL = ts.URL
	cfg.GatewayKey = "magpie"
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	tgt := &Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}
	if err := svc.UpsertTarget(tgt); err != nil {
		t.Fatal(err)
	}
	res, err := svc.runRound(tstCtx(), tgt, mustConfig(t, store), "manual")
	if err != nil {
		t.Fatalf("runRound: %v", err)
	}
	if res.Verdict != VerdictPassed {
		t.Errorf("verdict = %s (detail %s), want passed", res.Verdict, res.Detail)
	}
	tt, _ := store.Target("prov/a")
	if tt.State != StatePassed {
		t.Errorf("target state = %s, want passed", tt.State)
	}
	// probe must have used the alias prefix
	gw.mu.Lock()
	if len(gw.models) == 0 || !strings.HasPrefix(gw.models[0], ProbeAliasPrefix) || !strings.HasSuffix(gw.models[0], "/prov/a") {
		t.Errorf("probe model = %v, want alias-wrapped", gw.models)
	}
	gw.mu.Unlock()

	// failing gateway → failed state with retry
	gw.mu.Lock()
	gw.answer = "我认为需要取出 35 个糖果。"
	gw.mu.Unlock()
	svc2res, err := svc.runRound(tstCtx(), tt, mustConfig(t, store), "scheduled")
	if err != nil {
		t.Fatalf("runRound2: %v", err)
	}
	if svc2res.Verdict != VerdictFailed {
		t.Errorf("verdict2 = %s (detail %s), want failed", svc2res.Verdict, svc2res.Detail)
	}
	tt2, _ := store.Target("prov/a")
	if tt2.State != StateFailed || tt2.FailStreak != 1 {
		t.Errorf("state after fail = %s streak=%d", tt2.State, tt2.FailStreak)
	}
	if !tt2.NextCheckAt.After(time.Now()) {
		t.Error("failed target must be scheduled for recheck")
	}
}

func TestProbeUnavailableOnGatewayError(t *testing.T) {
	gw := &fakeGateway{status: 500}
	ts := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer ts.Close()
	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = ts.URL
	cfg.GatewayKey = "magpie"
	_ = svc.Configure(cfg)
	tgt := &Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}
	_ = svc.UpsertTarget(tgt)
	res, err := svc.runRound(tstCtx(), tgt, mustConfig(t, store), "scheduled")
	if err == nil && res.Verdict == VerdictPassed {
		t.Fatal("500 gateway must not pass")
	}
	tt, _ := store.Target("prov/a")
	if tt.State != StateUnavailable {
		t.Errorf("state = %s, want unavailable", tt.State)
	}
}

func TestSchedulerRunsDueTarget(t *testing.T) {
	gw := &fakeGateway{answer: "最少取 21 个"}
	ts := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer ts.Close()
	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = ts.URL
	cfg.GatewayKey = "magpie"
	_ = svc.Configure(cfg)
	// due in the past
	_ = svc.UpsertTarget(&Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}, NextCheckAt: time.Now().Add(-time.Minute)})
	svc.schedule()
	// wait for the async round to finish
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tt, _ := store.Target("prov/a")
		if tt.State == StatePassed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	tt, _ := store.Target("prov/a")
	t.Fatalf("scheduled round never completed; state=%s", tt.State)
}

func TestDailyBudgetExhausted(t *testing.T) {
	gw := &fakeGateway{answer: "21"}
	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = "http://127.0.0.1:1"
	cfg.GatewayKey = "k"
	cfg.DailyLimit = 1
	_ = svc.Configure(cfg)
	tgt := &Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}
	_ = svc.UpsertTarget(tgt)
	// consume the single-probe daily budget with an earlier probe record
	_ = store.AppendResult(&Result{ID: "seed", Model: "prov/a", Verdict: VerdictPassed, CreatedAt: time.Now()})
	res, _ := svc.runRound(tstCtx(), tgt, cfg, "scheduled")
	if res == nil || res.Verdict != VerdictUnavailable || !strings.Contains(res.Detail, "预算") {
		t.Fatalf("budget exhaustion should mark round unavailable, got %+v", res)
	}
}


func TestListLocalInternalModels(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".config", "magpie")
	cacheDir := filepath.Join(root, ".cache", "magpie")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "models"), 0o755); err != nil {
		t.Fatal(err)
	}

	providersJSON := `{
	  "providers": [
	    {
	      "id": "apifox",
	      "unlisted": true,
	      "models": ["claude-opus-4-7", "gpt-5.4"]
	    },
	    {
	      "id": "mosai",
	      "preset": "remote-magpie",
	      "unlisted": true,
	      "models": null
	    },
	    {
	      "id": "copilot",
	      "hidden": true,
	      "models": ["gpt-4o"]
	    },
	    {
	      "id": "disabled",
	      "off": true,
	      "models": ["skip-me"]
	    }
	  ],
	  "groups": [
	    {
	      "id": "coding",
	      "members": ["apifox/gpt-5.4:xhigh", "mosai/codex/gpt-5.4"]
	    }
	  ]
	}`
	if err := os.WriteFile(filepath.Join(cfgDir, "providers.json"), []byte(providersJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	mosaiCacheJSON := `{
	  "models": [
	    {"ID": "auto/deepseek-flash"},
	    {"ID": "codex/gpt-5.4"},
	    {"ID": "codex/gpt-image-2", "Draws": true}
	  ]
	}`
	if err := os.WriteFile(filepath.Join(cacheDir, "models", "mosai.json"), []byte(mosaiCacheJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	stateCacheJSON := `{
	  "providers": {
	    "providers": [
	      {
	        "id": "codex",
	        "off": false,
	        "models": [
	          {"id": "gpt-5.3-codex", "on": true},
	          {"id": "gpt-image-1", "on": true, "kind": "image"},
	          {"id": "off-model", "on": false}
	        ]
	      },
	      {
	        "id": "copilot",
	        "off": false,
	        "models": [{"id": "gpt-4o", "on": true}]
	      }
	    ]
	  }
	}`
	if err := os.WriteFile(filepath.Join(cacheDir, "providers-state.json"), []byte(stateCacheJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	gw := &fakeGateway{answer: "ok"}
	svc, _, _ := newTestService(t, gw)
	svc.routes = &groupController{configDir: cfgDir}

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	rec := httptest.NewRecorder()
	svc.apiModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gw.requests != 0 {
		t.Fatalf("GET /api/models must not query external /v1/models, got %d requests", gw.requests)
	}

	var got []GatewayModel
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []GatewayModel{
		{ID: "apifox/claude-opus-4-7", Group: "apifox"},
		{ID: "apifox/gpt-5.4", Group: "apifox"},
		{ID: "mosai/auto/deepseek-flash", Group: "mosai"},
		{ID: "mosai/codex/gpt-5.4", Group: "mosai"},
		{ID: "codex/gpt-5.3-codex", Group: "codex"},
	}
	if len(got) != len(want) {
		t.Fatalf("models = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("models[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	cliParsed := parseLocalModels(
		"  auto/default               smart\n  group/coding               round-robin\n  apifox/gpt-5.4             OpenAI\n",
		map[string]bool{"apifox": true, "group": true, "auto": true},
	)
	if len(cliParsed) != 1 || cliParsed[0].ID != "apifox/gpt-5.4" {
		t.Fatalf("parseLocalModels must exclude top-level auto/* and group/*, got %+v", cliParsed)
	}
}

func TestInFlightProbeAppearsInResults(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		once.Do(func() { close(entered) })
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": "最少需要取出 21 个糖果。"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"total_tokens": 18},
		})
	}))
	defer ts.Close()

	svc, store, _ := newTestService(t, nil)
	access := NewAccess("", "")
	token := access.SetControl()
	mux := http.NewServeMux()
	registerAPI(mux, svc, access, nil)

	cfg := mustConfig(t, store)
	cfg.GatewayURL = ts.URL
	cfg.GatewayKey = "magpie"
	cfg.DailyLimit = 1
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertTarget(&Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendResult(&Result{
		ID:        "older-1",
		Model:     "prov/a",
		PromptID:  "candy",
		Verdict:   VerdictPassed,
		Answer:    "21",
		CreatedAt: time.Now().Add(-48 * time.Hour),
		Source:    "scheduled",
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.TargetAction("prov/a", "recheck"); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	<-entered

	getResults := func(query string) []*Result {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/results"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/results%s = %d: %s", query, rec.Code, rec.Body.String())
		}
		var rows []*Result
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}

	inFlight := getResults("")
	if len(inFlight) != 2 {
		t.Fatalf("in-flight results len = %d, want 2 (checking + older); got %+v", len(inFlight), inFlight)
	}
	if inFlight[0].Verdict != StateChecking || inFlight[0].Model != "prov/a" || inFlight[0].PromptID != "candy" || inFlight[0].Source != "manual" {
		t.Fatalf("top in-flight result = %+v, want checking prov/a candy manual", inFlight[0])
	}
	if filtered := getResults("?model=prov/other"); len(filtered) != 0 {
		t.Fatalf("filtered other model results = %+v, want empty", filtered)
	}
	if filtered := getResults("?model=prov/a&prompt=candy"); len(filtered) != 2 || filtered[0].Verdict != StateChecking {
		t.Fatalf("filtered active model/prompt results = %+v, want checking first", filtered)
	}

	snap, err := svc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	checking, _ := snap["checking"].([]string)
	if len(checking) != 1 || checking[0] != "prov/a" {
		t.Fatalf("snapshot checking = %v, want [prov/a]", checking)
	}

	close(release)
	svc.wg.Wait()

	after := getResults("")
	for _, r := range after {
		if r.Verdict == StateChecking {
			t.Fatalf("completed round left stale checking entry: %+v", r)
		}
	}
	if len(after) < 2 || after[0].Verdict != VerdictPassed {
		t.Fatalf("results after completion = %+v, want newest passed result first", after)
	}
}
// helpers

func mustConfig(t *testing.T, s *Store) GuardConfig {
	t.Helper()
	c, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tstCtx() context.Context { return context.Background() }
