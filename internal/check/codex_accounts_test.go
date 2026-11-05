package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func codexAccountFixtures(t *testing.T, logins []map[string]any) (*Store, *codexAccountController, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	configDir := filepath.Join(dir, "magpie")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(logins)
	if err := os.WriteFile(filepath.Join(configDir, "logins.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return store, newCodexAccountController(nil, store, configDir), configDir
}

func TestCodexAccountIsolationUsesProviderSwitch(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com"},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, err := readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if states["aaaaaaaaaaaaaaaa"].On || !states["aaaaaaaaaaaaaaaa"].Paused {
		t.Fatalf("target still enabled: %+v", states)
	}
	if !states["bbbbbbbbbbbbbbbb"].On {
		t.Fatalf("fallback not enabled: %+v", states)
	}
	// Recovery restores both the account and the fallback's original state.
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStatePassed,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, err = readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !states["aaaaaaaaaaaaaaaa"].On || states["aaaaaaaaaaaaaaaa"].Paused {
		t.Fatalf("target not restored: %+v", states)
	}
	if states["bbbbbbbbbbbbbbbb"].On {
		t.Fatalf("fallback not restored: %+v", states)
	}
}

func TestCodexAccountIsolationRespectsExternalEdit(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com"},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The user turns the account back on by hand. The reconciler must not
	// fight that edit by disabling it again.
	states, err := readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(codexLoginsPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["id"] == "aaaaaaaaaaaaaaaa" {
			row["on"], row["paused"] = true, false
		}
	}
	raw, _ = json.Marshal(rows)
	if err := os.WriteFile(codexLoginsPath(configDir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err == nil {
		t.Fatalf("external edit was overwritten: %+v", states)
	}
}

func TestCodexAccountIsolationNeedsFallback(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err == nil {
		t.Fatal("last account was disabled")
	}
	states, err := readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !states["aaaaaaaaaaaaaaaa"].On {
		t.Fatalf("last account was changed: %+v", states)
	}
}

func TestCodexAccountControlsSurviveRestartDuringProbe(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com"},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	controls, err := ctrl.loadControls()
	if err != nil {
		t.Fatal(err)
	}
	ctl := controls.Accounts["aaaaaaaaaaaaaaaa"]
	ctl.Probing, ctl.Applied = true, true
	controls.Accounts["aaaaaaaaaaaaaaaa"] = ctl
	if err := ctrl.saveControls(&controls); err != nil {
		t.Fatal(err)
	}
	// Simulate the process dying while the account is enabled for a retest.
	raw, _ := os.ReadFile(codexLoginsPath(configDir))
	var rows []map[string]any
	_ = json.Unmarshal(raw, &rows)
	for _, row := range rows {
		if row["id"] == "aaaaaaaaaaaaaaaa" {
			row["on"], row["paused"] = true, false
		}
	}
	raw, _ = json.Marshal(rows)
	if err := os.WriteFile(codexLoginsPath(configDir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, err := readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if states["aaaaaaaaaaaaaaaa"].On || !states["aaaaaaaaaaaaaaaa"].Paused {
		t.Fatalf("interrupted probe left account enabled: %+v", states)
	}
	controls, _ = ctrl.loadControls()
	if controls.Accounts["aaaaaaaaaaaaaaaa"].Probing {
		t.Fatal("interrupted probe marker not cleared")
	}
}

func TestCodexEndProbeDoesNotUndoRecovery(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com"},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	target := &CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
	}
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.beginProbe(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	// A manual recovery wins while the request is in flight.
	target.Isolated, target.State = false, CodexStatePassed
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.endProbe(context.Background(), target, true); err != nil {
		t.Fatal(err)
	}
	states, err := readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !states["aaaaaaaaaaaaaaaa"].On || states["aaaaaaaaaaaaaaaa"].Paused {
		t.Fatalf("cleanup undid recovery: %+v", states)
	}
}

func TestCodexAccountVerificationRequiresFallback(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com"},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexLoginsPath(configDir))
	var rows []map[string]any
	_ = json.Unmarshal(raw, &rows)
	for _, row := range rows {
		if row["id"] == "bbbbbbbbbbbbbbbb" {
			row["on"] = false
		}
	}
	raw, _ = json.Marshal(rows)
	if err := os.WriteFile(codexLoginsPath(configDir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err == nil {
		t.Fatal("disabled fallback was accepted")
	}
}

func TestCodexRosterUpdatePreservesProbeState(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "renamed@example.com"},
	}
	store, ctrl, _ := codexAccountFixtures(t, logins)
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateChecking, Isolated: true, FailStreak: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncRoster(); err != nil {
		t.Fatal(err)
	}
	target, err := store.CodexAccountTarget("aaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if target.State != CodexStateChecking || !target.Isolated || target.FailStreak != 2 {
		t.Fatalf("roster overwrote probe state: %+v", target)
	}
	if target.User != "renamed@example.com" {
		t.Fatalf("roster user not updated: %+v", target)
	}
}

func TestCodexLastAccountStillProbesWhenIsolationFails(t *testing.T) {
	gw := &codexFakeGateway{answer: "最少需要取出 21 个糖果。"}
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer server.Close()
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
	}
	store, ctrl, _ := codexAccountFixtures(t, logins)
	cfg, _ := store.LoadConfig()
	cfg.GatewayURL, cfg.GatewayKey = server.URL, "test-key"
	cfg.TimingPolicy = TimingPolicy{NormalMin: 30, ErrorRetryMin: 1, IsolationFirstMin: 30, IsolationLaterMin: 30, RecoveryConfirmMin: 1, UnavailableMin: 5}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePrompt(&Prompt{
		ID: "candy", Name: "糖果", Text: "需要多少糖果？", Grader: GraderNumber, Expect: "21", Automatic: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true, NextCheckAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.runProbe(context.Background(), "aaaaaaaaaaaaaaaa", true); err != nil {
		t.Fatal(err)
	}
	target, err := store.CodexAccountTarget("aaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if target.Isolated || target.State != CodexStatePassed {
		t.Fatalf("last account stayed stuck: %+v", target)
	}
}

func TestCodexLoginWriteRejectsStaleExpectation(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
	}
	_, ctrl, configDir := codexAccountFixtures(t, logins)
	err := ctrl.updateLogins(context.Background(), []codexLoginChange{{
		ID: "aaaaaaaaaaaaaaaa", User: "a@example.com",
		On: false, Paused: true, Check: true, FromOn: false, FromPaused: true,
	}})
	if err == nil {
		t.Fatal("stale expected state was written")
	}
	states, _ := readCodexLoginStates(configDir)
	if !states["aaaaaaaaaaaaaaaa"].On {
		t.Fatalf("stale write changed the account: %+v", states)
	}
}

func TestCodexProbeCleanupKeepsFallbackJournalOnFailure(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com"},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	target := &CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
	}
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.beginProbe(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	path := codexLoginsPath(configDir)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.endProbe(context.Background(), target, true); err == nil {
		t.Fatal("cleanup unexpectedly wrote a wide-permission file")
	}
	controls, err := ctrl.loadControls()
	if err != nil {
		t.Fatal(err)
	}
	ctl, ok := controls.Accounts["aaaaaaaaaaaaaaaa"]
	if !ok || ctl.FallbackID != "bbbbbbbbbbbbbbbb" || ctl.Probing || ctl.Applied {
		t.Fatalf("fallback journal lost: %+v", ctl)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, err := readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if states["aaaaaaaaaaaaaaaa"].On || !states["bbbbbbbbbbbbbbbb"].On {
		t.Fatalf("retry did not reapply isolation: %+v", states)
	}
}

func TestCodexSharedFallbackRestoredAfterLastOwner(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com"},
		{"id": "cccccccccccccccc", "agent": "codex", "user": "c@example.com"},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	for _, id := range []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"} {
		user := map[string]string{"aaaaaaaaaaaaaaaa": "a@example.com", "bbbbbbbbbbbbbbbb": "b@example.com"}[id]
		if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
			AccountID: id, User: user, Model: "codex/gpt-5.4",
			Enabled: true, State: CodexStateIsolated, Isolated: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	controls, err := ctrl.loadControls()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"} {
		if controls.Accounts[id].FallbackID != "cccccccccccccccc" {
			t.Fatalf("%s fallback = %+v", id, controls.Accounts[id])
		}
	}
	// Recover A; B still depends on C.
	target, _ := store.CodexAccountTarget("aaaaaaaaaaaaaaaa")
	target.Isolated, target.State = false, CodexStatePassed
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, _ := readCodexLoginStates(configDir)
	if !states["cccccccccccccccc"].On {
		t.Fatalf("shared fallback restored too early: %+v", states)
	}
	// Recover B; now C can return to its original disabled state.
	target, _ = store.CodexAccountTarget("bbbbbbbbbbbbbbbb")
	target.Isolated, target.State = false, CodexStatePassed
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, _ = readCodexLoginStates(configDir)
	if states["cccccccccccccccc"].On {
		t.Fatalf("shared fallback not restored: %+v", states)
	}
}

func TestCodexProbeResultDiscardsAfterManualRecovery(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
	}
	store, _, _ := codexAccountFixtures(t, logins)
	target := &CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateChecking,
	}
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	recovered := *target
	recovered.State, recovered.Reason = CodexStatePassed, "手动恢复"
	if err := store.SaveCodexAccountTarget(&recovered); err != nil {
		t.Fatal(err)
	}
	stale := *target
	stale.State, stale.Isolated, stale.Reason = CodexStateIsolated, true, "stale"
	committed, err := store.SaveCodexAccountProbeResult(&stale, target.Model)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("stale probe result committed after manual recovery")
	}
	got, _ := store.CodexAccountTarget(target.AccountID)
	if got.State != CodexStatePassed || got.Isolated || got.Reason != "手动恢复" {
		t.Fatalf("manual recovery overwritten: %+v", got)
	}
}

func TestCodexProbeStartDiscardsAfterManualRecovery(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
	}
	store, _, _ := codexAccountFixtures(t, logins)
	observed := &CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
		CheckedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := store.SaveCodexAccountTarget(observed); err != nil {
		t.Fatal(err)
	}
	// A manual recovery saves its own state while the probe is being prepared.
	recovered := *observed
	recovered.State, recovered.Isolated, recovered.Reason = CodexStatePassed, false, "已手动解除隔离"
	recovered.CheckedAt = observed.CheckedAt.Add(time.Minute)
	if err := store.SaveCodexAccountTarget(&recovered); err != nil {
		t.Fatal(err)
	}
	started, err := store.MarkCodexAccountChecking(observed.AccountID, observed.Model,
		"账号检测进行中，等待模型响应…", observed.State, observed.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("stale probe start overwrote a manual recovery")
	}
	got, err := store.CodexAccountTarget(observed.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != CodexStatePassed || got.Isolated || got.Reason != "已手动解除隔离" {
		t.Fatalf("manual recovery overwritten: %+v", got)
	}
	// The state the recovery leaves behind still claims the next probe.
	started, err = store.MarkCodexAccountChecking(observed.AccountID, observed.Model,
		"账号检测进行中，等待模型响应…", got.State, got.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("recovered target could not start a probe")
	}
}

func TestCodexRecoveryWithRemovedFallbackRetiresControl(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com"},
	}
	store, ctrl, configDir := codexAccountFixtures(t, logins)
	target := &CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
	}
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The fallback disappears from Magpie while the primary is isolated.
	raw, _ := os.ReadFile(codexLoginsPath(configDir))
	var rows []map[string]any
	_ = json.Unmarshal(raw, &rows)
	var kept []map[string]any
	for _, row := range rows {
		if row["id"] != "bbbbbbbbbbbbbbbb" {
			kept = append(kept, row)
		}
	}
	raw, _ = json.Marshal(kept)
	if err := os.WriteFile(codexLoginsPath(configDir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	target.Isolated, target.State = false, CodexStatePassed
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.syncAccountIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, _ := readCodexLoginStates(configDir)
	if !states["aaaaaaaaaaaaaaaa"].On {
		t.Fatalf("primary not restored: %+v", states)
	}
	controls, _ := ctrl.loadControls()
	if len(controls.Accounts) != 0 {
		t.Fatalf("obsolete control kept: %+v", controls.Accounts)
	}
}

func TestCodexProbeResultDoesNotClearMissingTombstone(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
	}
	store, _, _ := codexAccountFixtures(t, logins)
	target := &CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateChecking, Isolated: true,
	}
	if err := store.SaveCodexAccountTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateCodexAccountRoster(target.AccountID, target.User, true); err != nil {
		t.Fatal(err)
	}
	target.State, target.Isolated, target.Reason = CodexStatePassed, false, "stale pass"
	committed, err := store.SaveCodexAccountProbeResult(target, target.Model)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("probe result cleared a missing tombstone")
	}
	got, _ := store.CodexAccountTarget(target.AccountID)
	if !got.Missing || !got.Isolated || got.State != CodexStateChecking {
		t.Fatalf("tombstone changed: %+v", got)
	}
}

func TestCodexPreflightResultDoesNotOverwriteManualRecovery(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
	}
	store, _, _ := codexAccountFixtures(t, logins)
	original := &CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateIsolated, Isolated: true,
		CheckedAt: time.Now().Add(-time.Hour).UTC(),
	}
	if err := store.SaveCodexAccountTarget(original); err != nil {
		t.Fatal(err)
	}
	recovered := *original
	recovered.State, recovered.Isolated = CodexStatePassed, false
	recovered.Reason, recovered.CheckedAt = "手动解除隔离", time.Now().UTC()
	if err := store.SaveCodexAccountTarget(&recovered); err != nil {
		t.Fatal(err)
	}
	stale := *original
	stale.Reason, stale.CheckedAt = "预检失败", time.Now().UTC()
	updated, err := store.UpdateCodexAccountPreflightFailure(&stale, original.State, original.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("stale preflight result overwrote manual recovery")
	}
	got, _ := store.CodexAccountTarget(original.AccountID)
	if got.State != CodexStatePassed || got.Isolated || got.Reason != "手动解除隔离" {
		t.Fatalf("manual recovery changed: %+v", got)
	}
}

func TestCodexAccountResultHistory(t *testing.T) {
	logins := []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "on": true},
	}
	store, _, _ := codexAccountFixtures(t, logins)
	for i, verdict := range []string{"failed", "passed"} {
		if err := store.AppendCodexResult(&CodexAccountResult{
			ID: string(rune('a' + i)), AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com",
			Model: "codex/gpt-5.4", PromptID: "candy", Verdict: verdict,
			Detail: verdict, CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err := store.CodexAccountResults("aaaaaaaaaaaaaaaa", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Verdict != "passed" {
		t.Fatalf("history = %+v", rows)
	}
}
