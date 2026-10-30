package check

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDisableIsNotMonitoringPause(t *testing.T) {
	s, store, _ := newTestService(t, nil)
	original := &Target{Model: "vendor/model", Enabled: true, IntervalMin: 30, State: StatePassed}
	if err := s.UpsertTarget(original); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"disable", "monitor-off"} {
		if err := s.TargetAction(original.Model, action); err != nil {
			t.Fatal(err)
		}
	}
	target, err := store.Target(original.Model)
	if err != nil {
		t.Fatal(err)
	}
	if target.Enabled || !target.ManualHold || !target.Blocked {
		t.Fatalf("paused detection must retain manual block: %+v", target)
	}
	// A probe started before the user disabled the model must not undo that action.
	s.applyRoundResult(original, VerdictPassed, "", mustConfig(t, store), time.Now())
	target, err = store.Target(original.Model)
	if err != nil {
		t.Fatal(err)
	}
	if target.Enabled || !target.Blocked || !target.ManualHold {
		t.Fatalf("late probe undid user action: %+v", target)
	}
	if err := s.TargetAction(original.Model, "enable"); err != nil {
		t.Fatal(err)
	}
	target, err = store.Target(original.Model)
	if err != nil {
		t.Fatal(err)
	}
	if target.Blocked || target.ManualHold || target.Enabled {
		t.Fatalf("enable must release block without restarting monitoring: %+v", target)
	}
}

func TestProbeCompletionDoesNotRecreateDeletedTarget(t *testing.T) {
	s, store, _ := newTestService(t, nil)
	target := &Target{Model: "vendor/deleted", Enabled: true}
	if err := s.UpsertTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTarget(target.Model); err != nil {
		t.Fatal(err)
	}
	s.applyRoundResult(target, VerdictFailed, "failed", mustConfig(t, store), time.Now())
	got, err := store.Target(target.Model)
	if err != nil || got != nil {
		t.Fatalf("late probe recreated deleted target: %+v, %v", got, err)
	}
}

func TestTemporaryAllowExpiresWithoutClearingHold(t *testing.T) {
	now := time.Now()
	target := Target{ManualHold: true, Enabled: false, State: StatePassed, AllowUntil: now.Add(time.Minute)}
	if target.blockedAt(now) {
		t.Fatal("temporary allow must pass")
	}
	if !target.blockedAt(now.Add(2 * time.Minute)) {
		t.Fatal("hold must resume when temporary allow expires")
	}
}

func TestRecheckRunsWhileMonitoringPaused(t *testing.T) {
	gw := &fakeGateway{answer: "21"}
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()
	s, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = server.URL, "fixture"
	if err := s.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTarget(&Target{Model: "vendor/model", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.TargetAction("vendor/model", "disable"); err != nil {
		t.Fatal(err)
	}
	if err := s.TargetAction("vendor/model", "recheck"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		target, err := store.Target("vendor/model")
		if err != nil {
			t.Fatal(err)
		}
		if target.State == StatePassed {
			if target.Enabled || !target.ManualHold || !target.Blocked {
				t.Fatalf("recheck changed user policy: %+v", target)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("paused target did not complete explicit recheck")
}

func TestScheduledProbeRecoversAutomaticallyBlockedModel(t *testing.T) {
	gw := &fakeGateway{answer: "35"}
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()
	s, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = server.URL, "fixture"
	if err := s.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	target := &Target{Model: "vendor/model", Enabled: true, PromptIDs: []string{"candy"}}
	if err := s.UpsertTarget(target); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runRound(tstCtx(), target, cfg, "scheduled"); err != nil {
		t.Fatal(err)
	}
	target, err := store.Target(target.Model)
	if err != nil {
		t.Fatal(err)
	}
	if target.State != StateFailed || !target.Blocked || !target.Enabled || target.ManualHold {
		t.Fatalf("failed probe must isolate while retaining monitoring: %+v", target)
	}
	target.NextCheckAt = time.Now().Add(-time.Minute)
	if err := store.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	gw.mu.Lock()
	gw.answer = "21"
	gw.mu.Unlock()
	s.schedule()
	s.wg.Wait()
	target, err = store.Target(target.Model)
	if err != nil {
		t.Fatal(err)
	}
	if target.State != StatePassed || target.Blocked || !target.Enabled {
		t.Fatalf("scheduled recovery did not restore model: %+v", target)
	}
}
