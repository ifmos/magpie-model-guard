package check

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestTimingValidation(t *testing.T) {
	// Test minute range 1..43200
	if err := ValidateTimingPolicyMinutes(0); err == nil {
		t.Error("0 should be invalid minute")
	}
	if err := ValidateTimingPolicyMinutes(-10); err == nil {
		t.Error("negative should be invalid minute")
	}
	if err := ValidateTimingPolicyMinutes(43201); err == nil {
		t.Error("43201 should be invalid minute")
	}
	if err := ValidateTimingPolicyMinutes(1); err != nil {
		t.Errorf("1 should be valid, got: %v", err)
	}
	if err := ValidateTimingPolicyMinutes(43200); err != nil {
		t.Errorf("43200 should be valid, got: %v", err)
	}

	// Schedule mode validation
	if err := ValidateScheduleMode("fixed", false); err != nil {
		t.Errorf("fixed should be valid, got: %v", err)
	}
	if err := ValidateScheduleMode("staged", false); err != nil {
		t.Errorf("staged should be valid, got: %v", err)
	}
	if err := ValidateScheduleMode("random", false); err == nil {
		t.Error("random should be invalid mode")
	}
	if err := ValidateScheduleMode("", false); err == nil {
		t.Error("empty mode without allowEmpty should be invalid")
	}
	if err := ValidateScheduleMode("", true); err != nil {
		t.Errorf("empty mode with allowEmpty should be valid, got: %v", err)
	}

	// Global timing policy validation
	validPolicy := DefaultTimingPolicy()
	if err := ValidateGlobalTimingPolicy(validPolicy); err != nil {
		t.Errorf("default policy should be valid, got: %v", err)
	}
	invalidPolicy := validPolicy
	invalidPolicy.NormalMin = 0
	if err := ValidateGlobalTimingPolicy(invalidPolicy); err == nil {
		t.Error("global policy with 0 normal_min should be invalid")
	}

	// Target timing policy validation
	targetPolicy := &TimingPolicy{NormalMin: 45} // partial override is valid
	if err := ValidateTargetTimingPolicy(targetPolicy); err != nil {
		t.Errorf("partial target policy should be valid, got: %v", err)
	}
	invalidTargetPolicy := &TimingPolicy{NormalMin: 50000}
	if err := ValidateTargetTimingPolicy(invalidTargetPolicy); err == nil {
		t.Error("target policy with 50000 normal_min should be invalid")
	}
}

func TestTimingPolicyResolution(t *testing.T) {
	global := TimingPolicy{
		NormalMin:          40,
		ErrorRetryMin:      3,
		IsolationFirstMin:  300,
		IsolationLaterMin:  1200,
		RecoveryConfirmMin: 3,
		UnavailableMin:     20,
	}
	target := &TimingPolicy{
		NormalMin:     60,
		ErrorRetryMin: 5,
	}
	resolved := ResolveTimingPolicy(target, global)
	if resolved.NormalMin != 60 {
		t.Errorf("resolved NormalMin = %d, want 60", resolved.NormalMin)
	}
	if resolved.ErrorRetryMin != 5 {
		t.Errorf("resolved ErrorRetryMin = %d, want 5", resolved.ErrorRetryMin)
	}
	if resolved.IsolationFirstMin != 300 {
		t.Errorf("resolved IsolationFirstMin = %d, want 300", resolved.IsolationFirstMin)
	}
	if resolved.IsolationLaterMin != 1200 {
		t.Errorf("resolved IsolationLaterMin = %d, want 1200", resolved.IsolationLaterMin)
	}
	if resolved.RecoveryConfirmMin != 3 {
		t.Errorf("resolved RecoveryConfirmMin = %d, want 3", resolved.RecoveryConfirmMin)
	}
	if resolved.UnavailableMin != 20 {
		t.Errorf("resolved UnavailableMin = %d, want 20", resolved.UnavailableMin)
	}
}

func TestStagedStateTransitions(t *testing.T) {
	now := time.Now().UTC()
	policy := DefaultTimingPolicy() // Normal: 30, Retry: 2, Iso1: 360, IsoLater: 1440, RecovConf: 2, Unavail: 30

	// 1. Normal -> Error round 1: enters retry stage, 2m interval, not blocked
	s1 := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:   ScheduleStageNormal,
		CurrentState:   StatePassed,
		FailedPromptID: "candy",
		Verdict:        VerdictFailed,
		Detail:         "err1",
		Policy:         policy,
		Now:            now,
	})
	if s1.NextStage != ScheduleStageRetry || s1.FailStreak != 1 || s1.RetryPromptID != "candy" {
		t.Fatalf("step 1 failed: %+v", s1)
	}
	if s1.NextState == StateFailed {
		t.Errorf("step 1 should not be failed yet: %s", s1.NextState)
	}
	if diff := s1.NextCheckAt.Sub(now); diff != 2*time.Minute {
		t.Errorf("step 1 check diff = %v, want 2m", diff)
	}

	// 2. Retry -> Error round 2: stays in retry stage, 2m interval, not blocked
	s2 := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:   s1.NextStage,
		CurrentState:   s1.NextState,
		FailStreak:     s1.FailStreak,
		RetryPromptID:  s1.RetryPromptID,
		FailedPromptID: "candy",
		Verdict:        VerdictFailed,
		Detail:         "err2",
		Policy:         policy,
		Now:            now,
	})
	if s2.NextStage != ScheduleStageRetry || s2.FailStreak != 2 || s2.NextState == StateFailed {
		t.Fatalf("step 2 failed: %+v", s2)
	}
	if diff := s2.NextCheckAt.Sub(now); diff != 2*time.Minute {
		t.Errorf("step 2 check diff = %v, want 2m", diff)
	}

	// 3. Retry -> Error round 3: reaches 3 errors! Now isolated, blocked, 6h interval (360m)
	s3 := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:   s2.NextStage,
		CurrentState:   s2.NextState,
		FailStreak:     s2.FailStreak,
		RetryPromptID:  s2.RetryPromptID,
		FailedPromptID: "candy",
		Verdict:        VerdictFailed,
		Detail:         "err3",
		Policy:         policy,
		Now:            now,
	})
	if s3.NextStage != ScheduleStageIsolated || s3.NextState != StateFailed || s3.FailStreak != 3 {
		t.Fatalf("step 3 failed: %+v", s3)
	}
	if diff := s3.NextCheckAt.Sub(now); diff != 360*time.Minute {
		t.Errorf("step 3 check diff = %v, want 360m", diff)
	}

	// 4. Isolated -> Error round 4 (subsequent failure): stays isolated, 24h interval (1440m)
	s4 := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:      s3.NextStage,
		CurrentState:      s3.NextState,
		FailStreak:        s3.FailStreak,
		IsolationRechecks: s3.IsolationRechecks,
		RetryPromptID:     s3.RetryPromptID,
		FailedPromptID:    "candy",
		Verdict:           VerdictFailed,
		Detail:            "err4",
		Policy:            policy,
		Now:               now,
	})
	if s4.NextStage != ScheduleStageIsolated || s4.NextState != StateFailed || s4.IsolationRechecks != 1 {
		t.Fatalf("step 4 failed: %+v", s4)
	}
	if diff := s4.NextCheckAt.Sub(now); diff != 1440*time.Minute {
		t.Errorf("step 4 check diff = %v, want 1440m", diff)
	}

	// 5. Isolated -> Pass round 1: enters recovery stage, stays blocked (StateFailed), 2m confirm interval
	s5 := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:      s4.NextStage,
		CurrentState:      s4.NextState,
		FailStreak:        s4.FailStreak,
		IsolationRechecks: s4.IsolationRechecks,
		RetryPromptID:     s4.RetryPromptID,
		Verdict:           VerdictPassed,
		Detail:            "pass1",
		Policy:            policy,
		Now:               now,
	})
	if s5.NextStage != ScheduleStageRecovery || s5.NextState != StateFailed || s5.RecoveryPasses != 1 {
		t.Fatalf("step 5 failed: %+v", s5)
	}
	if diff := s5.NextCheckAt.Sub(now); diff != 2*time.Minute {
		t.Errorf("step 5 check diff = %v, want 2m", diff)
	}

	// 6. Recovery -> Pass round 2: recovered! Normal stage, unblocked (StatePassed), 30m interval
	s6 := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:      s5.NextStage,
		CurrentState:      s5.NextState,
		FailStreak:        s5.FailStreak,
		RecoveryPasses:    s5.RecoveryPasses,
		IsolationRechecks: s5.IsolationRechecks,
		RetryPromptID:     s5.RetryPromptID,
		Verdict:           VerdictPassed,
		Detail:            "pass2",
		Policy:            policy,
		Now:               now,
	})
	if s6.NextStage != ScheduleStageNormal || s6.NextState != StatePassed || s6.RecoveryPasses != 0 || s6.RetryPromptID != "" {
		t.Fatalf("step 6 failed: %+v", s6)
	}
	if diff := s6.NextCheckAt.Sub(now); diff != 30*time.Minute {
		t.Errorf("step 6 check diff = %v, want 30m", diff)
	}
}

func TestUnavailableInterruptsRecovery(t *testing.T) {
	now := time.Now().UTC()
	policy := DefaultTimingPolicy()

	// 1. In recovery with 1 pass
	recoveryIn := StagedTransitionInput{
		CurrentStage:   ScheduleStageRecovery,
		CurrentState:   StateFailed,
		RecoveryPasses: 1,
		RetryPromptID:  "candy",
		Verdict:        VerdictUnavailable,
		Detail:         "gateway 502",
		Policy:         policy,
		Now:            now,
	}
	out := ApplyStagedTransition(recoveryIn)
	if out.NextStage != ScheduleStageUnavailable || out.NextState != StateUnavailable {
		t.Fatalf("unavailable stage transition failed: %+v", out)
	}
	if out.RecoveryPasses != 0 {
		t.Errorf("recovery passes must be reset on unavailable, got %d", out.RecoveryPasses)
	}
	if diff := out.NextCheckAt.Sub(now); diff != 30*time.Minute {
		t.Errorf("unavailable interval must be 30m, got %v", diff)
	}

	// 2. Recovery from unavailable requires 2 passes
	pass1 := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:   ScheduleStageUnavailable,
		CurrentState:   StateUnavailable,
		RecoveryPasses: 0,
		RetryPromptID:  "candy",
		Verdict:        VerdictPassed,
		Policy:         policy,
		Now:            now,
	})
	if pass1.NextStage != ScheduleStageRecovery || pass1.NextState != StateFailed || pass1.RecoveryPasses != 1 {
		t.Fatalf("pass 1 after unavailable must enter recovery with 1 pass, got %+v", pass1)
	}
	pass2 := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:   pass1.NextStage,
		CurrentState:   pass1.NextState,
		RecoveryPasses: pass1.RecoveryPasses,
		RetryPromptID:  "candy",
		Verdict:        VerdictPassed,
		Policy:         policy,
		Now:            now,
	})
	if pass2.NextStage != ScheduleStageNormal || pass2.NextState != StatePassed || pass2.RecoveryPasses != 0 {
		t.Fatalf("pass 2 after unavailable must restore to normal, got %+v", pass2)
	}
}

func TestUnavailableBindsFailedPrompt(t *testing.T) {
	now := time.Now().UTC()
	policy := DefaultTimingPolicy()

	// First normal round: a prompt is unavailable while no retry prompt is bound yet.
	out := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:   ScheduleStageNormal,
		CurrentState:   StatePassed,
		FailedPromptID: "candy",
		Verdict:        VerdictUnavailable,
		Detail:         "gateway down",
		Policy:         policy,
		Now:            now,
	})
	if out.NextStage != ScheduleStageUnavailable {
		t.Fatalf("stage = %s, want unavailable", out.NextStage)
	}
	if out.RetryPromptID != "candy" {
		t.Fatalf("retry prompt = %q, want candy", out.RetryPromptID)
	}

	// An already bound retry prompt must not be replaced.
	bound := ApplyStagedTransition(StagedTransitionInput{
		CurrentStage:   ScheduleStageRecovery,
		CurrentState:   StateFailed,
		RecoveryPasses: 1,
		RetryPromptID:  "milk",
		FailedPromptID: "candy",
		Verdict:        VerdictUnavailable,
		Policy:         policy,
		Now:            now,
	})
	if bound.RetryPromptID != "milk" {
		t.Fatalf("existing retry prompt must be kept, got %q", bound.RetryPromptID)
	}
}

func TestUnavailableFirstRoundKeepsFailedPrompt(t *testing.T) {
	gw := &fakeGateway{status: 500}
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()

	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = server.URL, "fixture"
	cfg.ScheduleMode = ScheduleModeStaged
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}

	target := &Target{
		Model:     "test/unavailable-first",
		Enabled:   true,
		PromptIDs: []string{"candy"},
	}
	if err := svc.UpsertTarget(target); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.runRound(context.Background(), target, cfg, "scheduled"); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Target(target.Model)
	if err != nil || saved == nil {
		t.Fatalf("target missing: %v", err)
	}
	if saved.ScheduleStage != ScheduleStageUnavailable || saved.State != StateUnavailable {
		t.Fatalf("stage/state = %s/%s, want unavailable", saved.ScheduleStage, saved.State)
	}
	if saved.RetryPromptID != "candy" {
		t.Fatalf("retry prompt = %q, want candy", saved.RetryPromptID)
	}
}

func TestPromptRetryAndNoFalsePass(t *testing.T) {
	gw := &fakeGateway{answer: "最少需要取出 21 个糖果。"}
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()

	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = server.URL, "fixture"
	cfg.ScheduleMode = ScheduleModeStaged
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}

	target := &Target{
		Model:         "test/staged-model",
		Enabled:       true,
		ScheduleMode:  ScheduleModeStaged,
		ScheduleStage: ScheduleStageIsolated,
		State:         StateFailed,
		RetryPromptID: "deleted-prompt-id",
	}
	if err := svc.UpsertTarget(target); err != nil {
		t.Fatal(err)
	}

	// Run round: retry prompt "deleted-prompt-id" does not exist in prompt bank!
	res, err := svc.runRound(context.Background(), target, cfg, "scheduled")
	if err != nil {
		t.Fatalf("runRound failed: %v", err)
	}
	if res.Verdict == VerdictPassed {
		t.Fatal("missing retry prompt must never fake pass!")
	}
	if res.Verdict != VerdictUnavailable {
		t.Fatalf("expected VerdictUnavailable for missing retry prompt, got %s", res.Verdict)
	}

	saved, err := store.Target(target.Model)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != StateUnavailable || !saved.Blocked {
		t.Fatalf("target must be blocked in unavailable state, got %+v", saved)
	}
}

func TestRestartPersistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	policy := &TimingPolicy{
		NormalMin:          45,
		ErrorRetryMin:      5,
		IsolationFirstMin:  120,
		IsolationLaterMin:  720,
		RecoveryConfirmMin: 5,
		UnavailableMin:     45,
	}
	target := &Target{
		Model:             "test/persisted",
		Enabled:           true,
		ScheduleMode:      ScheduleModeStaged,
		TimingPolicy:      policy,
		ScheduleStage:     ScheduleStageRecovery,
		RecoveryPasses:    1,
		IsolationRechecks: 2,
		RetryPromptID:     "candy",
		State:             StateFailed,
		Reason:            "recovering",
		NextCheckAt:       time.Now().Add(2 * time.Minute).Truncate(time.Second),
	}
	if err := store.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	// Reopen database
	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	got, err := store2.Target("test/persisted")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("target not found after restart")
	}
	if got.ScheduleMode != ScheduleModeStaged {
		t.Errorf("ScheduleMode = %s, want staged", got.ScheduleMode)
	}
	if got.TimingPolicy == nil || got.TimingPolicy.NormalMin != 45 || got.TimingPolicy.ErrorRetryMin != 5 {
		t.Errorf("TimingPolicy not restored: %+v", got.TimingPolicy)
	}
	if got.ScheduleStage != ScheduleStageRecovery {
		t.Errorf("ScheduleStage = %s, want recovery", got.ScheduleStage)
	}
	if got.RecoveryPasses != 1 {
		t.Errorf("RecoveryPasses = %d, want 1", got.RecoveryPasses)
	}
	if got.IsolationRechecks != 2 {
		t.Errorf("IsolationRechecks = %d, want 2", got.IsolationRechecks)
	}
	if got.RetryPromptID != "candy" {
		t.Errorf("RetryPromptID = %s, want candy", got.RetryPromptID)
	}
	if !got.Blocked {
		t.Error("expected target to be blocked")
	}
}

func TestRecalculateSchedule(t *testing.T) {
	gw := &fakeGateway{answer: "21"}
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()

	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = server.URL, "fixture"
	_ = svc.Configure(cfg)

	fixedDue := time.Now().Add(45 * time.Minute).Truncate(time.Second)
	target := &Target{
		Model:        "test/recalc",
		Enabled:      true,
		ScheduleMode: ScheduleModeFixed,
		NextCheckAt:  fixedDue,
	}
	if err := svc.UpsertTarget(target); err != nil {
		t.Fatal(err)
	}

	// 1. Re-save without recalculate: NextCheckAt must be preserved
	update1 := &Target{
		Model:        "test/recalc",
		Enabled:      true,
		ScheduleMode: ScheduleModeStaged,
	}
	if err := svc.UpsertTargetOpt(update1, false); err != nil {
		t.Fatal(err)
	}
	saved1, _ := store.Target("test/recalc")
	if !saved1.NextCheckAt.Equal(fixedDue) {
		t.Fatalf("without recalculate, NextCheckAt should be preserved; got %v, want %v", saved1.NextCheckAt, fixedDue)
	}

	// 2. Save with recalculate: NextCheckAt must be recalculated
	update2 := &Target{
		Model:        "test/recalc",
		Enabled:      true,
		ScheduleMode: ScheduleModeStaged,
	}
	if err := svc.UpsertTargetOpt(update2, true); err != nil {
		t.Fatal(err)
	}
	saved2, _ := store.Target("test/recalc")
	if saved2.NextCheckAt.Equal(fixedDue) {
		t.Fatal("with recalculate, NextCheckAt should have changed")
	}

	// 3. Busy target should skip recalculation
	svc.mu.Lock()
	svc.running["test/recalc"] = func() {}
	svc.mu.Unlock()

	busyDue := saved2.NextCheckAt
	update3 := &Target{
		Model:        "test/recalc",
		Enabled:      true,
		ScheduleMode: ScheduleModeStaged,
	}
	if err := svc.UpsertTargetOpt(update3, true); err != nil {
		t.Fatal(err)
	}
	saved3, _ := store.Target("test/recalc")
	if !saved3.NextCheckAt.Equal(busyDue) {
		t.Fatalf("busy target must not have its schedule recalculated; got %v, want %v", saved3.NextCheckAt, busyDue)
	}

	svc.mu.Lock()
	delete(svc.running, "test/recalc")
	svc.mu.Unlock()
}

func TestModeSwitchingRetainsBlock(t *testing.T) {
	gw := &fakeGateway{answer: "21"}
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()

	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = server.URL, "fixture"
	_ = svc.Configure(cfg)

	// Target in fixed mode that is blocked (failed)
	target := &Target{
		Model:        "test/switch",
		Enabled:      true,
		ScheduleMode: ScheduleModeFixed,
		State:        StateFailed,
		FailStreak:   5,
	}
	if err := svc.UpsertTarget(target); err != nil {
		t.Fatal(err)
	}
	saved, _ := store.Target("test/switch")
	if !saved.Blocked {
		t.Fatal("target should be blocked")
	}

	// Switch to staged mode
	switchTarget := &Target{
		Model:        "test/switch",
		Enabled:      true,
		ScheduleMode: ScheduleModeStaged,
	}
	if err := svc.UpsertTarget(switchTarget); err != nil {
		t.Fatal(err)
	}

	afterSwitch, _ := store.Target("test/switch")
	if !afterSwitch.Blocked || afterSwitch.State != StateFailed {
		t.Fatalf("mode switch must not clear blocked state! got: %+v", afterSwitch)
	}
	if afterSwitch.ScheduleStage != ScheduleStageIsolated {
		t.Errorf("blocked target after switch to staged must be in isolated stage, got %s", afterSwitch.ScheduleStage)
	}
	if afterSwitch.FailStreak != 0 {
		t.Errorf("counters should reset on mode switch, got FailStreak=%d", afterSwitch.FailStreak)
	}
}

func TestStagedMultiRoundLifecycleEndToEnd(t *testing.T) {
	gw := &fakeGateway{answer: "我认为需要 35 个糖果"} // wrong answer
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()

	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = server.URL, "fixture"
	cfg.ScheduleMode = ScheduleModeStaged
	_ = svc.Configure(cfg)

	target := &Target{
		Model:        "vendor/staged-lifecycle",
		Enabled:      true,
		ScheduleMode: ScheduleModeStaged,
		PromptIDs:    []string{"candy"},
	}
	if err := svc.UpsertTarget(target); err != nil {
		t.Fatal(err)
	}

	// Round 1: Fail 1 -> Stage=retry, not blocked
	r1, err := svc.runRound(context.Background(), target, cfg, "scheduled")
	if err != nil || r1.Verdict != VerdictFailed {
		t.Fatalf("r1 failed: %v, %+v", err, r1)
	}
	t1, _ := store.Target(target.Model)
	if t1.ScheduleStage != ScheduleStageRetry || t1.Blocked || t1.FailStreak != 1 {
		t.Fatalf("after round 1: stage=%s, blocked=%v, streak=%d", t1.ScheduleStage, t1.Blocked, t1.FailStreak)
	}

	// Round 2: Fail 2 -> Stage=retry, not blocked
	r2, err := svc.runRound(context.Background(), t1, cfg, "scheduled")
	if err != nil || r2.Verdict != VerdictFailed {
		t.Fatalf("r2 failed: %v, %+v", err, r2)
	}
	t2, _ := store.Target(target.Model)
	if t2.ScheduleStage != ScheduleStageRetry || t2.Blocked || t2.FailStreak != 2 {
		t.Fatalf("after round 2: stage=%s, blocked=%v, streak=%d", t2.ScheduleStage, t2.Blocked, t2.FailStreak)
	}

	// Round 3: Fail 3 -> Stage=isolated, blocked!
	r3, err := svc.runRound(context.Background(), t2, cfg, "scheduled")
	if err != nil || r3.Verdict != VerdictFailed {
		t.Fatalf("r3 failed: %v, %+v", err, r3)
	}
	t3, _ := store.Target(target.Model)
	if t3.ScheduleStage != ScheduleStageIsolated || !t3.Blocked || t3.FailStreak != 3 || t3.State != StateFailed {
		t.Fatalf("after round 3: stage=%s, blocked=%v, streak=%d, state=%s", t3.ScheduleStage, t3.Blocked, t3.FailStreak, t3.State)
	}

	// Now gateway answers correctly: "21"
	gw.mu.Lock()
	gw.answer = "最少需要取出 21 个糖果。"
	gw.mu.Unlock()

	// Round 4: Pass 1 in isolation -> Stage=recovery, recovery_passes=1, still blocked!
	r4, err := svc.runRound(context.Background(), t3, cfg, "scheduled")
	if err != nil || r4.Verdict != VerdictPassed {
		t.Fatalf("r4 failed: %v, %+v", err, r4)
	}
	t4, _ := store.Target(target.Model)
	if t4.ScheduleStage != ScheduleStageRecovery || !t4.Blocked || t4.RecoveryPasses != 1 {
		t.Fatalf("after pass 1: stage=%s, blocked=%v, passes=%d", t4.ScheduleStage, t4.Blocked, t4.RecoveryPasses)
	}

	// Round 5: Pass 2 in recovery -> Stage=normal, unblocked!
	r5, err := svc.runRound(context.Background(), t4, cfg, "scheduled")
	if err != nil || r5.Verdict != VerdictPassed {
		t.Fatalf("r5 failed: %v, %+v", err, r5)
	}
	t5, _ := store.Target(target.Model)
	if t5.ScheduleStage != ScheduleStageNormal || t5.Blocked || t5.State != StatePassed || t5.RecoveryPasses != 0 || t5.RetryPromptID != "" {
		t.Fatalf("after pass 2: stage=%s, blocked=%v, state=%s, passes=%d, retryID=%s", t5.ScheduleStage, t5.Blocked, t5.State, t5.RecoveryPasses, t5.RetryPromptID)
	}
}

func TestGlobalModeSwitchWithoutRecalc(t *testing.T) {
	svc, store, _ := newTestService(t, nil)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = "http://127.0.0.1:1", "fixture"
	cfg.ScheduleMode = ScheduleModeStaged
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}

	specificNext := time.Now().Add(45 * time.Minute).Truncate(time.Second)
	if err := svc.UpsertTarget(&Target{
		Model:         "test/mode-switch",
		Enabled:       true,
		State:         StateFailed,
		ScheduleStage: ScheduleStageIsolated,
		FailStreak:    2,
		NextCheckAt:   specificNext,
	}); err != nil {
		t.Fatal(err)
	}

	// Switch global to fixed with recalculate=false
	cfg2 := mustConfig(t, store)
	cfg2.ScheduleMode = ScheduleModeFixed
	if err := svc.ConfigureOpt(cfg2, false); err != nil {
		t.Fatal(err)
	}

	saved, err := store.Target("test/mode-switch")
	if err != nil || saved == nil {
		t.Fatalf("target missing after mode switch: %v", err)
	}
	if !saved.Blocked {
		t.Error("mode switch without recalc must retain block")
	}
	if saved.State != StateFailed {
		t.Errorf("state must remain failed, got %s", saved.State)
	}
	if saved.FailStreak != 0 {
		t.Errorf("mode switch must reset fail streak, got %d", saved.FailStreak)
	}
	if !saved.NextCheckAt.Equal(specificNext) {
		t.Errorf("NextCheckAt must be preserved without recalc: got %v, want %v", saved.NextCheckAt, specificNext)
	}
}

func TestConfigureRejectsNegativeTiming(t *testing.T) {
	svc, store, _ := newTestService(t, nil)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = "http://127.0.0.1:1", "fixture"
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	before := mustConfig(t, store)

	bad := mustConfig(t, store)
	bad.TimingPolicy.NormalMin = -5
	if err := svc.ConfigureOpt(bad, false); err == nil {
		t.Fatal("negative timing value must be rejected, not replaced")
	}

	after := mustConfig(t, store)
	if after.TimingPolicy.NormalMin != before.TimingPolicy.NormalMin {
		t.Errorf("rejected config must not be persisted: got %d, want %d", after.TimingPolicy.NormalMin, before.TimingPolicy.NormalMin)
	}

	badTarget := &Target{
		Model:        "test/negative",
		Enabled:      true,
		TimingPolicy: &TimingPolicy{IsolationFirstMin: -1},
	}
	if err := svc.UpsertTarget(badTarget); err == nil {
		t.Fatal("negative target timing value must be rejected")
	}
	if got, _ := store.Target("test/negative"); got != nil {
		t.Fatalf("invalid target must not be saved: %+v", got)
	}
}

func TestInFlightModeChangeWritebackSafety(t *testing.T) {
	svc, store, _ := newTestService(t, nil)
	cfg := mustConfig(t, store)
	cfg.GatewayURL, cfg.GatewayKey = "http://127.0.0.1:1", "fixture"
	cfg.ScheduleMode = ScheduleModeStaged
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}

	if err := svc.UpsertTarget(&Target{
		Model:         "test/inflight",
		Enabled:       true,
		State:         StatePassed,
		ScheduleStage: ScheduleStageRetry,
		FailStreak:    1,
	}); err != nil {
		t.Fatal(err)
	}

	// Snapshot the staged config as captured when the probe started.
	stagedCfg := mustConfig(t, store)

	// Global switches to fixed while the probe is in flight (reset counters, keep block).
	fixedCfg := mustConfig(t, store)
	fixedCfg.ScheduleMode = ScheduleModeFixed
	if err := svc.ConfigureOpt(fixedCfg, false); err != nil {
		t.Fatal(err)
	}

	// Old in-flight staged result arrives: a single failure must NOT be applied
	// as a fixed-mode isolation.
	svc.applyRoundResult(&Target{Model: "test/inflight"}, VerdictFailed, "inflight fail", stagedCfg, time.Now())

	saved, err := store.Target("test/inflight")
	if err != nil || saved == nil {
		t.Fatalf("target missing: %v", err)
	}
	if saved.Blocked || saved.State == StateFailed {
		t.Fatalf("stale staged result must not be applied as fixed isolation: %+v", saved)
	}
	if saved.ScheduleStage == ScheduleStageIsolated {
		t.Fatalf("stale result must not move target into isolated stage: %+v", saved)
	}
	if saved.FailStreak != 0 {
		t.Fatalf("stale result must not mutate fail streak: %+v", saved)
	}

	// A fresh round in the new mode still applies normally.
	svc.applyRoundResult(&Target{Model: "test/inflight"}, VerdictPassed, "fresh pass", fixedCfg, time.Now())
	saved, _ = store.Target("test/inflight")
	if saved.State != StatePassed || saved.Blocked || saved.ScheduleStage != ScheduleStageNormal {
		t.Fatalf("fresh result in current mode must apply: %+v", saved)
	}
}
