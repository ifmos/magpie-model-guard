package check

import (
	"strings"
	"testing"
	"time"
)

// supplierEvents returns the protection log newest first.
func supplierEvents(t *testing.T, store *Store) []*SupplierEvent {
	t.Helper()
	events, err := store.SupplierEvents(500)
	if err != nil {
		t.Fatalf("SupplierEvents: %v", err)
	}
	return events
}

// supplierEventCount counts log entries with the given kind (and model when
// non-empty), so tests never need to couple protection hits to probe results.
func supplierEventCount(t *testing.T, store *Store, kind, model string) int {
	t.Helper()
	n := 0
	for _, ev := range supplierEvents(t, store) {
		if ev.Kind == kind && (model == "" || ev.Model == model) {
			n++
		}
	}
	return n
}

func requireRuleHold(t *testing.T, target *Target) {
	t.Helper()
	if target == nil || !target.Blocked || target.BlockSource != BlockSourceRule || !target.RuleHoldUntil.After(time.Now()) {
		t.Fatalf("expected a rule hold, got %+v", target)
	}
}

func TestRuleHoldSurvivesProbePassAndExpiresIndependently(t *testing.T) {
	_, store := newRuleTestService(t)
	target := &Target{
		Model: "vendor/big", Enabled: true, State: StatePassed,
		RuleHoldUntil: time.Now().UTC().Add(time.Hour),
		Reason:        "业务回复模型命中规则",
	}
	if err := store.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Target("vendor/big")
	requireRuleHold(t, got)

	// A passing probe rewrites only the probe state; the rule hold stays.
	got.State = StatePassed
	got.FailStreak = 0
	got.ScheduleStage = ScheduleStageNormal
	got.NextCheckAt = time.Now().UTC().Add(30 * time.Minute)
	if err := store.SaveTarget(got); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Target("vendor/big")
	requireRuleHold(t, got)
	if got.State != StatePassed {
		t.Fatalf("probe state = %q", got.State)
	}

	// Expiry alone reopens the target even when its probe state still failed.
	got.State = StateFailed
	got.RuleHoldUntil = time.Now().UTC().Add(-time.Second)
	if err := store.SaveTarget(got); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Target("vendor/big")
	if !got.Blocked || got.BlockSource != BlockSourceProbe {
		t.Fatalf("expired rule hold left source=%q blocked=%v", got.BlockSource, got.Blocked)
	}

	// The probe state decides again once the hold is gone.
	got.State = StatePassed
	if err := store.SaveTarget(got); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Target("vendor/big")
	if got.Blocked || got.BlockSource != "" {
		t.Fatalf("passed probe still blocked: %+v", got)
	}
}

func TestLegacyResponseResultsMigrateWithRuleID(t *testing.T) {
	dbPath := t.TempDir() + "/legacy.db"
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO results(id,model,prompt_id,answer,verdict,detail,actual_model,rule_id,tokens,created_at,source)
	  VALUES('legacy','vendor/big','response-model','','failed','业务回复模型 vendor/flash 命中规则 flash','vendor/flash','r-legacy',0,?, 'response')`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reopened.SupplierEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "action" || events[0].RuleID != "r-legacy" ||
		events[0].ActualModel != "vendor/flash" || events[0].Action != ResponseActionDisable {
		t.Fatalf("migrated event = %#v", events)
	}
	if results, err := reopened.Results("", "", 10); err != nil || len(results) != 0 {
		t.Fatalf("legacy rows stayed in probe results: %#v err=%v", results, err)
	}

	// A second start must not duplicate the migrated event.
	reopened.Close()
	again, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if events, _ := again.SupplierEvents(10); len(events) != 1 {
		t.Fatalf("migration duplicated events: %#v", events)
	}
}

func TestTargetEditPreservesRuleHold(t *testing.T) {
	svc, store := newRuleTestService(t)
	if err := store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true, State: StatePassed, IntervalMin: 30,
		RuleHoldUntil: time.Now().UTC().Add(time.Hour),
		Reason:        "业务回复模型命中规则",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpsertTargetOpt(&Target{
		Model: "vendor/big", Enabled: true, IntervalMin: 45, PromptIDs: []string{},
	}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Target("vendor/big")
	requireRuleHold(t, got)
	if got.IntervalMin != 45 {
		t.Fatalf("policy edit did not apply: %+v", got)
	}
}

func TestAsyncActionFailureIsLogged(t *testing.T) {
	svc, store := newRuleTestService(t)
	if err := store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-fail", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_targets BEFORE UPDATE ON targets BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "vendor/big", ActualModel: "vendor/flash", Success: true,
	}); err == nil {
		t.Fatal("action failure was swallowed")
	}
	events := supplierEvents(t, store)
	if len(events) != 1 || events[0].Kind != "error" || events[0].RuleID != "r-fail" ||
		!strings.Contains(events[0].Detail, "规则动作提交失败") {
		t.Fatalf("async failure log = %#v", events)
	}
}

func TestManualEnableClearsRuleHold(t *testing.T) {
	svc, store := newRuleTestService(t)
	if err := store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		RuleHoldUntil: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.TargetAction("vendor/big", "enable"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Target("vendor/big")
	if got.Blocked || !got.RuleHoldUntil.IsZero() {
		t.Fatalf("manual enable kept rule hold: %+v", got)
	}
}
