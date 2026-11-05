package check

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestValidateProbeEffort(t *testing.T) {
	for _, tc := range []struct {
		value        string
		allowDefault bool
		ok           bool
	}{
		{"", false, true},
		{"high", false, true},
		{"xhigh", false, true},
		{"default", false, false},
		{"default", true, true},
		{"auto", true, false},
		{"HIGH", true, false},
	} {
		if err := ValidateProbeEffort(tc.value, tc.allowDefault); (err == nil) != tc.ok {
			t.Fatalf("ValidateProbeEffort(%q, %v) = %v, want ok=%v", tc.value, tc.allowDefault, err, tc.ok)
		}
	}
}

func TestEffectiveProbeEffort(t *testing.T) {
	for _, tc := range []struct{ target, global, want string }{
		{"", "", ""},
		{"", "low", "low"},
		{"high", "low", "high"},
		{"default", "low", ""},
	} {
		if got := EffectiveProbeEffort(tc.target, tc.global); got != tc.want {
			t.Fatalf("EffectiveProbeEffort(%q, %q) = %q, want %q", tc.target, tc.global, got, tc.want)
		}
	}
}

func TestProbeEffortReachesGateway(t *testing.T) {
	gw := &fakeGateway{answer: "最少需要取出 21 个糖果。"}
	ts := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer ts.Close()

	svc, store, _ := newTestService(t, gw)
	cfg, _ := store.LoadConfig()
	cfg.GatewayURL, cfg.GatewayKey = ts.URL, "magpie"
	cfg.ProbeEffort = "low"
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertTarget(&Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}); err != nil {
		t.Fatal(err)
	}
	tgt, _ := store.Target("prov/a")
	if _, err := svc.runRound(tstCtx(), tgt, mustConfig(t, store), "manual"); err != nil {
		t.Fatal(err)
	}

	override := &Target{Model: "prov/b", Enabled: true, PromptIDs: []string{"candy"}, ProbeEffort: "high"}
	if err := svc.UpsertTarget(override); err != nil {
		t.Fatal(err)
	}
	read, _ := store.Target("prov/b")
	if read.ProbeEffort != "high" {
		t.Fatalf("stored effort = %q, want high", read.ProbeEffort)
	}
	if _, err := svc.runRound(tstCtx(), read, mustConfig(t, store), "manual"); err != nil {
		t.Fatal(err)
	}

	optOut := &Target{Model: "prov/c", Enabled: true, PromptIDs: []string{"candy"}, ProbeEffort: ProbeEffortModelDefault}
	if err := svc.UpsertTarget(optOut); err != nil {
		t.Fatal(err)
	}
	read, _ = store.Target("prov/c")
	if _, err := svc.runRound(tstCtx(), read, mustConfig(t, store), "manual"); err != nil {
		t.Fatal(err)
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.efforts) != 3 || gw.efforts[0] != "low" || gw.efforts[1] != "high" || gw.efforts[2] != "" {
		t.Fatalf("gateway efforts = %v, want [low high \"\"]", gw.efforts)
	}
}

func TestProbeEffortValidationOnSave(t *testing.T) {
	svc, store, _ := newTestService(t, nil)
	cfg, _ := store.LoadConfig()
	cfg.ProbeEffort = "ultra"
	if err := svc.Configure(cfg); err == nil {
		t.Fatal("invalid global effort accepted")
	}
	if err := svc.UpsertTarget(&Target{Model: "prov/a", Enabled: true, ProbeEffort: "auto"}); err == nil {
		t.Fatal("invalid target effort accepted")
	}
}

func TestInflightEffortChangeDiscardsVerdict(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var gotEffort string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		gotEffort = body.ReasoningEffort
		mu.Unlock()
		once.Do(func() { close(entered) })
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": "最少需要取出 21 个糖果。"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"total_tokens": 12},
		})
	}))
	defer ts.Close()

	svc, store, _ := newTestService(t, nil)
	cfg, _ := store.LoadConfig()
	cfg.GatewayURL, cfg.GatewayKey = ts.URL, "magpie"
	cfg.ProbeEffort = "high"
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertTarget(&Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}); err != nil {
		t.Fatal(err)
	}
	start, err := store.Target("prov/a")
	if err != nil || start == nil {
		t.Fatalf("target: %v", err)
	}
	roundCfg := mustConfig(t, store)

	done := make(chan *Result, 1)
	go func() {
		res, _ := svc.runRound(tstCtx(), start, roundCfg, "manual")
		done <- res
	}()
	<-entered

	current, err := store.Target("prov/a")
	if err != nil {
		t.Fatal(err)
	}
	current.ProbeEffort = "low"
	if err := svc.UpsertTarget(current); err != nil {
		t.Fatal(err)
	}
	close(release)
	res := <-done
	if res == nil || res.Verdict != VerdictPassed {
		t.Fatalf("round result = %+v, want passed", res)
	}

	mu.Lock()
	effort := gotEffort
	mu.Unlock()
	if effort != "high" {
		t.Fatalf("in-flight probe effort = %q, want high", effort)
	}
	after, err := store.Target("prov/a")
	if err != nil {
		t.Fatal(err)
	}
	if after.State == StatePassed {
		t.Fatalf("verdict from the old effort was applied: %+v", after)
	}
	if after.ProbeEffort != "low" {
		t.Fatalf("effort update lost: %+v", after)
	}
	if after.CheckedAt.IsZero() {
		t.Fatal("discarded round did not record checked time")
	}
}
