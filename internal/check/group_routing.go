package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const groupRoutingLedgerKey = "group_routing_ledger"

// GroupRoutingStatus reports the synchronization state of Magpie routing groups.
type GroupRoutingStatus struct {
	Enabled       bool   `json:"enabled"`
	ManagedGroups int    `json:"managed_groups"`
	Error         string `json:"error,omitempty"`
}

type groupController struct {
	mu         sync.Mutex
	store      *Store
	configDir  string
	magpiePath string

	// Injectable hooks for tests to avoid relying on real CLI.
	discoverAuto func(ctx context.Context, magpiePath string) ([]autoGroupSummary, error)
	fetchMembers func(ctx context.Context, magpiePath, ref string) ([]string, error)
}

func newGroupController(store *Store, configDir, magpiePath string) *groupController {
	c := &groupController{
		store:      store,
		configDir:  configDir,
		magpiePath: magpiePath,
	}

	if c.magpiePath == "" {
		if p, err := exec.LookPath("magpie"); err == nil {
			c.magpiePath = p
		} else if home, err := os.UserHomeDir(); err == nil {
			candidate := filepath.Join(home, ".local", "bin", "magpie")
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				c.magpiePath = candidate
			} else {
				macCandidate := "/Applications/magpie.app/Contents/MacOS/magpie"
				if fi, err := os.Stat(macCandidate); err == nil && !fi.IsDir() {
					c.magpiePath = macCandidate
				}
			}
		}
	}

	return c
}

type groupLedger struct {
	ProvidersPath string                       `json:"providers_path"`
	Groups        map[string]*groupLedgerEntry `json:"groups"` // Committed active groups
	Pending       *pendingLedgerTx             `json:"pending,omitempty"`
}

// pendingLedgerTx is the write-ahead record of one providers.json commit.
// TargetGroups is the full post-write managed state (superset of Groups,
// including entries about to be released with Deleted set); BeforeGroups is
// the pre-write managed state, so a crash can be resolved in either
// direction and never clears the ledger on a guess.
type pendingLedgerTx struct {
	TargetGroups map[string]*groupLedgerEntry `json:"target_groups"`
	BeforeGroups map[string]*groupLedgerEntry `json:"before_groups"`
	Timestamp    int64                        `json:"timestamp"`
}

type groupLedgerEntry struct {
	GroupID string `json:"group_id"`
	IsAuto  bool   `json:"is_auto"`
	// Deleted marks a released group: the plugin no longer manages it, so the
	// plan expects the group's own (baseline) managed fields. Removed means
	// the group is dropped from the list altogether (a materialized auto
	// group returning to derived mode).
	Deleted      bool            `json:"deleted,omitempty"`
	Removed      bool            `json:"removed,omitempty"`
	OriginalJSON json.RawMessage `json:"original_json,omitempty"`
	Before       managedFields   `json:"before"`
	After        managedFields   `json:"after"`
	AddedOff     []string        `json:"added_off,omitempty"`
}

type managedFields struct {
	HasOff  bool     `json:"has_off"`
	Off     []string `json:"off,omitempty"`
	Routing string   `json:"routing,omitempty"`
	HasPick bool     `json:"has_pick"`
	Pick    string   `json:"pick,omitempty"`
}

type autoGroupSummary struct {
	Ref     string
	Name    string
	Routing string
	Summary string
}

func (c *groupController) sync(ctx context.Context, blocked []string) (GroupRoutingStatus, error) {
	status := GroupRoutingStatus{
		Enabled: c.configDir != "",
	}
	if !status.Enabled {
		return status, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	providersPath := filepath.Clean(filepath.Join(c.configDir, "providers.json"))
	absProvidersPath, _ := filepath.Abs(providersPath)

	var preLedger groupLedger
	hasPreLedger, err := c.store.getKV(groupRoutingLedgerKey, &preLedger)
	if err != nil {
		status.Error = fmt.Sprintf("读取路由账本失败: %v", err)
		return status, err
	}
	if hasPreLedger && preLedger.ProvidersPath != "" && preLedger.ProvidersPath != absProvidersPath && len(preLedger.Groups) > 0 {
		err := fmt.Errorf("配置路径不匹配: ledger路径 %s 与当前路径 %s 不一致", preLedger.ProvidersPath, absProvidersPath)
		status.Error = err.Error()
		return status, err
	}

	// Optimization: If no targets are blocked and no managed ledger entries exist (and no pending WAL), no-op.
	if len(blocked) == 0 && (!hasPreLedger || len(preLedger.Groups) == 0) && preLedger.Pending == nil {
		return status, nil
	}

	// If blocked or ledger entries exist, providers.json MUST exist and be readable.
	if _, statErr := os.Stat(providersPath); statErr != nil {
		err := fmt.Errorf("providers.json 不可读: %w", statErr)
		status.Error = err.Error()
		return status, err
	}

	// Outside file lock: collect auto groups catalog via CLI when models are blocked.
	var (
		discoveredCatalog []autoGroupCatalogItem
		autoErrs          []error
	)
	if len(blocked) > 0 {
		// Leave time in the service deadline for the short commit critical section.
		catalogCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		discoveredCatalog, autoErrs = c.collectAutoGroupsCatalog(catalogCtx, providersPath)
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return status, fmt.Errorf("路由信息读取已取消或超时，尚未尝试获取侧车锁: %w", err)
	}

	fileLock, lockErr := acquireSidecarLock(ctx, c.configDir)
	if lockErr != nil {
		status.Error = fmt.Sprintf("获取路由侧车锁失败: %v", lockErr)
		return status, lockErr
	}
	defer fileLock.Unlock()

	// Inside lock: re-read ledger from SQLite so concurrent commits are respected.
	var ledger groupLedger
	hasLedger, err := c.store.getKV(groupRoutingLedgerKey, &ledger)
	if err != nil {
		status.Error = fmt.Sprintf("读取路由账本失败: %v", err)
		return status, err
	}
	if !hasLedger || ledger.Groups == nil {
		ledger = groupLedger{
			ProvidersPath: absProvidersPath,
			Groups:        make(map[string]*groupLedgerEntry),
		}
	} else if ledger.ProvidersPath != "" && ledger.ProvidersPath != absProvidersPath && len(ledger.Groups) > 0 {
		err := fmt.Errorf("配置路径不匹配: ledger路径 %s 与当前路径 %s 不一致", ledger.ProvidersPath, absProvidersPath)
		status.Error = err.Error()
		return status, err
	}
	ledger.ProvidersPath = absProvidersPath

	origBytes, err := os.ReadFile(providersPath)
	if err != nil {
		status.Error = fmt.Sprintf("读取 providers.json 失败: %v", err)
		return status, err
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(origBytes, &root); err != nil {
		status.Error = fmt.Sprintf("解析 providers.json 失败: %v", err)
		return status, err
	}

	var groups []map[string]any
	if rawGroups, ok := root["groups"]; ok && len(rawGroups) > 0 && string(rawGroups) != "null" {
		dec := json.NewDecoder(bytes.NewReader(rawGroups))
		dec.UseNumber()
		if err := dec.Decode(&groups); err != nil {
			status.Error = fmt.Sprintf("解析 groups 失败: %v", err)
			return status, err
		}
	}

	// Crash Recovery: resolve pending WAL transaction before proceeding.
	// Only a state that is exactly the pre-write baseline or exactly the
	// staged target is safe to resolve; anything else keeps the pending
	// record and reports a conflict instead of silently clearing it.
	if ledger.Pending != nil {
		live := extractManagedFromList(groups)
		target := ledger.Pending.TargetGroups
		before := ledger.Pending.BeforeGroups
		switch {
		case planSideMatchesLive(live, target, before):
			// The providers write completed before the crash: commit the target.
			ledger.Groups = activeGroups(target)
			ledger.Pending = nil
			if err := c.store.setKV(groupRoutingLedgerKey, ledger); err != nil {
				status.Error = fmt.Sprintf("提交中断恢复事务失败: %v", err)
				return status, err
			}
		case planSideMatchesLive(live, before, target):
			// The providers write never landed: roll back to the baseline.
			ledger.Pending = nil
			if err := c.store.setKV(groupRoutingLedgerKey, ledger); err != nil {
				status.Error = fmt.Sprintf("回滚中断恢复事务失败: %v", err)
				return status, err
			}
		default:
			status.ManagedGroups = len(ledger.Groups)
			err := fmt.Errorf("上次路由写入中断且 providers.json 既非提交前也非提交后状态，保留待决事务并等待人工确认")
			status.Error = err.Error()
			return status, err
		}
	}

	groupMap := make(map[string]map[string]any)
	for _, g := range groups {
		if id, ok := g["id"].(string); ok && id != "" {
			groupMap[id] = g
		}
	}

	// Restart / journal validation: check for external edits on managed fields.
	var conflictErrors []string
	conflictedGroups := make(map[string]bool)

	for id, entry := range ledger.Groups {
		g, exists := groupMap[id]
		if !exists {
			continue
		}
		live := extractManagedFields(g)
		// Compare live against committed After and baseline Before
		if !managedFieldsEqual(live, entry.After) {
			if !managedFieldsEqual(live, entry.Before) {
				field := diffManagedField(live, entry.After)
				conflictedGroups[id] = true
				conflictErrors = append(conflictErrors, fmt.Sprintf("组 %s 的受管字段 %s 被外部修改", id, field))
			} else {
				// State is at Before (crash prior to previous write), rebase After to Before.
				entry.After = entry.Before
			}
		}
	}

	// Copy committed groups into staged targetGroups. Entries released during
	// this pass stay in the map with Deleted set so the plan still describes
	// the expected post-write state for every group this plugin touched.
	targetGroups := make(map[string]*groupLedgerEntry, len(ledger.Groups))
	beforePlan := make(map[string]*groupLedgerEntry, len(ledger.Groups))
	for id, entry := range ledger.Groups {
		clone := cloneEntry(entry)
		clone.Deleted = false
		targetGroups[id] = clone
		base := cloneEntry(entry)
		base.After = clone.Before
		base.Deleted = false
		beforePlan[id] = base
	}
	livePlan := extractManagedFromList(groups)
	for id, lf := range livePlan {
		if _, ok := beforePlan[id]; ok {
			beforePlan[id].After = lf
		}
	}

	// Materialize auto groups from pre-collected catalog if models are blocked.
	if len(blocked) > 0 {
		materializeAutoGroups(discoveredCatalog, blocked, groupMap, &groups, targetGroups, conflictedGroups)
		for id, entry := range targetGroups {
			if _, ok := beforePlan[id]; !ok {
				beforePlan[id] = &groupLedgerEntry{GroupID: id, After: entry.Before, Removed: true}
			}
		}
	}

	changed := false
	for _, g := range groups {
		id, _ := g["id"].(string)
		if id == "" || conflictedGroups[id] {
			continue
		}

		entry, isManaged := targetGroups[id]
		if !isManaged {
			// Never take over a group the guard has no business in: only a
			// group a blocked model can actually reach is brought in.
			if !groupTouchedByBlocked(g, blocked) {
				continue
			}
			raw, _ := json.Marshal(g)
			entry = &groupLedgerEntry{
				GroupID:      id,
				OriginalJSON: raw,
				Before:       extractManagedFields(g),
			}
		}

		if reconcileGroup(g, entry, blocked) {
			changed = true
		}

		wasReleased := entry.Deleted
		if isGroupRecovered(g, entry) {
			entry.After = extractManagedFields(g)
			entry.Deleted = true
			entry.Removed = false
			if entry.IsAuto && !isUserModified(g, entry) {
				// Stop pointing agents at a guard-made group: drop it so the
				// auto group is derived from the catalog again.
				delete(groupMap, id)
				entry.Removed = true
			}
		} else {
			entry.After = extractManagedFields(g)
			entry.Deleted = false
			entry.Removed = false
		}
		if wasReleased != entry.Deleted || entry.Removed {
			changed = true
		}
		targetGroups[id] = entry
	}

	// Filter out removed groups (e.g. recovered auto groups).
	if changed {
		var newGroups []map[string]any
		for _, g := range groups {
			id, _ := g["id"].(string)
			if _, exists := groupMap[id]; exists {
				newGroups = append(newGroups, g)
			}
		}
		groups = newGroups
	}

	status.ManagedGroups = len(activeGroups(targetGroups))

	// A providers write is needed only when the staged plan really differs
	// from what is on disk now (recovery may already be in effect).
	needWrite := changed && !managedFieldsEqualAll(livePlan, targetGroups)

	if needWrite {
		// WAL Step 1: Stage the pending transaction (pre-write and target
		// plans) in SQLite. The committed ledger.Groups is left untouched so
		// a crash can never lose the recovery baseline.
		ledger.Pending = &pendingLedgerTx{
			TargetGroups: targetGroups,
			BeforeGroups: beforePlan,
			Timestamp:    time.Now().Unix(),
		}
		if err := c.store.setKV(groupRoutingLedgerKey, ledger); err != nil {
			status.Error = fmt.Sprintf("记录待决事务失败: %v", err)
			return status, err
		}

		// WAL Step 2: Optimistic concurrency check: re-read providers.json to ensure unchanged
		freshBytes, err := os.ReadFile(providersPath)
		if err != nil {
			ledger.Pending = nil
			_ = c.store.setKV(groupRoutingLedgerKey, ledger)
			status.Error = fmt.Sprintf("验证 providers.json 失败: %v", err)
			return status, err
		}
		if !bytes.Equal(freshBytes, origBytes) {
			ledger.Pending = nil
			_ = c.store.setKV(groupRoutingLedgerKey, ledger)
			err := errors.New("providers.json 在同步期间被外部修改，放弃写回以避免冲突")
			status.Error = err.Error()
			return status, err
		}

		// WAL Step 3: Serialize root JSON preserving all unknown fields and credentials
		groupsJSON, err := json.Marshal(groups)
		if err != nil {
			ledger.Pending = nil
			_ = c.store.setKV(groupRoutingLedgerKey, ledger)
			status.Error = fmt.Sprintf("序列化 groups 失败: %v", err)
			return status, err
		}
		root["groups"] = json.RawMessage(groupsJSON)

		newRootJSON, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			ledger.Pending = nil
			_ = c.store.setKV(groupRoutingLedgerKey, ledger)
			status.Error = fmt.Sprintf("序列化 providers.json 失败: %v", err)
			return status, err
		}
		newRootJSON = append(newRootJSON, '\n')

		// WAL Step 4: Atomic write 0600
		if err := atomicWriteFile(providersPath, newRootJSON, 0o600); err != nil {
			ledger.Pending = nil
			_ = c.store.setKV(groupRoutingLedgerKey, ledger)
			status.Error = fmt.Sprintf("写入 providers.json 失败: %v", err)
			return status, err
		}

		// WAL Step 5: Providers write succeeded! Finalize and commit ledger.
		ledger.Groups = activeGroups(targetGroups)
		ledger.Pending = nil
		if err := c.store.setKV(groupRoutingLedgerKey, ledger); err != nil {
			status.Error = fmt.Sprintf("提交路由账本失败: %v", err)
			return status, err
		}
	} else if changed {
		// Nothing to write on disk: only ledger rows were released.
		ledger.Groups = activeGroups(targetGroups)
		ledger.Pending = nil
		if err := c.store.setKV(groupRoutingLedgerKey, ledger); err != nil {
			status.Error = fmt.Sprintf("更新路由账本失败: %v", err)
			return status, err
		}
	} else if len(ledger.Groups) == 0 {
		_ = c.store.setKV(groupRoutingLedgerKey, ledger)
	}

	var allErrors []string
	if len(conflictErrors) > 0 {
		allErrors = append(allErrors, fmt.Sprintf("外部冲突: %s，已停止对冲突组的覆盖", strings.Join(conflictErrors, "; ")))
	}
	for _, e := range autoErrs {
		allErrors = append(allErrors, e.Error())
	}

	if len(allErrors) > 0 {
		err := errors.New(strings.Join(allErrors, "; "))
		status.Error = err.Error()
		return status, err
	}

	return status, nil
}

// groupTouchedByBlocked reports whether a blocked model can reach the group:
// through a concrete member, a member pattern, a match pattern, or the
// manual pick. Nested groups are guarded where they are defined.
func groupTouchedByBlocked(g map[string]any, blocked []string) bool {
	for _, m := range toStringSlice(g["members"]) {
		if strings.HasPrefix(m, "group/") {
			continue
		}
		if strings.Contains(m, "*") || strings.HasPrefix(m, "re:") {
			for _, b := range blocked {
				if patternMatches(m, b) {
					return true
				}
			}
			continue
		}
		for _, b := range blocked {
			if memberMatchesBlocked(m, b) {
				return true
			}
		}
	}
	for _, pat := range toStringSlice(g["match"]) {
		for _, b := range blocked {
			if patternMatches(pat, b) {
				return true
			}
		}
	}
	if pick, _ := g["pick"].(string); pick != "" {
		for _, b := range blocked {
			if memberMatchesBlocked(pick, b) {
				return true
			}
		}
	}
	return false
}

func reconcileGroup(g map[string]any, entry *groupLedgerEntry, blocked []string) bool {
	changed := false

	members := toStringSlice(g["members"])
	matchPatterns := toStringSlice(g["match"])
	liveOff := toStringSlice(g["off"])
	routing, _ := g["routing"].(string)
	pick, _ := g["pick"].(string)

	var newlyBlocked []string

	// Concrete members
	for _, m := range members {
		if strings.HasPrefix(m, "group/") {
			continue // Nested child group synchronizes independently
		}
		if strings.Contains(m, "*") || strings.HasPrefix(m, "re:") {
			// Member entry is a pattern
			for _, b := range blocked {
				if patternMatches(m, b) {
					newlyBlocked = append(newlyBlocked, b)
				}
			}
			continue
		}
		for _, b := range blocked {
			if memberMatchesBlocked(m, b) {
				newlyBlocked = append(newlyBlocked, m)
				break
			}
		}
	}

	// Match patterns
	for _, pat := range matchPatterns {
		for _, b := range blocked {
			if patternMatches(pat, b) {
				newlyBlocked = append(newlyBlocked, b)
			}
		}
	}

	newlyBlocked = dedupeStrings(newlyBlocked)

	// Combine with pre-existing user off
	desiredOff := dedupeStrings(append(append([]string{}, entry.Before.Off...), newlyBlocked...))

	if !equalStringSets(liveOff, desiredOff) {
		if len(desiredOff) > 0 {
			g["off"] = desiredOff
		} else {
			if entry.Before.HasOff {
				g["off"] = []string{}
			} else {
				delete(g, "off")
			}
		}
		changed = true
	}

	entry.AddedOff = stringSliceDiff(desiredOff, entry.Before.Off)

	// Handle manual routing
	isManual := routing == "manual" || entry.Before.Routing == "manual"
	if isManual {
		var healthyMembers []string
		for _, m := range members {
			if strings.Contains(m, "*") || strings.HasPrefix(m, "re:") {
				continue
			}
			if sliceContains(desiredOff, m) {
				continue
			}
			blockedMatch := false
			for _, b := range blocked {
				if memberMatchesBlocked(m, b) {
					blockedMatch = true
					break
				}
			}
			if !blockedMatch {
				healthyMembers = append(healthyMembers, m)
			}
		}

		if len(newlyBlocked) == 0 {
			// Nothing the guard cares about is blocked: restore routing and
			// pick exactly as they were, including an originally absent pick.
			if routing != "manual" {
				g["routing"] = "manual"
				changed = true
			}
			if entry.Before.HasPick {
				if pick != entry.Before.Pick {
					g["pick"] = entry.Before.Pick
					changed = true
				}
			} else if _, hasPick := g["pick"]; hasPick {
				delete(g, "pick")
				changed = true
			}
		} else if len(healthyMembers) > 0 {
			if routing != "manual" {
				g["routing"] = "manual"
				changed = true
			}
			desiredPick := pick
			if entry.Before.Pick != "" && sliceContains(healthyMembers, entry.Before.Pick) {
				desiredPick = entry.Before.Pick
			} else if !sliceContains(healthyMembers, pick) {
				desiredPick = healthyMembers[0]
			}
			if pick != desiredPick {
				g["pick"] = desiredPick
				changed = true
			}
		} else {
			// All members blocked: fail-closed by switching away from manual to order with all-off
			if routing != "order" {
				g["routing"] = "order"
				changed = true
			}
			if _, hasPick := g["pick"]; hasPick {
				delete(g, "pick")
				changed = true
			}
		}
	}

	return changed
}

type autoGroupCatalogItem struct {
	GroupID string
	Ref     string
	Name    string
	Routing string
	Members []string
}

func (c *groupController) collectAutoGroupsCatalog(
	ctx context.Context,
	providersPath string,
) ([]autoGroupCatalogItem, []error) {
	if c.magpiePath == "" && c.discoverAuto == nil {
		return nil, []error{errors.New("缺少 magpie 命令路径，无法探测自动路由组")}
	}

	summaries, err := c.discoverAutoGroups(ctx)
	if err != nil {
		return nil, []error{fmt.Errorf("探测自动路由组失败: %w", err)}
	}
	if len(summaries) == 0 {
		return nil, nil
	}

	// Read snapshot of providers.json to optimize by skipping existing groups
	snapshotExistingIDs := make(map[string]bool)
	if snapBytes, err := os.ReadFile(providersPath); err == nil {
		var snapRoot map[string]json.RawMessage
		if err := json.Unmarshal(snapBytes, &snapRoot); err == nil {
			if rawGroups, ok := snapRoot["groups"]; ok && len(rawGroups) > 0 && string(rawGroups) != "null" {
				var snapGroups []map[string]any
				dec := json.NewDecoder(bytes.NewReader(rawGroups))
				dec.UseNumber()
				if err := dec.Decode(&snapGroups); err == nil {
					for _, g := range snapGroups {
						if id, ok := g["id"].(string); ok && id != "" {
							snapshotExistingIDs[id] = true
						}
					}
				}
			}
		}
	}

	var (
		catalog []autoGroupCatalogItem
		errs    []error
		seen    = make(map[string]bool)
	)

	for _, sum := range summaries {
		slug := autoRefSlug(sum.Ref)
		if slug == "" {
			slug = toSlug(sum.Name)
		}
		if slug == "" {
			continue
		}
		groupID := "auto-" + slug

		if seen[groupID] {
			continue
		}
		seen[groupID] = true

		if snapshotExistingIDs[groupID] {
			continue
		}

		members, err := c.getGroupMembers(ctx, sum.Ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("读取自动组 %s 成员失败: %w", sum.Ref, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if len(members) == 0 {
			continue
		}

		catalog = append(catalog, autoGroupCatalogItem{
			GroupID: groupID,
			Ref:     sum.Ref,
			Name:    sum.Name,
			Routing: sum.Routing,
			Members: members,
		})
	}
	return catalog, errs
}

func materializeAutoGroups(
	catalog []autoGroupCatalogItem,
	blocked []string,
	groupMap map[string]map[string]any,
	groups *[]map[string]any,
	targetGroups map[string]*groupLedgerEntry,
	conflictedGroups map[string]bool,
) {
	for _, item := range catalog {
		if _, exists := groupMap[item.GroupID]; exists {
			continue
		}
		if conflictedGroups[item.GroupID] {
			continue
		}

		hasBlocked := false
		for _, m := range item.Members {
			for _, b := range blocked {
				if memberMatchesBlocked(m, b) {
					hasBlocked = true
					break
				}
			}
			if hasBlocked {
				break
			}
		}

		if hasBlocked {
			r := item.Routing
			if r == "" {
				r = "smart"
			}
			matGroup := map[string]any{
				"id":      item.GroupID,
				"name":    item.Name,
				"members": item.Members,
				"routing": r,
			}

			raw, _ := json.Marshal(matGroup)
			entry := &groupLedgerEntry{
				GroupID:      item.GroupID,
				IsAuto:       true,
				OriginalJSON: raw,
				Before: managedFields{
					HasOff:  false,
					HasPick: false,
					Routing: r,
				},
			}

			*groups = append(*groups, matGroup)
			groupMap[item.GroupID] = matGroup
			targetGroups[item.GroupID] = entry
		}
	}
}

func (c *groupController) buildCLIEnv() []string {
	env := os.Environ()
	env = setEnvKV(env, "MAGPIE_MODEL_GUARD_DISCOVERY", "1")
	if c.configDir == "" {
		return env
	}
	if xdg, ok := c.cliXDGHome(); ok {
		env = setEnvKV(env, "XDG_CONFIG_HOME", xdg)
	}
	return env
}

// cliXDGHome is the XDG_CONFIG_HOME magpie itself reads for this controller's
// config dir. It is only reported when that mapping is certain, so the guard
// never asks the CLI about a different install.
func (c *groupController) cliXDGHome() (string, bool) {
	dir := filepath.Clean(c.configDir)
	if filepath.Base(dir) == "magpie" {
		return filepath.Dir(dir), true
	}
	if cur, ok := os.LookupEnv("XDG_CONFIG_HOME"); ok && cur != "" {
		if filepath.Clean(filepath.Join(cur, "magpie")) == dir {
			return filepath.Clean(cur), true
		}
	}
	return "", false
}

func setEnvKV(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = append(out, prefix+val)
			found = true
		} else {
			out = append(out, e)
		}
	}
	if !found {
		out = append(out, prefix+val)
	}
	return out
}

func (c *groupController) discoverAutoGroups(ctx context.Context) ([]autoGroupSummary, error) {
	if c.discoverAuto != nil {
		return c.discoverAuto(ctx, c.magpiePath)
	}
	if c.magpiePath == "" {
		return nil, errors.New("缺少 magpie 命令路径")
	}
	env, ok := c.cliEnvForConfig()
	if !ok {
		return nil, fmt.Errorf("无法将配置目录 %s 映射为 magpie 的 XDG_CONFIG_HOME，已跳过自动组探测", c.configDir)
	}

	tCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	cmd := exec.CommandContext(tCtx, c.magpiePath, "groups")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("magpie groups 执行超时或失败")
	}

	return parseMagpieGroupsOutput(string(out)), nil
}

func (c *groupController) getGroupMembers(ctx context.Context, ref string) ([]string, error) {
	if c.fetchMembers != nil {
		return c.fetchMembers(ctx, c.magpiePath, ref)
	}
	if c.magpiePath == "" {
		return nil, errors.New("缺少 magpie 命令路径")
	}
	env, ok := c.cliEnvForConfig()
	if !ok {
		return nil, fmt.Errorf("无法将配置目录 %s 映射为 magpie 的 XDG_CONFIG_HOME", c.configDir)
	}

	tCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(tCtx, c.magpiePath, "group", ref)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("magpie group 执行超时或失败")
	}

	return parseMagpieGroupMembers(string(out)), nil
}

// cliEnvForConfig is buildCLIEnv plus the certainty that the environment
// points at this controller's own config dir.
func (c *groupController) cliEnvForConfig() ([]string, bool) {
	if _, ok := c.cliXDGHome(); !ok {
		return nil, false
	}
	return c.buildCLIEnv(), true
}

var reTwoPlusSpaces = regexp.MustCompile(`\s{2,}`)

func parseMagpieGroupsOutput(out string) []autoGroupSummary {
	var list []autoGroupSummary
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "no routing groups") {
			continue
		}
		parts := reTwoPlusSpaces.Split(line, -1)
		if len(parts) >= 2 {
			name := strings.TrimSpace(parts[0])
			ref := strings.TrimSpace(parts[1])
			if isAutoRef(ref) {
				routing := "smart"
				if len(parts) >= 3 {
					routing = strings.TrimSpace(parts[2])
				}
				summary := ""
				if len(parts) >= 4 {
					summary = strings.TrimSpace(parts[3])
				}
				list = append(list, autoGroupSummary{
					Ref:     ref,
					Name:    name,
					Routing: routing,
					Summary: summary,
				})
			}
		}
	}
	return list
}

func isAutoRef(ref string) bool {
	return strings.HasPrefix(ref, "auto/") || strings.HasPrefix(ref, "auto-") || strings.HasPrefix(ref, "group/auto-")
}

func autoRefSlug(ref string) string {
	ref = strings.TrimPrefix(ref, "group/")
	ref = strings.TrimPrefix(ref, "auto/")
	ref = strings.TrimPrefix(ref, "auto-")
	return toSlug(ref)
}

// Regex allows optional 'models ' prefix on first member line.
var reMemberLine = regexp.MustCompile(`^\s*(?:models\s+)?(\d+)\s+([^\s]+)`)

func parseMagpieGroupMembers(out string) []string {
	var members []string
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		m := reMemberLine.FindStringSubmatch(line)
		if len(m) >= 3 {
			members = append(members, m[2])
		}
	}
	return members
}

var slugRegex = regexp.MustCompile(`[^a-z0-9]+`)

func toSlug(s string) string {
	s = strings.ToLower(s)
	s = slugRegex.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// memberEfforts are the reasoning levels a group member may be fixed at;
// upstream only treats a trailing ":<level>" as an effort suffix, so model
// ids that legitimately contain a colon (OpenRouter's ":free", Ollama's
// ":7b", Bedrock's ":0") are left alone.
var memberEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// splitMemberEffort splits "provider/model[:low+high]" into model and effort
// suffix ("" when the trailing part is part of the model's own id).
func splitMemberEffort(id string) (model, effort string) {
	i := strings.LastIndex(id, ":")
	if i <= 0 {
		return id, ""
	}
	suffix := strings.ToLower(id[i+1:])
	for _, part := range strings.Split(suffix, "+") {
		if !sliceContains(memberEfforts, part) {
			return id, ""
		}
	}
	return id[:i], suffix
}

func memberMatchesBlocked(member, blocked string) bool {
	if member == blocked {
		return true
	}
	if member == "" || blocked == "" {
		return false
	}
	if strings.HasPrefix(member, "group/") {
		return false
	}
	memberModel, memberEffort := splitMemberEffort(member)
	if memberModel == blocked {
		return true
	}
	blockedModel, blockedEffort := splitMemberEffort(blocked)
	if blockedModel == blocked {
		// Blocked model has no effort: every effort of it is blocked too.
		return memberModel == blocked
	}
	if memberModel != blockedModel {
		return false
	}
	// Same model at efforts: a blocked subset matches the wider member, but a
	// member with no effort is its own member and stays usable.
	if memberEffort == "" {
		return false
	}
	blockedParts := strings.Split(blockedEffort, "+")
	for _, part := range strings.Split(memberEffort, "+") {
		if !sliceContains(blockedParts, part) {
			return false
		}
	}
	return true
}

func patternMatches(pattern, model string) bool {
	if strings.HasPrefix(pattern, "re:") {
		regexStr := strings.TrimPrefix(pattern, "re:")
		// Upstream matches the whole id, case-insensitively unless the
		// pattern opts out with (?-i): regexp.Compile("(?i)^(?:"+re+")$").
		if re, err := regexp.Compile("(?i)^(?:" + regexStr + ")$"); err == nil {
			return re.MatchString(model)
		}
		if re, err := regexp.Compile(regexStr); err == nil {
			return re.MatchString(model)
		}
		return false
	}
	return globMatch(pattern, model)
}

func globMatch(glob, target string) bool {
	var b strings.Builder
	b.WriteString("^(?i)")
	parts := strings.Split(glob, "*")
	for i, part := range parts {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(target)
}

// extractManagedFromList maps every live group id to its managed fields.
func extractManagedFromList(groups []map[string]any) map[string]managedFields {
	out := make(map[string]managedFields, len(groups))
	for _, g := range groups {
		if id, ok := g["id"].(string); ok && id != "" {
			out[id] = extractManagedFields(g)
		}
	}
	return out
}

// managedFieldsEqualAll reports whether the on-disk state matches the staged
// plan. A managed group is present with exactly the planned fields; a
// released group carries its own baseline fields again, or is gone when the
// plan removed it from the list.
func managedFieldsEqualAll(live map[string]managedFields, plan map[string]*groupLedgerEntry) bool {
	for id, entry := range plan {
		if entry == nil {
			continue
		}
		lf, present := live[id]
		if entry.Removed {
			if present {
				return false
			}
			continue
		}
		if !present {
			return false
		}
		if entry.Deleted {
			if !managedFieldsEqual(lf, entry.Before) {
				return false
			}
			continue
		}
		if !managedFieldsEqual(lf, entry.After) {
			return false
		}
	}
	return true
}

// planSideMatchesLive compares live against one side of a staged write,
// judged over the union of both sides' keys. A managed side expects its
// fields; a released side expects the group's baseline fields (or absence
// when the side removed it).
func planSideMatchesLive(live map[string]managedFields, plan, other map[string]*groupLedgerEntry) bool {
	keys := make(map[string]bool, len(plan)+len(other))
	for id := range plan {
		keys[id] = true
	}
	for id := range other {
		keys[id] = true
	}
	for id := range keys {
		entry := plan[id]
		if entry == nil {
			// Not touched by this side: expected to keep no guard fields.
			lf, present := live[id]
			if present && (lf.HasOff || lf.HasPick || lf.Routing != "") {
				return false
			}
			continue
		}
		lf, present := live[id]
		if entry.Removed {
			if present {
				return false
			}
			continue
		}
		if !present {
			return false
		}
		if entry.Deleted {
			if !managedFieldsEqual(lf, entry.Before) {
				return false
			}
			continue
		}
		if !managedFieldsEqual(lf, entry.After) {
			return false
		}
	}
	return true
}

func isGroupRecovered(g map[string]any, entry *groupLedgerEntry) bool {
	if len(entry.AddedOff) > 0 {
		return false
	}
	live := extractManagedFields(g)
	if live.HasOff != entry.Before.HasOff {
		return false
	}
	if !equalStringSets(live.Off, entry.Before.Off) {
		return false
	}
	if live.Routing != entry.Before.Routing {
		return false
	}
	if live.HasPick != entry.Before.HasPick {
		return false
	}
	if entry.Before.HasPick && live.Pick != entry.Before.Pick {
		return false
	}
	return true
}

func isUserModified(g map[string]any, entry *groupLedgerEntry) bool {
	if len(entry.OriginalJSON) == 0 {
		return false
	}
	var orig map[string]any
	if err := json.Unmarshal(entry.OriginalJSON, &orig); err != nil {
		return false
	}

	// Compare non-managed fields
	for k, v := range g {
		if k == "off" || k == "routing" || k == "pick" {
			continue
		}
		origV, exists := orig[k]
		if !exists {
			return true // User added new custom field
		}
		if fmt.Sprintf("%v", v) != fmt.Sprintf("%v", origV) {
			return true // User modified non-managed field
		}
	}
	for k := range orig {
		if k == "off" || k == "routing" || k == "pick" {
			continue
		}
		if _, exists := g[k]; !exists {
			return true // User deleted field
		}
	}
	return false
}

func extractManagedFields(g map[string]any) managedFields {
	mf := managedFields{}
	if rawOff, ok := g["off"]; ok && rawOff != nil {
		mf.HasOff = true
		mf.Off = toStringSlice(rawOff)
	}
	if r, ok := g["routing"].(string); ok {
		mf.Routing = r
	}
	if rawPick, ok := g["pick"]; ok && rawPick != nil {
		mf.HasPick = true
		if p, ok := rawPick.(string); ok {
			mf.Pick = p
		}
	}
	return mf
}

func managedFieldsEqual(a, b managedFields) bool {
	if a.HasOff != b.HasOff {
		return false
	}
	if !equalStringSets(a.Off, b.Off) {
		return false
	}
	if a.Routing != b.Routing {
		return false
	}
	if a.HasPick != b.HasPick {
		return false
	}
	if a.Pick != b.Pick {
		return false
	}
	return true
}

func diffManagedField(live, recorded managedFields) string {
	if live.HasOff != recorded.HasOff || !equalStringSets(live.Off, recorded.Off) {
		return "off"
	}
	if live.Routing != recorded.Routing {
		return "routing"
	}
	if live.HasPick != recorded.HasPick || live.Pick != recorded.Pick {
		return "pick"
	}
	return "unknown"
}

func toStringSlice(val any) []string {
	if val == nil {
		return nil
	}
	switch v := val.(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		if m[x] == 0 {
			return false
		}
		m[x]--
	}
	return true
}

func dedupeStrings(items []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func stringSliceDiff(all, subset []string) []string {
	subMap := make(map[string]bool, len(subset))
	for _, s := range subset {
		subMap[s] = true
	}
	var diff []string
	for _, a := range all {
		if !subMap[a] {
			diff = append(diff, a)
		}
	}
	return diff
}

func sliceContains(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}

func cloneEntry(e *groupLedgerEntry) *groupLedgerEntry {
	if e == nil {
		return nil
	}
	c := *e
	c.AddedOff = append([]string{}, e.AddedOff...)
	c.Before.Off = append([]string{}, e.Before.Off...)
	c.After.Off = append([]string{}, e.After.Off...)
	return &c
}

// activeGroups drops the entries this plugin has released, leaving the
// groups that are still under management.
func activeGroups(plan map[string]*groupLedgerEntry) map[string]*groupLedgerEntry {
	out := make(map[string]*groupLedgerEntry, len(plan))
	for id, entry := range plan {
		if entry != nil && !entry.Deleted {
			out[id] = cloneEntry(entry)
		}
	}
	return out
}

func acquireSidecarLock(ctx context.Context, dir string) (*flock.Flock, error) {
	lockPath := filepath.Join(dir, ".model-guard-routing.lock")
	fileLock := flock.New(lockPath)

	tCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	locked, err := fileLock.TryLockContext(tCtx, 40*time.Millisecond)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(tCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("等待路由侧车锁超时（3s），可能有并发同步进程占用锁: %w", err)
		}
		return nil, fmt.Errorf("获取路由侧车锁失败: %w", err)
	}
	if !locked {
		return nil, errors.New("等待路由侧车锁超时（3s），可能有并发同步进程占用锁")
	}
	return fileLock, nil
}

func atomicWriteFile(filename string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(filename)
	f, err := os.CreateTemp(dir, ".tmp-atomic-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)

	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filename)
}
