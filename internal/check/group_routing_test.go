package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func setupTestEnv(t *testing.T) (*Store, string, *groupController) {
	t.Helper()
	dir := t.TempDir()

	dbPath := filepath.Join(dir, "test.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	configDir := filepath.Join(dir, "magpie")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	ctrl := newTestController(store, configDir)
	return store, configDir, ctrl
}

// newTestController builds a controller whose auto-group discovery is stubbed
// out, so tests never shell out to a real magpie (and need no CLI on PATH).
func newTestController(store *Store, configDir string) *groupController {
	ctrl := newGroupController(store, configDir, "")
	ctrl.discoverAuto = func(ctx context.Context, magpiePath string) ([]autoGroupSummary, error) {
		return nil, nil
	}
	ctrl.fetchMembers = func(ctx context.Context, magpiePath, ref string) ([]string, error) {
		return nil, nil
	}
	return ctrl
}

func writeProvidersJSON(t *testing.T, configDir string, data map[string]any) {
	t.Helper()
	p := filepath.Join(configDir, "providers.json")
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
}

func readProvidersJSON(t *testing.T, configDir string) map[string]any {
	t.Helper()
	p := filepath.Join(configDir, "providers.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	var data map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	return data
}

func getGroup(groups []any, id string) map[string]any {
	for _, g := range groups {
		if gm, ok := g.(map[string]any); ok {
			if gm["id"] == id {
				return gm
			}
		}
	}
	return nil
}

// cloneEntries deep-copies a ledger group map for tests that hand-build plans.
func cloneEntries(in map[string]*groupLedgerEntry) map[string]*groupLedgerEntry {
	out := make(map[string]*groupLedgerEntry, len(in))
	for id, e := range in {
		out[id] = cloneEntry(e)
	}
	return out
}

// 1. Normal recovery test
func TestNormalRecovery(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"providers": []any{
			map[string]any{"id": "prov1", "name": "Provider 1"},
		},
		"groups": []any{
			map[string]any{
				"id":      "normal-grp",
				"name":    "Normal Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()

	// Sync with vendor/m1 blocked
	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if !status.Enabled || status.ManagedGroups != 1 {
		t.Fatalf("expected Enabled=true, ManagedGroups=1, got %+v", status)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "normal-grp")
	off := toStringSlice(grp["off"])
	if !reflect.DeepEqual(off, []string{"vendor/m1"}) {
		t.Fatalf("expected off [vendor/m1], got %v", off)
	}

	// Recover vendor/m1
	status, err = ctrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("sync recovery failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0, got %d", status.ManagedGroups)
	}

	data = readProvidersJSON(t, configDir)
	groups = data["groups"].([]any)
	grp = getGroup(groups, "normal-grp")
	if _, hasOff := grp["off"]; hasOff {
		t.Fatalf("expected 'off' field to be removed on recovery, but was %v", grp["off"])
	}
}

// 2. Auto derived group materialization and recovery test
func TestAutoDerivedRecovery(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"providers": []any{map[string]any{"id": "p1"}},
		"groups":    []any{},
	}
	writeProvidersJSON(t, configDir, initial)

	// Mock discovery and member fetching
	ctrl.discoverAuto = func(ctx context.Context, magpiePath string) ([]autoGroupSummary, error) {
		return []autoGroupSummary{
			{
				Ref:     "auto/claude-sonnet-4-5",
				Name:    "Claude Sonnet 4.5",
				Routing: "smart",
			},
		}, nil
	}
	ctrl.fetchMembers = func(ctx context.Context, magpiePath, ref string) ([]string, error) {
		return []string{"anthropic/claude-sonnet-4-5", "bedrock/claude-sonnet-4-5"}, nil
	}

	ctx := context.Background()

	// Sync with anthropic/claude-sonnet-4-5 blocked
	status, err := ctrl.sync(ctx, []string{"anthropic/claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "auto-claude-sonnet-4-5")
	if grp == nil {
		t.Fatalf("expected materialized group auto-claude-sonnet-4-5 not found")
	}
	members := toStringSlice(grp["members"])
	expectedMembers := []string{"anthropic/claude-sonnet-4-5", "bedrock/claude-sonnet-4-5"}
	if !reflect.DeepEqual(members, expectedMembers) {
		t.Fatalf("expected members %v, got %v", expectedMembers, members)
	}
	off := toStringSlice(grp["off"])
	if !reflect.DeepEqual(off, []string{"anthropic/claude-sonnet-4-5"}) {
		t.Fatalf("expected off [anthropic/claude-sonnet-4-5], got %v", off)
	}

	// Recover
	status, err = ctrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("recovery sync failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0, got %d", status.ManagedGroups)
	}

	data = readProvidersJSON(t, configDir)
	groups, _ = data["groups"].([]any)
	if grp := getGroup(groups, "auto-claude-sonnet-4-5"); grp != nil {
		t.Fatalf("expected auto group to be deleted to return to derived mode, but found: %v", grp)
	}
}

// 3. Nested child group sync test
func TestNested(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "parent-grp",
				"name":    "Parent Group",
				"members": []any{"group/child-grp", "vendor/m-fallback"},
				"routing": "order",
			},
			map[string]any{
				"id":      "child-grp",
				"name":    "Child Group",
				"members": []any{"vendor/c1", "vendor/c2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{"vendor/c1"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)

	parent := getGroup(groups, "parent-grp")
	if parentOff := toStringSlice(parent["off"]); len(parentOff) > 0 {
		t.Fatalf("parent group should not have off entries, got %v", parentOff)
	}

	child := getGroup(groups, "child-grp")
	childOff := toStringSlice(child["off"])
	if !reflect.DeepEqual(childOff, []string{"vendor/c1"}) {
		t.Fatalf("child group expected off [vendor/c1], got %v", childOff)
	}
}

// 4. Manual routing pick failover test
func TestManual(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "manual-grp",
				"name":    "Manual Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "manual",
				"pick":    "vendor/m1",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()

	// vendor/m1 is picked, but gets blocked
	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "manual-grp")
	if grp["pick"] != "vendor/m2" {
		t.Fatalf("expected pick switched to vendor/m2, got %v", grp["pick"])
	}
	if grp["routing"] != "manual" {
		t.Fatalf("expected routing manual, got %v", grp["routing"])
	}

	// Recovery restores original pick vendor/m1
	status, err = ctrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	data = readProvidersJSON(t, configDir)
	groups = data["groups"].([]any)
	grp = getGroup(groups, "manual-grp")
	if grp["pick"] != "vendor/m1" {
		t.Fatalf("expected pick restored to vendor/m1, got %v", grp["pick"])
	}
}

// 5. Pattern matching test
func TestPattern(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "pattern-grp",
				"name":    "Pattern Group",
				"members": []any{"vendor/fixed"},
				"match":   []any{"openrouter/*:free", "re:custom/.*-flash"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{"openrouter/gemini-2.5-flash:free", "custom/deepseek-flash"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "pattern-grp")
	off := toStringSlice(grp["off"])
	sort.Strings(off)
	expected := []string{"custom/deepseek-flash", "openrouter/gemini-2.5-flash:free"}
	sort.Strings(expected)
	if !reflect.DeepEqual(off, expected) {
		t.Fatalf("expected off %v, got %v", expected, off)
	}
}

// 6. All-blocked fail-closed test
func TestAllBlocked(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "manual-all-blocked",
				"name":    "Manual All Blocked",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "manual",
				"pick":    "vendor/m1",
			},
			map[string]any{
				"id":      "normal-all-blocked",
				"name":    "Normal All Blocked",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{"vendor/m1", "vendor/m2"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if status.ManagedGroups != 2 {
		t.Fatalf("expected ManagedGroups=2, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)

	// Manual group should fail closed: routing switched to order, pick cleared, all off
	manualGrp := getGroup(groups, "manual-all-blocked")
	if manualGrp["routing"] != "order" {
		t.Fatalf("expected manual group routing switched to order, got %v", manualGrp["routing"])
	}
	if _, hasPick := manualGrp["pick"]; hasPick {
		t.Fatalf("expected pick removed, got %v", manualGrp["pick"])
	}
	off := toStringSlice(manualGrp["off"])
	if len(off) != 2 {
		t.Fatalf("expected 2 off entries, got %v", off)
	}

	// Normal group: all off
	normalGrp := getGroup(groups, "normal-all-blocked")
	normalOff := toStringSlice(normalGrp["off"])
	if len(normalOff) != 2 {
		t.Fatalf("expected 2 off entries, got %v", normalOff)
	}
}

// 7. User off preservation test
func TestUserOff(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "user-off-grp",
				"name":    "User Off Group",
				"members": []any{"user-disabled", "vendor/m1", "vendor/m2"},
				"off":     []any{"user-disabled"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	// Block vendor/m1
	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "user-off-grp")
	off := toStringSlice(grp["off"])
	sort.Strings(off)
	if !reflect.DeepEqual(off, []string{"user-disabled", "vendor/m1"}) {
		t.Fatalf("expected off [user-disabled, vendor/m1], got %v", off)
	}

	// Recover vendor/m1
	status, err = ctrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("sync recovery failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0, got %d", status.ManagedGroups)
	}

	data = readProvidersJSON(t, configDir)
	groups = data["groups"].([]any)
	grp = getGroup(groups, "user-off-grp")
	off = toStringSlice(grp["off"])
	// User pre-existing off MUST remain!
	if !reflect.DeepEqual(off, []string{"user-disabled"}) {
		t.Fatalf("expected user-disabled to be preserved in off, got %v", off)
	}
}

// 8. External edits: non-managed preserved; managed field conflicts reported
func TestExternalEdit(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "edit-grp",
				"name":    "Original Name",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	_, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Part A: User modifies a non-managed field (name and context)
	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "edit-grp")
	grp["name"] = "Renamed By User"
	grp["context"] = 128000
	writeProvidersJSON(t, configDir, data)

	// Sync again: non-managed edits should be preserved
	_, err = ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("sync after non-managed edit failed: %v", err)
	}

	data = readProvidersJSON(t, configDir)
	groups = data["groups"].([]any)
	grp = getGroup(groups, "edit-grp")
	if grp["name"] != "Renamed By User" {
		t.Fatalf("expected name to be preserved, got %v", grp["name"])
	}

	// Part B: User modifies a managed field externally (off)
	grp["off"] = []any{"vendor/m1", "user-added-off"}
	writeProvidersJSON(t, configDir, data)

	// Sync again: MUST detect conflict on managed field and NOT overwrite it
	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err == nil {
		t.Fatalf("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "edit-grp") || !strings.Contains(err.Error(), "off") {
		t.Fatalf("expected error mentioning group id edit-grp and field off, got %v", err)
	}
	if status.Error == "" {
		t.Fatalf("expected status.Error to be populated")
	}
}

// 8b. An external conflict must not pin a group after the guard no longer
// needs to isolate anything in it; deleting or pausing the target has to be a
// working recovery path.
func TestExternalConflictReleasedWhenNoLongerBlocked(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "conflict-grp",
				"name":    "Conflict Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	if _, err := ctrl.sync(ctx, []string{"vendor/m1"}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	data := readProvidersJSON(t, configDir)
	grp := getGroup(data["groups"].([]any), "conflict-grp")
	grp["off"] = []any{"vendor/m1", "user-added-off"}
	writeProvidersJSON(t, configDir, data)

	if _, err := ctrl.sync(ctx, []string{"vendor/m1"}); err == nil {
		t.Fatalf("expected conflict while the model is still blocked")
	}

	// Target removed/paused: the guard has nothing left to isolate, so the
	// external edit wins and the group is released.
	status, err := ctrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("release after conflict failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0 after release, got %d", status.ManagedGroups)
	}

	grp = getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "conflict-grp")
	if off := toStringSlice(grp["off"]); !reflect.DeepEqual(off, []string{"vendor/m1", "user-added-off"}) {
		t.Fatalf("released group must keep the external off edit, got %v", off)
	}

	var ledger groupLedger
	if _, err := store.getKV(groupRoutingLedgerKey, &ledger); err != nil {
		t.Fatalf("getKV failed: %v", err)
	}
	if _, stillManaged := ledger.Groups["conflict-grp"]; stillManaged {
		t.Fatalf("released group must leave the ledger")
	}
}

// 8c. A group whose own definition no longer reaches any blocked model must
// also be released despite an external off edit.
func TestExternalConflictReleasedWhenGroupNoLongerTouched(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "detached-grp",
				"name":    "Detached Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	if _, err := ctrl.sync(ctx, []string{"vendor/m1"}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	data := readProvidersJSON(t, configDir)
	grp := getGroup(data["groups"].([]any), "detached-grp")
	grp["off"] = []any{"vendor/m1", "user-added-off"}
	grp["members"] = []any{"vendor/m2", "vendor/m3"}
	writeProvidersJSON(t, configDir, data)

	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("release after member change failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0 after release, got %d", status.ManagedGroups)
	}

	grp = getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "detached-grp")
	if off := toStringSlice(grp["off"]); !reflect.DeepEqual(off, []string{"vendor/m1", "user-added-off"}) {
		t.Fatalf("released group must keep the external off edit, got %v", off)
	}
}

// 8d. A group deleted in Magpie must not stay counted as managed.
func TestMissingGroupDropsLedger(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "gone-grp",
				"name":    "Gone Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	if _, err := ctrl.sync(ctx, []string{"vendor/m1"}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	writeProvidersJSON(t, configDir, map[string]any{"groups": []any{}})
	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("sync after group removal failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0 for a removed group, got %d", status.ManagedGroups)
	}

	var ledger groupLedger
	if _, err := store.getKV(groupRoutingLedgerKey, &ledger); err != nil {
		t.Fatalf("getKV failed: %v", err)
	}
	if _, stillManaged := ledger.Groups["gone-grp"]; stillManaged {
		t.Fatalf("removed group must leave the ledger")
	}
}

// 8e. Materialized auto groups are guard-owned: an external off edit becomes
// the new baseline so isolation continues instead of failing forever.
func TestAutoGroupExternalEditRebases(t *testing.T) {
	cases := []struct {
		name string
		off  []any
	}{
		{name: "user added exclusion", off: []any{"vendor/m1", "user-added-off"}},
		{name: "user removed guard exclusion", off: []any{"user-added-off"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, configDir, ctrl := setupTestEnv(t)
			defer store.Close()
			writeProvidersJSON(t, configDir, map[string]any{"providers": []any{}, "groups": []any{}})

			ctrl.discoverAuto = func(ctx context.Context, magpiePath string) ([]autoGroupSummary, error) {
				return []autoGroupSummary{{Ref: "auto/test-model", Name: "Test Model", Routing: "smart"}}, nil
			}
			ctrl.fetchMembers = func(ctx context.Context, magpiePath, ref string) ([]string, error) {
				return []string{"vendor/m1", "vendor/m2"}, nil
			}

			ctx := context.Background()
			status, err := ctrl.sync(ctx, []string{"vendor/m1"})
			if err != nil || status.ManagedGroups != 1 {
				t.Fatalf("materialize sync failed: status=%+v err=%v", status, err)
			}

			data := readProvidersJSON(t, configDir)
			grp := getGroup(data["groups"].([]any), "auto-test-model")
			grp["off"] = tc.off
			// Keep the group after recovery so the user-owned exclusion is visible.
			grp["name"] = "Renamed Auto Group"
			writeProvidersJSON(t, configDir, data)

			status, err = ctrl.sync(ctx, []string{"vendor/m1"})
			if err != nil {
				t.Fatalf("auto group rebase failed: %v", err)
			}
			if status.ManagedGroups != 1 {
				t.Fatalf("expected auto group to stay managed, got %d", status.ManagedGroups)
			}

			grp = getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "auto-test-model")
			off := toStringSlice(grp["off"])
			sort.Strings(off)
			if !reflect.DeepEqual(off, []string{"user-added-off", "vendor/m1"}) {
				t.Fatalf("expected guard exclusion restored beside the user edit, got %v", off)
			}

			// Recovery must drop only the guard's exclusion.
			status, err = ctrl.sync(ctx, []string{})
			if err != nil {
				t.Fatalf("auto group recovery failed: %v", err)
			}
			if status.ManagedGroups != 0 {
				t.Fatalf("expected ManagedGroups=0 after recovery, got %d", status.ManagedGroups)
			}
			grp = getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "auto-test-model")
			if got := toStringSlice(grp["off"]); !reflect.DeepEqual(got, []string{"user-added-off"}) {
				t.Fatalf("expected user exclusion preserved after recovery, got %v", got)
			}
		})
	}
}

// 8f. A group whose off already contains a blocked model must not be released
// or removed on the first sync. The auto- prefix alone is not proof that the
// group is guard-owned; only the ledger's IsAuto may drive removal.
func TestExistingOffWithBlockedKeepsIsolation(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "auto-user-group",
				"name":    "User Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "smart",
				"off":     []any{"vendor/m1", "user-added-off"},
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("first sync must keep isolation, got %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected group to stay managed, got %d", status.ManagedGroups)
	}

	grp := getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "auto-user-group")
	if grp == nil {
		t.Fatalf("group must not be deleted")
	}
	off := toStringSlice(grp["off"])
	sort.Strings(off)
	if !reflect.DeepEqual(off, []string{"user-added-off", "vendor/m1"}) {
		t.Fatalf("expected exclusions preserved, got %v", off)
	}

	var ledger groupLedger
	if _, err := store.getKV(groupRoutingLedgerKey, &ledger); err != nil {
		t.Fatalf("getKV failed: %v", err)
	}
	entry := ledger.Groups["auto-user-group"]
	if entry == nil {
		t.Fatalf("expected group in ledger")
	}
	if entry.IsAuto {
		t.Fatalf("auto- prefix must not mark a user group as auto")
	}

	// Once nothing is blocked, the guard releases the group but must not
	// delete it: it was never proven to be guard-owned.
	status, err = ctrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("release sync failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected release, got %d managed", status.ManagedGroups)
	}
	if getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "auto-user-group") == nil {
		t.Fatalf("released user group must not be deleted")
	}
}

// 9. Restart journal crash recovery test
func TestRestartJournal(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "journal-grp",
				"name":    "Journal Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	_, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}

	// Simulate restart: new controller instance with same store and configDir
	newCtrl := newTestController(store, configDir)

	// Check status on restart
	status, err := newCtrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("sync after restart failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1 after restart, got %d", status.ManagedGroups)
	}

	// Now recover on restarted controller
	status, err = newCtrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("recovery after restart failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0 after recovery, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "journal-grp")
	if _, hasOff := grp["off"]; hasOff {
		t.Fatalf("expected off to be removed on recovery after restart, got %v", grp["off"])
	}
}

// 10. Path mismatch test
func TestPathMismatch(t *testing.T) {
	store, configDirA, ctrlA := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "g1",
				"members": []any{"m1", "m2"},
			},
		},
	}
	writeProvidersJSON(t, configDirA, initial)

	ctx := context.Background()
	_, err := ctrlA.sync(ctx, []string{"m1"})
	if err != nil {
		t.Fatalf("ctrlA sync failed: %v", err)
	}

	// Create controller pointing to different configDirB with the same store
	configDirB := filepath.Join(t.TempDir(), "magpie-b")
	os.MkdirAll(configDirB, 0o700)
	writeProvidersJSON(t, configDirB, initial)

	ctrlB := newGroupController(store, configDirB, "")
	_, err = ctrlB.sync(ctx, []string{"m1"})
	if err == nil {
		t.Fatalf("expected path mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "配置路径不匹配") {
		t.Fatalf("expected error mentioning path mismatch, got: %v", err)
	}
}

// 11. No-op test when no blocked models and no ledger
func TestNoopWhenNoBlockedAndNoLedger(t *testing.T) {
	store, _, ctrl := setupTestEnv(t)
	defer store.Close()

	// providers.json does NOT exist at all!
	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("expected no-op without error, got %v", err)
	}
	if !status.Enabled || status.ManagedGroups != 0 {
		t.Fatalf("expected Enabled=true, ManagedGroups=0, got %+v", status)
	}
}

// 12. Parse members regex test allowing 'models ' prefix on first line (archive146 CLI evidence)
func TestParseMagpieGroupMembersWithModelsPrefix(t *testing.T) {
	sample := `  Claude Sonnet 4.5  auto/claude-sonnet-4-5
  routing   smart
  stays     auto
  models    1 opencode-zen/claude-sonnet-5  OpenCode Zen · Claude Sonnet 5
            2 opencode-zen/deepseek-v4.1-flash  OpenCode Zen · DeepSeek V4.1 Flash
            3 bedrock/claude-sonnet-4.5  Amazon Bedrock`

	members := parseMagpieGroupMembers(sample)
	expected := []string{
		"opencode-zen/claude-sonnet-5",
		"opencode-zen/deepseek-v4.1-flash",
		"bedrock/claude-sonnet-4.5",
	}
	if !reflect.DeepEqual(members, expected) {
		t.Fatalf("expected members %v, got %v", expected, members)
	}

	// A single line as the CLI prints it must yield the first member too.
	first := parseMagpieGroupMembers("  models  1 opencode-zen/model-one  Name\n            2 opencode-zen/model-two")
	if !reflect.DeepEqual(first, []string{"opencode-zen/model-one", "opencode-zen/model-two"}) {
		t.Fatalf("expected both members from the first-line form, got %v", first)
	}
}

// 13. Auto group discovery failure error propagation test
func TestAutoGroupDiscoveryFailurePropagates(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	// Setup providers.json with a normal group
	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "normal-safe",
				"name":    "Normal Safe Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	// Mock discovery to fail
	ctrl.discoverAuto = func(ctx context.Context, magpiePath string) ([]autoGroupSummary, error) {
		return nil, errors.New("simulated CLI timeout / failure")
	}

	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{"vendor/m1", "auto/unreachable-model"})
	if err == nil {
		t.Fatalf("expected sync error due to discovery failure, got nil")
	}
	if !strings.Contains(err.Error(), "探测自动路由组失败") {
		t.Fatalf("expected error mentioning discovery failure, got: %v", err)
	}

	// But normal group should STILL be safely updated despite discovery failure!
	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "normal-safe")
	off := toStringSlice(grp["off"])
	if !reflect.DeepEqual(off, []string{"vendor/m1"}) {
		t.Fatalf("expected normal group off [vendor/m1] to still be saved, got %v", off)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1 for saved normal group, got %d", status.ManagedGroups)
	}
}

// 14. Missing magpiePath returns error when blocked
func TestMissingMagpiePathReturnsErrorWhenBlocked(t *testing.T) {
	store, configDir, _ := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "g1",
				"members": []any{"vendor/m1"},
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	// Controller with empty magpiePath and no injected mock
	ctrl := newGroupController(store, configDir, "")
	ctrl.magpiePath = ""
	ctrl.discoverAuto = nil

	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err == nil {
		t.Fatalf("expected error when magpiePath is missing and models blocked, got nil")
	}
	if !strings.Contains(err.Error(), "缺少 magpie 命令路径") {
		t.Fatalf("expected error mentioning missing magpie path, got: %v", err)
	}
	if status.Error == "" {
		t.Fatalf("expected status.Error to be populated")
	}
}

// 15. Manual routing: originally had NO pick -> restores omission on recovery
func TestManualOriginallyNoPickRestoresOmission(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	// Manual group WITHOUT 'pick' key
	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "manual-no-pick",
				"name":    "Manual No Pick",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "manual",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()

	// Sync with vendor/m1 blocked -> guard picks healthy vendor/m2
	status, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "manual-no-pick")
	if grp["pick"] != "vendor/m2" {
		t.Fatalf("expected guard to set pick to vendor/m2, got %v", grp["pick"])
	}

	// Recovery -> MUST delete 'pick' key and restore omission!
	status, err = ctrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("recovery sync failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0, got %d", status.ManagedGroups)
	}

	data = readProvidersJSON(t, configDir)
	groups = data["groups"].([]any)
	grp = getGroup(groups, "manual-no-pick")
	if _, hasPick := grp["pick"]; hasPick {
		t.Fatalf("expected 'pick' to be omitted on recovery, but was %v", grp["pick"])
	}
}

// 16. WAL Interruption Test 1: Crash before writing providers.json
func TestWALInterruptionBeforeProvidersWrite(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "crash-grp",
				"name":    "Crash Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	_, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	var ledger groupLedger
	ok, err := store.getKV(groupRoutingLedgerKey, &ledger)
	if !ok || err != nil {
		t.Fatalf("getKV failed: %v", err)
	}
	managed := ledger.Groups["crash-grp"]
	if managed == nil {
		t.Fatalf("expected crash-grp to be managed after block")
	}

	// Crash before a recovery write landed: the target releases the group
	// (Deleted) while the before plan still carries off=[vendor/m1]. The
	// on-disk state is the before plan, so the restart must roll back.
	ledger.Pending = &pendingLedgerTx{
		TargetGroups: map[string]*groupLedgerEntry{
			"crash-grp": {GroupID: "crash-grp", Deleted: true, Before: managed.Before},
		},
		BeforeGroups: cloneEntries(ledger.Groups),
		Timestamp:    1234567,
	}
	if err := store.setKV(groupRoutingLedgerKey, ledger); err != nil {
		t.Fatalf("setKV failed: %v", err)
	}

	newCtrl := newTestController(store, configDir)

	// Rollback keeps the committed recovery baseline: off must still be there.
	status, err := newCtrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("restart sync failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1, got %d", status.ManagedGroups)
	}
	grp := getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "crash-grp")
	if off := toStringSlice(grp["off"]); !reflect.DeepEqual(off, []string{"vendor/m1"}) {
		t.Fatalf("expected guard off to survive the interrupted recovery, got %v", off)
	}

	// Genuine recovery afterwards must work.
	status, err = newCtrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("genuine recovery failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0 after recovery, got %d", status.ManagedGroups)
	}
	grp = getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "crash-grp")
	if _, hasOff := grp["off"]; hasOff {
		t.Fatalf("expected off to be removed on recovery, got %v", grp["off"])
	}
}

// 17. WAL Interruption Test 2: Crash after writing providers.json but before clearing Pending
func TestWALInterruptionAfterProvidersWrite(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "crash-grp-2",
				"name":    "Crash Group 2",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	_, err := ctrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// The write landed: providers.json is back to the recovered state.
	writeProvidersJSON(t, configDir, initial)

	var ledger groupLedger
	_, _ = store.getKV(groupRoutingLedgerKey, &ledger)
	ledger.Pending = &pendingLedgerTx{
		TargetGroups: map[string]*groupLedgerEntry{
			"crash-grp-2": {GroupID: "crash-grp-2", Deleted: true, Before: managedFields{Routing: "order"}},
		},
		BeforeGroups: map[string]*groupLedgerEntry{
			"crash-grp-2": {GroupID: "crash-grp-2", After: managedFields{HasOff: true, Off: []string{"vendor/m1"}, Routing: "order"}},
		},
		Timestamp: 1234567,
	}
	_ = store.setKV(groupRoutingLedgerKey, ledger)

	newCtrl := newTestController(store, configDir)

	status, err := newCtrl.sync(ctx, []string{})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if status.ManagedGroups != 0 {
		t.Fatalf("expected ManagedGroups=0, got %d", status.ManagedGroups)
	}
	var after groupLedger
	_, _ = store.getKV(groupRoutingLedgerKey, &after)
	if len(after.Groups) != 0 || after.Pending != nil {
		t.Fatalf("expected committed ledger cleared and pending resolved, got groups=%d pending=%v", len(after.Groups), after.Pending != nil)
	}
}

//  18. Full recovery interrupted before the providers write must NOT clear the
//     ledger and leave a stale guard off on disk.
func TestWALRecoveryInterruptedBeforeWrite(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "recover-grp",
				"name":    "Recover Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	if _, err := ctrl.sync(ctx, []string{"vendor/m1"}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	var ledger groupLedger
	ok, err := store.getKV(groupRoutingLedgerKey, &ledger)
	if !ok || err != nil || ledger.Groups["recover-grp"] == nil {
		t.Fatalf("expected recover-grp managed, ok=%v err=%v", ok, err)
	}

	// Recovery planned (target = released group) but the write never landed:
	// Pending.TargetGroups is the full post-write plan, Pending.BeforeGroups is
	// the committed baseline (off=[vendor/m1]), and providers.json is still at
	// that baseline. The crash must roll back, never clear the ledger.
	ledger.Pending = &pendingLedgerTx{
		TargetGroups: map[string]*groupLedgerEntry{
			"recover-grp": {GroupID: "recover-grp", Deleted: true, Before: ledger.Groups["recover-grp"].Before},
		},
		BeforeGroups: cloneEntries(ledger.Groups),
		Timestamp:    42,
	}
	_ = store.setKV(groupRoutingLedgerKey, ledger)

	newCtrl := newTestController(store, configDir)
	status, err := newCtrl.sync(ctx, []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("restart sync failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ledger kept (ManagedGroups=1), got %d", status.ManagedGroups)
	}
	var kept groupLedger
	_, _ = store.getKV(groupRoutingLedgerKey, &kept)
	if kept.Pending != nil {
		t.Fatalf("expected resolved rollback to clear pending")
	}
	if kept.Groups["recover-grp"] == nil {
		t.Fatalf("expected committed ledger entry to survive the interrupted recovery")
	}
	grp := getGroup(readProvidersJSON(t, configDir)["groups"].([]any), "recover-grp")
	if off := toStringSlice(grp["off"]); !reflect.DeepEqual(off, []string{"vendor/m1"}) {
		t.Fatalf("expected stale guard off to still be tracked, got %v", off)
	}
}

// 19. Unresolvable pending transaction is kept and reported, never cleared.
func TestWALAmbiguousKeepsPendingAndConflicts(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "amb-grp",
				"name":    "Ambiguous Group",
				"members": []any{"vendor/m1", "vendor/m2"},
				"routing": "order",
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	ctx := context.Background()
	if _, err := ctrl.sync(ctx, []string{"vendor/m1"}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	var ledger groupLedger
	_, _ = store.getKV(groupRoutingLedgerKey, &ledger)
	ledger.Pending = &pendingLedgerTx{
		TargetGroups: map[string]*groupLedgerEntry{"amb-grp": {GroupID: "amb-grp", Deleted: true, Before: managedFields{Routing: "order"}}},
		BeforeGroups: map[string]*groupLedgerEntry{"amb-grp": {GroupID: "amb-grp", After: managedFields{HasOff: true, Off: []string{"vendor/m1"}, Routing: "order"}}},
		Timestamp:    7,
	}
	_ = store.setKV(groupRoutingLedgerKey, ledger)

	// On-disk state matches neither plan: a third party edited a managed field.
	data := readProvidersJSON(t, configDir)
	grp := getGroup(data["groups"].([]any), "amb-grp")
	grp["off"] = []any{"vendor/m1", "vendor/m3"}
	writeProvidersJSON(t, configDir, data)

	newCtrl := newTestController(store, configDir)
	status, err := newCtrl.sync(ctx, []string{"vendor/m1"})
	if err == nil {
		t.Fatalf("expected conflict error for ambiguous interrupted write")
	}
	if !strings.Contains(err.Error(), "待决事务") {
		t.Fatalf("expected error mentioning pending transaction, got %v", err)
	}
	if status.Error == "" {
		t.Fatalf("expected status.Error populated")
	}

	var after groupLedger
	_, _ = store.getKV(groupRoutingLedgerKey, &after)
	if after.Pending == nil {
		t.Fatalf("expected pending transaction to be preserved, not cleared")
	}
	if len(after.Groups) != 1 {
		t.Fatalf("expected committed ledger preserved, got %d groups", len(after.Groups))
	}
}

// 20. Effort-aware and model-id-safe member matching.
func TestMemberMatchingHonorsEffortAndModelColons(t *testing.T) {
	cases := []struct {
		member  string
		blocked string
		want    bool
	}{
		{"vendor/m1", "vendor/m1", true},
		{"vendor/m1:high", "vendor/m1", true},
		{"vendor/m1:high", "vendor/m1:high", true},
		{"vendor/m1:high", "vendor/m1:low", false},
		{"vendor/m1:low+high", "vendor/m1:high", false},
		{"vendor/m1:low+high", "vendor/m1:low+high", true},
		{"vendor/m1", "vendor/m1:high", false},
		// Model ids that legitimately carry a colon are not effort suffixes.
		{"openrouter/deepseek-r1:free", "openrouter/deepseek-r1", false},
		{"openrouter/deepseek-r1:free", "openrouter/deepseek-r1:free", true},
		{"ollama/qwen:7b", "ollama/qwen", false},
		{"ollama/qwen:7b", "ollama/qwen:7b", true},
		// Nested groups never match a concrete model.
		{"group/child", "vendor/m1", false},
	}
	for _, tc := range cases {
		if got := memberMatchesBlocked(tc.member, tc.blocked); got != tc.want {
			t.Errorf("memberMatchesBlocked(%q, %q) = %v, want %v", tc.member, tc.blocked, got, tc.want)
		}
	}
}

// 21. re: patterns match the whole id, never a substring.
func TestPatternMatchesWholeID(t *testing.T) {
	cases := []struct {
		pattern string
		model   string
		want    bool
	}{
		{"re:vendor/m1", "vendor/m1", true},
		{"re:vendor/m1", "xvendor/m1", false},
		{"re:vendor/m1", "vendor/m1-extra", false},
		{"re:vendor/.*", "VENDOR/m2", true},
		{"re:(?-i)vendor/M2", "VENDOR/M2", false},
		{"re:(?-i)vendor/M2", "vendor/M2", true},
		{"openrouter/*:free", "openrouter/gemini-2.5-flash:free", true},
		{"openrouter/*:free", "openrouter/gemini-2.5-flash", false},
	}
	for _, tc := range cases {
		if got := patternMatches(tc.pattern, tc.model); got != tc.want {
			t.Errorf("patternMatches(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

// 22. CLI environment test for XDG_CONFIG_HOME
func TestCLIEnvPassesXDGConfigHome(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	env := ctrl.buildCLIEnv()

	expectedXDG := filepath.Dir(configDir)
	foundXDG := false
	for _, e := range env {
		if strings.HasPrefix(e, "XDG_CONFIG_HOME=") {
			val := strings.TrimPrefix(e, "XDG_CONFIG_HOME=")
			if val != expectedXDG {
				t.Fatalf("expected XDG_CONFIG_HOME=%s, got %s", expectedXDG, val)
			}
			foundXDG = true
		}
	}
	if !foundXDG {
		t.Fatalf("expected XDG_CONFIG_HOME=%s in buildCLIEnv, not found in %v", expectedXDG, env)
	}

	// The caller's environment is inherited: nothing unrelated is stripped,
	// and MAGPIE_MODEL_GUARD_DISCOVERY=1 is set for CLI sub-processes.
	foundDiscovery := false
	for _, e := range env {
		if e == "MAGPIE_MODEL_GUARD_DISCOVERY=1" {
			foundDiscovery = true
			break
		}
	}
	if !foundDiscovery {
		t.Fatalf("expected MAGPIE_MODEL_GUARD_DISCOVERY=1 in buildCLIEnv, not found in %v", env)
	}

	// A config dir that cannot be mapped to an XDG home must not probe.
	odd := newGroupController(store, filepath.Join(t.TempDir(), "custom-config"), "")
	odd.discoverAuto = nil
	odd.magpiePath = "/bin/false"
	if _, ok := odd.cliXDGHome(); ok {
		t.Fatalf("expected no XDG mapping for a custom config dir")
	}
	if _, err := odd.discoverAutoGroups(context.Background()); err == nil {
		t.Fatalf("expected discovery to refuse an unmappable config dir")
	}
}

// 24. discover/fetch callbacks can independently acquire sidecar lock (lock held area excludes CLI)
func TestDiscoverFetchCanAcquireSidecarLock(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "grp1",
				"members": []any{"vendor/m1", "vendor/m2"},
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	discoverLockAcquired := false
	fetchLockAcquired := false

	ctrl.discoverAuto = func(ctx context.Context, magpiePath string) ([]autoGroupSummary, error) {
		// Attempt to acquire the sidecar lock from an independent flock during discovery
		lock, err := acquireSidecarLock(ctx, configDir)
		if err != nil {
			t.Errorf("expected to acquire sidecar lock during discoverAuto, got: %v", err)
		} else {
			discoverLockAcquired = true
			_ = lock.Unlock()
		}
		return []autoGroupSummary{
			{
				Ref:     "auto/claude-sonnet-4-5",
				Name:    "Claude Sonnet 4.5",
				Routing: "smart",
			},
		}, nil
	}

	ctrl.fetchMembers = func(ctx context.Context, magpiePath, ref string) ([]string, error) {
		// Attempt to acquire the sidecar lock from an independent flock during member fetch
		lock, err := acquireSidecarLock(ctx, configDir)
		if err != nil {
			t.Errorf("expected to acquire sidecar lock during fetchMembers, got: %v", err)
		} else {
			fetchLockAcquired = true
			_ = lock.Unlock()
		}
		return []string{"anthropic/claude-sonnet-4-5"}, nil
	}

	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{"anthropic/claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if !discoverLockAcquired {
		t.Fatalf("discoverAuto did not acquire sidecar lock")
	}
	if !fetchLockAcquired {
		t.Fatalf("fetchMembers did not acquire sidecar lock")
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected 1 managed group, got %d", status.ManagedGroups)
	}
}

// 25. Lock contention timeout gives actionable error and next sync recovers after lock release
func TestLockContentionReleaseAndRecovery(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "grp1",
				"members": []any{"vendor/m1", "vendor/m2"},
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	// Hold the sidecar lock with an external flock
	lockPath := filepath.Join(configDir, ".model-guard-routing.lock")
	externalLock := flock.New(lockPath)
	locked, err := externalLock.TryLock()
	if err != nil || !locked {
		t.Fatalf("failed to acquire external lock: %v", err)
	}

	// sync with short context timeout to verify lock contention failure
	tCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	status, err := ctrl.sync(tCtx, []string{"vendor/m1"})
	if err == nil {
		t.Fatalf("expected sync to fail when sidecar lock is held, got nil")
	}
	if !strings.Contains(err.Error(), "路由侧车锁") {
		t.Fatalf("expected lock error mentioning 路由侧车锁, got: %v", err)
	}
	if strings.Contains(err.Error(), "配置冲突") {
		t.Fatalf("lock timeout must not report config conflict, got: %v", err)
	}
	if status.Error == "" {
		t.Fatalf("expected status.Error to be populated")
	}

	// Release the external lock
	_ = externalLock.Unlock()

	// Next sync call should succeed and recover/reconcile properly
	status, err = ctrl.sync(context.Background(), []string{"vendor/m1"})
	if err != nil {
		t.Fatalf("sync after lock release failed: %v", err)
	}
	if status.ManagedGroups != 1 {
		t.Fatalf("expected ManagedGroups=1 after lock release, got %d", status.ManagedGroups)
	}

	data := readProvidersJSON(t, configDir)
	groups := data["groups"].([]any)
	grp := getGroup(groups, "grp1")
	off := toStringSlice(grp["off"])
	if !reflect.DeepEqual(off, []string{"vendor/m1"}) {
		t.Fatalf("expected off [vendor/m1], got %v", off)
	}
}

// 26. Concurrent ledger changes made before lock acquisition are respected and not overwritten
func TestConcurrentLedgerChangeRespected(t *testing.T) {
	store, configDir, ctrl := setupTestEnv(t)
	defer store.Close()

	initial := map[string]any{
		"groups": []any{
			map[string]any{
				"id":      "normal-grp",
				"members": []any{"vendor/m1", "vendor/m2"},
			},
			map[string]any{
				"id":      "concurrent-grp",
				"members": []any{"other/m1", "other/m2"},
				"off":     []any{"other/m2", "other/m1"},
			},
		},
	}
	writeProvidersJSON(t, configDir, initial)

	absProvidersPath, _ := filepath.Abs(filepath.Join(configDir, "providers.json"))

	// Hook into discoverAuto to simulate a concurrent process updating the ledger
	// before ctrl acquires the file lock.
	ctrl.discoverAuto = func(ctx context.Context, magpiePath string) ([]autoGroupSummary, error) {
		concurrentLedger := groupLedger{
			ProvidersPath: absProvidersPath,
			Groups: map[string]*groupLedgerEntry{
				"concurrent-grp": {
					GroupID: "concurrent-grp",
					Before:  managedFields{HasOff: true, Off: []string{"other/m2"}},
					After:   managedFields{HasOff: true, Off: []string{"other/m2", "other/m1"}},
				},
			},
		}
		if err := store.setKV(groupRoutingLedgerKey, concurrentLedger); err != nil {
			t.Fatalf("failed to set concurrent ledger: %v", err)
		}
		return nil, nil
	}

	ctx := context.Background()
	status, err := ctrl.sync(ctx, []string{"vendor/m1", "other/m1"})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	var savedLedger groupLedger
	hasLedger, err := store.getKV(groupRoutingLedgerKey, &savedLedger)
	if err != nil || !hasLedger {
		t.Fatalf("failed to read ledger: %v", err)
	}

	if _, exists := savedLedger.Groups["concurrent-grp"]; !exists {
		t.Fatalf("expected concurrent-grp to be preserved in ledger, but was lost! Groups: %+v", savedLedger.Groups)
	}
	if _, exists := savedLedger.Groups["normal-grp"]; !exists {
		t.Fatalf("expected normal-grp to be managed in ledger, but was missing! Groups: %+v", savedLedger.Groups)
	}
	if status.ManagedGroups != 2 {
		t.Fatalf("expected 2 managed groups (normal-grp + concurrent-grp), got %d", status.ManagedGroups)
	}

	// Releasing the concurrent exclusion must retain its user's original off entry.
	ctrl.discoverAuto = func(context.Context, string) ([]autoGroupSummary, error) { return nil, nil }
	if _, err := ctrl.sync(ctx, []string{"vendor/m1"}); err != nil {
		t.Fatal(err)
	}
	live := readProvidersJSON(t, configDir)
	other := getGroup(live["groups"].([]any), "concurrent-grp")
	if got := toStringSlice(other["off"]); !reflect.DeepEqual(got, []string{"other/m2"}) {
		t.Fatalf("concurrent ownership baseline lost during recovery: %v", got)
	}
}
