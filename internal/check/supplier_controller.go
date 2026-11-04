package check

// Supplier-side protection keeps the vendor protocol bases the user saved in
// Magpie, but points them at this Guard while protection is on. Magpie keeps
// all of its native provider behavior (routing, fallbacks, headers, protocol
// translation); Guard only has to forward to the saved vendor base again.
//
// The ledger deliberately never stores a provider key, header values, or
// proxy URLs: the only raw provider fields kept are the protocol bases and
// the headers container state. Every other provider field is left untouched.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const supplierLedgerKey = "supplier_ledger"

// supplierSessionHeader is the nested header Guard adds while a provider is
// protected. Magpie expands ${sessionId} per request; the probe path uses the
// same header to identify itself without changing the native request body.
const (
	supplierSessionHeader = "X-Model-Guard-Session"
	supplierSessionValue  = "${sessionId}"
)

// supplierHeaderAbsent, supplierHeaderNull and supplierHeaderContainerObject
// record how the provider's headers container looked before takeover, so
// removing the managed leaf can restore absent, null, or {} exactly without
// managing the whole container.
const (
	supplierHeaderAbsent          = "absent"
	supplierHeaderNull            = "null"
	supplierHeaderContainerObject = "object"
)

var supplierProtocolOrder = []string{"chat", "responses", "anthropic", "gemini", "decide"}

var supplierGeminiMethod = regexp.MustCompile(`/models(?:/[^/?#]*)?(?:[?#].*)?$`)

type supplierController struct {
	mu        sync.Mutex
	store     *Store
	configDir string
	origin    string
}

func newSupplierController(store *Store, configDir, origin string) *supplierController {
	return &supplierController{store: store, configDir: configDir, origin: origin}
}

// supplierList is the JSON shape served by GET /api/suppliers.
type supplierList struct {
	Configured bool               `json:"configured"`
	Providers  []supplierProvider `json:"providers"`
	Error      string             `json:"error,omitempty"`
}

type supplierProvider struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Preset      string   `json:"preset,omitempty"`
	Protected   bool     `json:"protected"`
	Protocols   []string `json:"protocols"`
	Conflict    string   `json:"conflict,omitempty"`
	Unsupported string   `json:"unsupported,omitempty"`
}

// logSupplierEvent records one protection activity entry. A log write must
// never undo or fail a completed provider change.
func (c *supplierController) logSupplierEvent(kind, id, detail string) {
	if c.store == nil {
		return
	}
	_ = c.store.AppendSupplierEvent(&SupplierEvent{Kind: kind, ProviderID: id, Detail: detail})
}

// supplierBinding is what /upstream resolves a route token to. Base is the
// exact original provider value; the proxy path normalizes it at runtime,
// including omitting a scheme and Gemini's /v1beta, so restoring it later is
// lossless.
type supplierBinding struct {
	ProviderID     string
	Base           string
	Protocol       string
	Token          string
	Proxy          string
	AccountProxies map[string]string
	InsecureTLS    bool
}

type supplierLedger struct {
	ProvidersPath string                          `json:"providers_path,omitempty"`
	Origin        string                          `json:"origin,omitempty"`
	Entries       map[string]*supplierLedgerEntry `json:"entries,omitempty"`
	Pending       *supplierPendingTx              `json:"pending,omitempty"`
}

// supplierManagedSnapshot holds only the fields Guard controls. A missing key
// means the field itself was absent; a present raw "null" is preserved as a
// present null. The nested probe header is addressed as
// "headers.X-Model-Guard-Session" so all other headers stay untouched.
type supplierManagedSnapshot map[string]json.RawMessage

type supplierLedgerEntry struct {
	ProviderID  string                  `json:"provider_id"`
	Token       string                  `json:"token"`
	Origin      string                  `json:"origin"`
	Protected   bool                    `json:"protected"`
	InsecureTLS bool                    `json:"insecure_tls,omitempty"`
	HeaderState string                  `json:"header_state,omitempty"`
	Original    supplierManagedSnapshot `json:"original"`
	After       supplierManagedSnapshot `json:"after"`
}

type supplierPendingTx struct {
	ProviderID string              `json:"provider_id"`
	Before     supplierPendingSide `json:"before"`
	After      supplierPendingSide `json:"after"`
	Timestamp  int64               `json:"timestamp"`
}

type supplierPendingSide struct {
	Entry    *supplierLedgerEntry    `json:"entry,omitempty"`
	Snapshot supplierManagedSnapshot `json:"snapshot,omitempty"`
}

func (c *supplierController) providersPath() string {
	return filepath.Clean(filepath.Join(c.configDir, "providers.json"))
}

func (c *supplierController) loadSupplierLedger() (supplierLedger, error) {
	ledger := supplierLedger{Entries: make(map[string]*supplierLedgerEntry)}
	ok, err := c.store.getKV(supplierLedgerKey, &ledger)
	if err != nil {
		return ledger, err
	}
	if !ok {
		return ledger, nil
	}
	if ledger.Entries == nil {
		ledger.Entries = make(map[string]*supplierLedgerEntry)
	}
	if ledger.ProvidersPath != "" {
		absPath, _ := filepath.Abs(c.providersPath())
		if ledger.ProvidersPath != absPath && (len(ledger.Entries) > 0 || ledger.Pending != nil) {
			return ledger, fmt.Errorf("配置路径不匹配: supplier ledger 路径 %s 与当前路径 %s 不一致", ledger.ProvidersPath, absPath)
		}
	}
	return ledger, nil
}

func (c *supplierController) list(ctx context.Context) (supplierList, error) {
	out := supplierList{Providers: []supplierProvider{}}
	if c.configDir == "" {
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	fi, err := os.Stat(c.configDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	if !fi.IsDir() {
		return out, fmt.Errorf("Magpie 配置路径不是目录: %s", c.configDir)
	}

	fileLock, err := acquireSidecarLock(ctx, c.configDir)
	if err != nil {
		out.Error = fmt.Sprintf("获取供应商侧车锁失败: %v", err)
		return out, err
	}
	defer fileLock.Unlock()

	providersPath := c.providersPath()
	_, providers, _, err := supplierReadProvidersFile(providersPath)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		out.Configured = true
		out.Error = fmt.Sprintf("读取 providers.json 失败: %v", err)
		return out, nil
	}
	out.Configured = true

	ledger, err := c.loadSupplierLedger()
	if err != nil {
		out.Error = fmt.Sprintf("读取供应商账本失败: %v", err)
		return out, err
	}
	entries, pendingConflicts := supplierEffectiveEntries(ledger, providers)
	originURL, _, originErr := supplierParseGuardOrigin(c.origin)

	seen := make(map[string]bool, len(providers))
	for _, provider := range providers {
		id := supplierProviderID(provider)
		if id == "" {
			continue
		}
		if seen[id] {
			out.Error = joinSupplierErrors(out.Error, fmt.Sprintf("providers.json 中存在重复供应商 id: %s", id))
			continue
		}
		seen[id] = true

		entry := entries[id]
		preset := supplierRawStringField(provider, "preset")
		if reason := supplierUnsupportedPresetReason(provider); reason != "" && !(entry != nil && entry.Protected) {
			// Remote Magpie keeps its own native endpoint rewrite; it can never
			// be taken over, so list it apart from recoverable conflicts. An
			// existing takeover stays restorable until the user disables it.
			protocols, _ := supplierProtocolInfo(provider, originURL)
			out.Providers = append(out.Providers, supplierProvider{
				ID:          id,
				Name:        supplierProviderName(provider, id),
				Preset:      preset,
				Protocols:   protocols,
				Unsupported: reason,
			})
			continue
		}
		var protocols []string
		var problems []string
		if entry != nil && entry.Protected {
			// The live fields are Guard URLs by design; validating them as
			// vendor addresses would report every protected supplier as a
			// loop. The managed snapshot below is the only live check.
			protocols = supplierProtocolsFromSnapshot(entry.After)
		} else {
			protocols, problems = supplierProtocolInfo(provider, originURL)
		}
		if len(protocols) == 0 && entry == nil {
			continue
		}

		info := supplierProvider{
			ID:        id,
			Name:      supplierProviderName(provider, id),
			Preset:    preset,
			Protocols: protocols,
		}
		if entry != nil && entry.Protected {
			info.Protected = true
			if len(protocols) == 0 {
				problems = append(problems, "没有可保护的协议地址")
			}
			live := supplierSnapshotOf(provider, supplierSnapshotKeys(entry.After))
			if diff := supplierConflictDiffKeys(entry.After, live); len(diff) > 0 {
				problems = append(problems, fmt.Sprintf("受管字段 %s 被外部修改", strings.Join(diff, "、")))
			}
		}
		if pc := pendingConflicts[id]; pc != "" {
			problems = append(problems, pc)
		}
		if originErr != nil && entry == nil && len(protocols) > 0 {
			// An invalid origin only blocks new writes; report it so the
			// console can explain why protection cannot be turned on.
			problems = append(problems, "Guard origin 无效: "+originErr.Error())
		}
		info.Conflict = strings.Join(problems, "；")
		out.Providers = append(out.Providers, info)
	}
	return out, nil
}

func (c *supplierController) setProtection(ctx context.Context, id string, enabled bool) error {
	if c.configDir == "" {
		return errors.New("未配置 Magpie 配置目录，无法修改供应商保护")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("供应商 id 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	fileLock, err := acquireSidecarLock(ctx, c.configDir)
	if err != nil {
		return fmt.Errorf("获取供应商侧车锁失败: %w", err)
	}
	defer fileLock.Unlock()

	ledger, err := c.loadSupplierLedger()
	if err != nil {
		return err
	}
	providersPath := c.providersPath()
	absPath, _ := filepath.Abs(providersPath)
	_, providers, _, err := supplierReadProvidersFile(providersPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("未找到 Magpie providers.json，无法修改供应商保护")
		}
		return fmt.Errorf("读取 providers.json 失败: %w", err)
	}
	idx := supplierFindProvider(providers, id)
	if idx < 0 {
		return fmt.Errorf("未找到供应商 %s", id)
	}

	// A previous interrupted write must resolve before a new transaction can
	// be staged. Never guess a third state.
	if ledger.Pending != nil {
		if err := c.resolveSupplierPending(&ledger, providers); err != nil {
			return err
		}
		idx = supplierFindProvider(providers, id)
		if idx < 0 {
			return fmt.Errorf("未找到供应商 %s", id)
		}
	}

	entry := ledger.Entries[id]
	provider := providers[idx]

	if enabled {
		if reason := supplierUnsupportedPresetReason(provider); reason != "" {
			return fmt.Errorf("供应商 %s 不能启用保护: %s", id, reason)
		}
		originURL, canonicalOrigin, err := supplierParseGuardOrigin(c.origin)
		if err != nil {
			return err
		}

		if entry != nil && entry.Protected {
			live := supplierSnapshotOf(provider, supplierSnapshotKeys(entry.After))
			if diff := supplierConflictDiffKeys(entry.After, live); len(diff) > 0 {
				return fmt.Errorf("供应商 %s 的受管字段 %s 已被外部修改，拒绝覆盖", id, strings.Join(diff, "、"))
			}
			if supplierOriginEqual(entry.Origin, canonicalOrigin) {
				return nil
			}
			newEntry := supplierCloneEntry(entry)
			newEntry.Origin = canonicalOrigin
			after, err := supplierProtectedSnapshot(entry.Original, canonicalOrigin, entry.Token)
			if err != nil {
				return err
			}
			newEntry.After = after
			if err := c.writeSupplierTransition(ctx, &ledger, providersPath, id, entry, newEntry, entry.After, after, entry.HeaderState); err != nil {
				return fmt.Errorf("重建供应商 %s 的 Guard 地址失败: %w", id, err)
			}
			c.logSupplierEvent("protect", id, fmt.Sprintf("已启用供应商保护并重建 Guard 地址：%s", supplierProviderName(provider, id)))
			return nil
		}

		original, headerState, err := supplierStrictOriginalSnapshot(provider, originURL)
		if err != nil {
			return fmt.Errorf("供应商 %s 不能启用保护: %w", id, err)
		}
		token := randomHex(24)
		after, err := supplierProtectedSnapshot(original, canonicalOrigin, token)
		if err != nil {
			return err
		}
		newEntry := &supplierLedgerEntry{
			ProviderID:  id,
			Token:       token,
			Origin:      canonicalOrigin,
			Protected:   true,
			InsecureTLS: supplierInsecureTLS(provider),
			HeaderState: headerState,
			Original:    supplierCloneSnapshot(original),
			After:       after,
		}
		beforeSnapshot := supplierCloneSnapshot(original)
		if err := c.writeSupplierTransition(ctx, &ledger, providersPath, id, nil, newEntry, beforeSnapshot, after, headerState); err != nil {
			return fmt.Errorf("启用供应商 %s 保护失败: %w", id, err)
		}
		ledger.ProvidersPath = absPath
		c.logSupplierEvent("protect", id, fmt.Sprintf("已启用供应商保护：%s", supplierProviderName(provider, id)))
		return nil
	}

	if entry == nil || !entry.Protected {
		return nil
	}

	live := supplierSnapshotOf(provider, supplierSnapshotKeys(entry.After))
	if !supplierConflictEqual(entry.After, live) {
		if supplierConflictEqual(entry.Original, live) {
			// The vendor bases are already back to their originals. Drop the
			// stale ledger row without rewriting those bases, but still clean
			// up the plugin-owned probe header.
			if err := c.supplierRemoveReservedHeaderFile(ctx, providersPath, id, entry.HeaderState); err != nil {
				return fmt.Errorf("清理供应商 %s 的保留请求头失败: %w", id, err)
			}
			delete(ledger.Entries, id)
			ledger.Pending = nil
			err := c.store.setKV(supplierLedgerKey, ledger)
			if err == nil {
				c.logSupplierEvent("restore", id, fmt.Sprintf("供应商地址已恢复原始值，保护记录已解除：%s", supplierProviderName(provider, id)))
			}
			return err
		}
		diff := supplierConflictDiffKeys(entry.After, live)
		return fmt.Errorf("供应商 %s 的受管字段 %s 已被外部修改，拒绝覆盖", id, strings.Join(diff, "、"))
	}

	if err := c.writeSupplierTransition(ctx, &ledger, providersPath, id, entry, nil, entry.After, entry.Original, entry.HeaderState); err != nil {
		return fmt.Errorf("关闭供应商 %s 保护失败: %w", id, err)
	}
	c.logSupplierEvent("restore", id, fmt.Sprintf("已恢复原始地址并关闭保护：%s", supplierProviderName(provider, id)))
	return nil
}

func (c *supplierController) active() (bool, error) {
	ledger, err := c.loadSupplierLedger()
	if err != nil {
		return false, err
	}
	if len(ledger.Entries) > 0 {
		for _, entry := range ledger.Entries {
			if entry != nil && entry.Protected {
				return true, nil
			}
		}
	}
	if ledger.Pending != nil && ledger.Pending.After.Entry != nil && ledger.Pending.After.Entry.Protected {
		return true, nil
	}
	return false, nil
}

// binding resolves a route token against durable ledger state. It fails
// closed when the provider file no longer matches the managed After snapshot,
// when a pending write cannot be identified, and for unknown tokens.
func (c *supplierController) binding(token, protocol string) (supplierBinding, bool, error) {
	var zero supplierBinding
	if token == "" || !supplierIsProtocol(protocol) {
		return zero, false, nil
	}
	ledger, err := c.loadSupplierLedger()
	if err != nil {
		return zero, false, err
	}
	if len(ledger.Entries) == 0 && ledger.Pending == nil {
		return zero, false, nil
	}

	_, providers, _, err := supplierReadProvidersFile(c.providersPath())
	if err != nil {
		return zero, false, fmt.Errorf("读取 providers.json 失败: %w", err)
	}
	entries, pendingConflicts := supplierEffectiveEntries(ledger, providers)

	var entry *supplierLedgerEntry
	for _, candidate := range entries {
		if candidate == nil || candidate.Token != token {
			continue
		}
		if entry != nil {
			return zero, false, errors.New("供应商账本中存在重复的路由 token")
		}
		entry = candidate
	}
	if entry == nil || !entry.Protected {
		return zero, false, nil
	}
	if conflict := pendingConflicts[entry.ProviderID]; conflict != "" {
		return zero, false, errors.New(conflict)
	}
	if _, ok := entry.After[protocol]; !ok {
		return zero, false, nil
	}
	idx := supplierFindProvider(providers, entry.ProviderID)
	if idx < 0 {
		return zero, false, fmt.Errorf("供应商 %s 已不存在", entry.ProviderID)
	}
	live := supplierSnapshotOf(providers[idx], supplierSnapshotKeys(entry.After))
	if diff := supplierConflictDiffKeys(entry.After, live); len(diff) > 0 {
		return zero, false, fmt.Errorf("供应商 %s 的受管字段 %s 与账本不一致", entry.ProviderID, strings.Join(diff, "、"))
	}

	rawBase, ok := entry.Original[protocol]
	if !ok || !supplierProtocolActive(rawBase) {
		return zero, false, nil
	}
	var base string
	if err := json.Unmarshal(rawBase, &base); err != nil {
		return zero, false, fmt.Errorf("供应商 %s 的 %s 原始地址无效", entry.ProviderID, protocol)
	}
	current := supplierSnapshotOf(providers[idx], []string{"proxy", "accountProxies"})
	proxy, err := supplierRawString(current, "proxy")
	if err != nil {
		return zero, false, err
	}
	accountProxies, err := supplierRawAccountProxies(current, "accountProxies")
	if err != nil {
		return zero, false, err
	}
	return supplierBinding{
		ProviderID:     entry.ProviderID,
		Base:           base,
		Protocol:       protocol,
		Token:          entry.Token,
		Proxy:          proxy,
		AccountProxies: accountProxies,
		InsecureTLS:    supplierInsecureTLS(providers[idx]),
	}, true, nil
}

// protectedProvider reports whether this supplier id is currently taken over
// and its managed fields still match the ledger. It is used only to suppress
// duplicate async response events for requests that already passed through
// Guard; unprotected suppliers keep their normal asynchronous judgment.
func (c *supplierController) protectedProvider(id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" || c.configDir == "" {
		return false, nil
	}
	ledger, err := c.loadSupplierLedger()
	if err != nil {
		return false, err
	}
	if len(ledger.Entries) == 0 && ledger.Pending == nil {
		return false, nil
	}
	_, providers, _, err := supplierReadProvidersFile(c.providersPath())
	if err != nil {
		return false, err
	}
	entries, pendingConflicts := supplierEffectiveEntries(ledger, providers)
	entry := entries[id]
	if entry == nil || !entry.Protected {
		return false, nil
	}
	if pendingConflicts[id] != "" {
		return false, nil
	}
	idx := supplierFindProvider(providers, id)
	if idx < 0 {
		return false, nil
	}
	live := supplierSnapshotOf(providers[idx], supplierSnapshotKeys(entry.After))
	if !supplierConflictEqual(entry.After, live) {
		return false, nil
	}
	return true, nil
}

// reconcile resolves an interrupted provider-file write, releases ledger rows
// for deleted providers, and moves Guard URLs to the current loopback origin
// only when the managed fields still match the previous After snapshot.
func (c *supplierController) reconcile(ctx context.Context) error {
	if c.configDir == "" {
		ledger, err := c.loadSupplierLedger()
		if err != nil {
			return err
		}
		if len(ledger.Entries) > 0 || ledger.Pending != nil {
			return errors.New("Supplier ledger 存在但未配置 Magpie 配置目录")
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	fileLock, err := acquireSidecarLock(ctx, c.configDir)
	if err != nil {
		return fmt.Errorf("获取供应商侧车锁失败: %w", err)
	}
	defer fileLock.Unlock()

	ledger, err := c.loadSupplierLedger()
	if err != nil {
		return err
	}
	providersPath := c.providersPath()
	absPath, _ := filepath.Abs(providersPath)

	data, err := os.ReadFile(providersPath)
	if err != nil {
		if os.IsNotExist(err) {
			if len(ledger.Entries) == 0 && ledger.Pending == nil {
				return nil
			}
			// The provider file is gone, so no Guard URL can reach a vendor.
			// Releasing the ledger is safe and prevents stale bindings.
			ledger.Entries = make(map[string]*supplierLedgerEntry)
			ledger.Pending = nil
			if err := c.store.setKV(supplierLedgerKey, ledger); err != nil {
				return fmt.Errorf("释放已删除供应商的账本失败: %w", err)
			}
			return nil
		}
		return fmt.Errorf("读取 providers.json 失败: %w", err)
	}
	root, providers, err := supplierParseProviderBytes(data)
	if err != nil {
		return fmt.Errorf("解析 providers.json 失败: %w", err)
	}
	_ = root

	if err := c.resolveSupplierPending(&ledger, providers); err != nil {
		return err
	}
	ledger.ProvidersPath = absPath
	if ledger.Entries == nil {
		ledger.Entries = make(map[string]*supplierLedgerEntry)
	}

	changed := false
	var conflicts []string
	ids := make([]string, 0, len(ledger.Entries))
	for id := range ledger.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if supplierFindProvider(providers, id) < 0 {
			// A provider deleted from Magpie cannot expose a Guard URL.
			delete(ledger.Entries, id)
			changed = true
		}
	}

	if len(ledger.Entries) > 0 {
		originURL, canonicalOrigin, err := supplierParseGuardOrigin(c.origin)
		if err != nil {
			return fmt.Errorf("Guard origin 无效: %w", err)
		}
		_ = originURL
		ids = ids[:0]
		for id := range ledger.Entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			entry := ledger.Entries[id]
			idx := supplierFindProvider(providers, id)
			if idx < 0 {
				continue
			}
			provider := providers[idx]
			live := supplierSnapshotOf(provider, supplierSnapshotKeys(entry.After))
			if diff := supplierConflictDiffKeys(entry.After, live); len(diff) > 0 {
				conflicts = append(conflicts, fmt.Sprintf("供应商 %s 的受管字段 %s 被外部修改，已停止覆盖", id, strings.Join(diff, "、")))
				continue
			}
			insecure := supplierInsecureTLS(provider)
			if entry.InsecureTLS != insecure {
				entry = supplierCloneEntry(entry)
				entry.InsecureTLS = insecure
				ledger.Entries[id] = entry
				changed = true
			}
			if supplierOriginEqual(entry.Origin, canonicalOrigin) {
				continue
			}
			newEntry := supplierCloneEntry(entry)
			newEntry.Origin = canonicalOrigin
			after, err := supplierProtectedSnapshot(entry.Original, canonicalOrigin, entry.Token)
			if err != nil {
				return err
			}
			newEntry.After = after
			if err := c.writeSupplierTransition(ctx, &ledger, providersPath, id, entry, newEntry, entry.After, after, entry.HeaderState); err != nil {
				return fmt.Errorf("重建供应商 %s 的 Guard 地址失败: %w", id, err)
			}
		}
	}

	if changed && ledger.Pending == nil {
		if err := c.store.setKV(supplierLedgerKey, ledger); err != nil {
			return fmt.Errorf("提交供应商账本失败: %w", err)
		}
	}
	if len(conflicts) > 0 {
		return errors.New(strings.Join(conflicts, "；"))
	}
	return nil
}

// writeSupplierTransition performs one durable provider-file commit:
// pending-before -> optimistic bytes check -> atomic write -> commit ledger.
// beforeSnapshot is verified against the live managed fields before staging.
func (c *supplierController) writeSupplierTransition(
	ctx context.Context,
	ledger *supplierLedger,
	providersPath, id string,
	beforeEntry, afterEntry *supplierLedgerEntry,
	beforeSnapshot, afterSnapshot supplierManagedSnapshot,
	headerState string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	origBytes, err := os.ReadFile(providersPath)
	if err != nil {
		return err
	}
	root, providers, err := supplierParseProviderBytes(origBytes)
	if err != nil {
		return fmt.Errorf("解析 providers.json 失败: %w", err)
	}
	idx := supplierFindProvider(providers, id)
	if idx < 0 {
		return fmt.Errorf("供应商 %s 已不存在", id)
	}
	live := supplierSnapshotOf(providers[idx], supplierSnapshotKeys(beforeSnapshot))
	if diff := supplierConflictDiffKeys(beforeSnapshot, live); len(diff) > 0 {
		return fmt.Errorf("供应商 %s 的受管字段 %s 在写入前被外部修改", id, strings.Join(diff, "、"))
	}

	if absPath, err := filepath.Abs(providersPath); err == nil {
		ledger.ProvidersPath = absPath
	}
	ledger.Pending = &supplierPendingTx{
		ProviderID: id,
		Before:     supplierPendingSide{Entry: supplierCloneEntry(beforeEntry), Snapshot: supplierCloneSnapshot(live)},
		After:      supplierPendingSide{Entry: supplierCloneEntry(afterEntry), Snapshot: supplierCloneSnapshot(afterSnapshot)},
		Timestamp:  time.Now().Unix(),
	}
	if err := c.store.setKV(supplierLedgerKey, ledger); err != nil {
		ledger.Pending = nil
		return fmt.Errorf("记录供应商待决事务失败: %w", err)
	}

	freshBytes, err := os.ReadFile(providersPath)
	if err != nil {
		return c.abortSupplierPending(ledger, fmt.Errorf("验证 providers.json 失败: %w", err))
	}
	if !bytes.Equal(freshBytes, origBytes) {
		return c.abortSupplierPending(ledger, errors.New("providers.json 在写入期间被外部修改，放弃写回以避免冲突"))
	}

	if err := supplierApplyTransition(providers[idx], beforeSnapshot, afterSnapshot, headerState); err != nil {
		return c.abortSupplierPending(ledger, fmt.Errorf("更新供应商字段失败: %w", err))
	}
	providersJSON, err := json.Marshal(providers)
	if err != nil {
		return c.abortSupplierPending(ledger, fmt.Errorf("序列化 providers 失败: %w", err))
	}
	root["providers"] = json.RawMessage(providersJSON)
	newRootJSON, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return c.abortSupplierPending(ledger, fmt.Errorf("序列化 providers.json 失败: %w", err))
	}
	newRootJSON = append(newRootJSON, '\n')
	if err := atomicWriteFile(providersPath, newRootJSON, 0o600); err != nil {
		return c.abortSupplierPending(ledger, fmt.Errorf("写入 providers.json 失败: %w", err))
	}

	if afterEntry != nil {
		ledger.Entries[id] = supplierCloneEntry(afterEntry)
		ledger.Origin = afterEntry.Origin
	} else {
		delete(ledger.Entries, id)
	}
	ledger.Pending = nil
	if err := c.store.setKV(supplierLedgerKey, ledger); err != nil {
		return fmt.Errorf("providers.json 已写入但提交供应商账本失败（重启后可恢复）: %w", err)
	}
	return nil
}

// supplierRemoveReservedHeaderFile removes the plugin-owned probe header from
// one provider when the vendor bases are already back to their originals and a
// stale ledger entry must be dropped without rewriting those bases.
func (c *supplierController) supplierRemoveReservedHeaderFile(ctx context.Context, providersPath, id, headerState string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	origBytes, err := os.ReadFile(providersPath)
	if err != nil {
		return err
	}
	root, providers, err := supplierParseProviderBytes(origBytes)
	if err != nil {
		return err
	}
	idx := supplierFindProvider(providers, id)
	if idx < 0 {
		return fmt.Errorf("供应商 %s 已不存在", id)
	}
	provider := providers[idx]
	headers, err := supplierHeaderObject(provider, false)
	if err != nil {
		return err
	}
	removed := false
	for name := range headers {
		if strings.EqualFold(name, supplierSessionHeader) {
			delete(headers, name)
			removed = true
		}
	}
	if !removed {
		return nil
	}
	if len(headers) == 0 {
		supplierRestoreEmptyHeaders(provider, headerState)
	} else if err := supplierStoreHeaderObject(provider, headers); err != nil {
		return err
	}
	providersJSON, err := json.Marshal(providers)
	if err != nil {
		return err
	}
	root["providers"] = json.RawMessage(providersJSON)
	newRootJSON, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	newRootJSON = append(newRootJSON, '\n')
	freshBytes, err := os.ReadFile(providersPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(freshBytes, origBytes) {
		return errors.New("providers.json 在清理保留头期间被外部修改")
	}
	return atomicWriteFile(providersPath, newRootJSON, 0o600)
}

func (c *supplierController) abortSupplierPending(ledger *supplierLedger, cause error) error {
	ledger.Pending = nil
	if err := c.store.setKV(supplierLedgerKey, ledger); err != nil {
		return fmt.Errorf("%w；清理供应商待决事务失败: %v", cause, err)
	}
	return cause
}

func (c *supplierController) resolveSupplierPending(ledger *supplierLedger, providers []map[string]json.RawMessage) error {
	pending := ledger.Pending
	if pending == nil {
		return nil
	}
	idx := supplierFindProvider(providers, pending.ProviderID)
	if idx < 0 {
		// The provider was deleted while the write was pending. No Guard URL
		// remains to serve, so releasing both sides is safe.
		delete(ledger.Entries, pending.ProviderID)
		ledger.Pending = nil
		if err := c.store.setKV(supplierLedgerKey, ledger); err != nil {
			return fmt.Errorf("释放已删除供应商的待决事务失败: %w", err)
		}
		return nil
	}
	live := supplierSnapshotOf(providers[idx], supplierSnapshotUnionKeys(pending.Before.Snapshot, pending.After.Snapshot))
	switch {
	case supplierSnapshotEqual(pending.After.Snapshot, live):
		supplierApplyLedgerSide(ledger, pending.ProviderID, pending.After.Entry)
	case supplierSnapshotEqual(pending.Before.Snapshot, live):
		supplierApplyLedgerSide(ledger, pending.ProviderID, pending.Before.Entry)
	default:
		return fmt.Errorf("上次供应商写入中断且 providers.json 既非提交前也非提交后状态，保留待决事务并等待人工确认（供应商 %s）", pending.ProviderID)
	}
	ledger.Pending = nil
	if err := c.store.setKV(supplierLedgerKey, ledger); err != nil {
		return fmt.Errorf("提交供应商中断恢复事务失败: %w", err)
	}
	return nil
}

func supplierApplyLedgerSide(ledger *supplierLedger, id string, entry *supplierLedgerEntry) {
	if entry == nil || !entry.Protected {
		delete(ledger.Entries, id)
		return
	}
	ledger.Entries[id] = supplierCloneEntry(entry)
}

func supplierEffectiveEntries(ledger supplierLedger, providers []map[string]json.RawMessage) (map[string]*supplierLedgerEntry, map[string]string) {
	entries := make(map[string]*supplierLedgerEntry, len(ledger.Entries))
	for id, entry := range ledger.Entries {
		entries[id] = supplierCloneEntry(entry)
	}
	conflicts := make(map[string]string)
	pending := ledger.Pending
	if pending == nil {
		return entries, conflicts
	}
	idx := supplierFindProvider(providers, pending.ProviderID)
	if idx < 0 {
		delete(entries, pending.ProviderID)
		return entries, conflicts
	}
	live := supplierSnapshotOf(providers[idx], supplierSnapshotUnionKeys(pending.Before.Snapshot, pending.After.Snapshot))
	switch {
	case supplierSnapshotEqual(pending.After.Snapshot, live):
		supplierApplyLedgerSideMap(entries, pending.ProviderID, pending.After.Entry)
	case supplierSnapshotEqual(pending.Before.Snapshot, live):
		supplierApplyLedgerSideMap(entries, pending.ProviderID, pending.Before.Entry)
	default:
		conflicts[pending.ProviderID] = "上次供应商写入中断且 providers.json 既非提交前也非提交后状态，保留待决事务"
	}
	return entries, conflicts
}

func supplierApplyLedgerSideMap(entries map[string]*supplierLedgerEntry, id string, entry *supplierLedgerEntry) {
	if entry == nil || !entry.Protected {
		delete(entries, id)
		return
	}
	entries[id] = supplierCloneEntry(entry)
}

func supplierReadProvidersFile(path string) (map[string]json.RawMessage, []map[string]json.RawMessage, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	root, providers, err := supplierParseProviderBytes(data)
	if err != nil {
		return nil, nil, data, err
	}
	return root, providers, data, nil
}

func supplierParseProviderBytes(data []byte) (map[string]json.RawMessage, []map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, err
	}
	if root == nil {
		return nil, nil, errors.New("providers.json 不是对象")
	}
	var providers []map[string]json.RawMessage
	if raw, ok := root["providers"]; ok && len(bytes.TrimSpace(raw)) > 0 && !supplierJSONNull(raw) {
		if err := json.Unmarshal(raw, &providers); err != nil {
			return nil, nil, fmt.Errorf("providers 不是对象数组: %w", err)
		}
	}
	return root, providers, nil
}

func supplierFindProvider(providers []map[string]json.RawMessage, id string) int {
	for i, provider := range providers {
		if supplierProviderID(provider) == id {
			return i
		}
	}
	return -1
}

func supplierProviderID(provider map[string]json.RawMessage) string {
	raw, ok := provider["id"]
	if !ok {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	return strings.TrimSpace(id)
}

func supplierProviderName(provider map[string]json.RawMessage, fallback string) string {
	raw, ok := provider["name"]
	if !ok {
		return fallback
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || strings.TrimSpace(name) == "" {
		return fallback
	}
	return name
}

// supplierProtocolActive reports whether a stored protocol value is an actual
// vendor base. Null and empty values are kept in the managed snapshot so a
// later fill-in is detected, but they are not routable.
func supplierProtocolActive(raw json.RawMessage) bool {
	if len(raw) == 0 || supplierJSONNull(raw) {
		return false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value != ""
}

func supplierProtocolsFromSnapshot(snapshot supplierManagedSnapshot) []string {
	protocols := make([]string, 0, len(supplierProtocolOrder))
	for _, proto := range supplierProtocolOrder {
		if raw, ok := snapshot[proto]; ok && supplierProtocolActive(raw) {
			protocols = append(protocols, proto)
		}
	}
	return protocols
}

// supplierProtocolInfo is the lenient view used by list: a malformed address
// is reported as a conflict, not hidden and not repaired.
func supplierProtocolInfo(provider map[string]json.RawMessage, origin *url.URL) ([]string, []string) {
	protocols := make([]string, 0, len(supplierProtocolOrder))
	var problems []string
	if reason := supplierLockedPresetReason(supplierRawStringField(provider, "preset")); reason != "" {
		problems = append(problems, reason)
	}
	for _, proto := range supplierProtocolOrder {
		raw, ok := provider[proto]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			if supplierJSONNull(raw) {
				continue
			}
			protocols = append(protocols, proto)
			problems = append(problems, fmt.Sprintf("%s 地址不是字符串", proto))
			continue
		}
		if value == "" {
			continue
		}
		protocols = append(protocols, proto)
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Sprintf("%s 地址为空", proto))
			continue
		}
		normalized, err := supplierNormalizeVendorBase(proto, value)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s 地址无效: %v", proto, err))
			continue
		}
		if err := supplierValidateOriginalURL(proto, value); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if err := supplierRejectVendorLoop(normalized, origin); err != nil {
			problems = append(problems, fmt.Sprintf("%s 地址指回 Guard: %v", proto, err))
		}
	}
	return protocols, supplierUniqueStrings(problems)
}

// supplierStrictOriginalSnapshot validates every protected address, records
// the original headers container state, and keeps the exact raw provider
// values for restoration. Existing X-Model-Guard-Session headers (any case)
// are rejected so the probe marker cannot be duplicated or user data
// overwritten.
func supplierStrictOriginalSnapshot(provider map[string]json.RawMessage, origin *url.URL) (supplierManagedSnapshot, string, error) {
	original := make(supplierManagedSnapshot)
	protocolCount := 0
	if reason := supplierLockedPresetReason(supplierRawStringField(provider, "preset")); reason != "" {
		return nil, "", errors.New(reason)
	}
	for _, proto := range supplierProtocolOrder {
		raw, ok := provider[proto]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			if !supplierJSONNull(raw) {
				return nil, "", fmt.Errorf("%s 地址不是字符串", proto)
			}
		} else if value != "" {
			if strings.TrimSpace(value) == "" {
				return nil, "", fmt.Errorf("%s 地址为空", proto)
			}
			if err := supplierValidateOriginalURL(proto, value); err != nil {
				return nil, "", err
			}
			normalized, err := supplierNormalizeVendorBase(proto, value)
			if err != nil {
				return nil, "", fmt.Errorf("%s 地址无效: %w", proto, err)
			}
			if err := supplierRejectVendorLoop(normalized, origin); err != nil {
				return nil, "", fmt.Errorf("%s 地址指回 Guard: %w", proto, err)
			}
			protocolCount++
		}
		// Null and empty values are inactive, but their presence must stay
		// managed: filling one in later would otherwise bypass protection
		// while the supplier still lists as protected.
		original[proto] = append(json.RawMessage(nil), raw...)
	}
	if protocolCount == 0 {
		return nil, "", errors.New("没有可保护的自定义协议地址")
	}
	headerState, err := supplierHeaderState(provider)
	if err != nil {
		return nil, "", err
	}
	return original, headerState, nil
}

// supplierUnsupportedPresetReason marks presets that are listed separately
// and never offered for takeover, instead of being reported as conflicts.
func supplierUnsupportedPresetReason(provider map[string]json.RawMessage) string {
	if supplierRawStringField(provider, "preset") == "remote-magpie" {
		return supplierLockedPresetReason("remote-magpie")
	}
	return ""
}

// supplierLockedPresetReason reports why a preset cannot be taken over. These
// request shapes and signatures come from the preset or the original host;
// replacing the bases would silently change authentication or routing.
func supplierLockedPresetReason(preset string) string {
	switch preset {
	case "azure":
		return "Azure OpenAI 依赖原生请求改写与 api-key 鉴权，暂不支持接管"
	case "bedrock", "bedrock-mantle":
		return "Bedrock 依赖 AWS 请求签名，暂不支持接管"
	case "google-vertex":
		return "Google Vertex AI 依赖项目地址与签名，暂不支持接管"
	case "remote-magpie":
		return "远程 Magpie 依赖原生端点重写，暂不支持接管"
	}
	return ""
}

func supplierRawStringField(provider map[string]json.RawMessage, key string) string {
	raw, ok := provider[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func supplierParseRawURL(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	return url.Parse(value)
}

// supplierValidateOriginalURL rejects bases whose takeover would copy a
// credential into the ledger or change native host-based authentication,
// signing or decision-envelope selection.
func supplierValidateOriginalURL(proto, raw string) error {
	parsed, err := supplierParseRawURL(raw)
	if err != nil {
		return fmt.Errorf("%s 地址无效: %w", proto, err)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%s 地址不能包含用户信息、查询参数或片段；请把凭据放到供应商 Key 字段", proto)
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if (proto == "chat" || proto == "responses") && supplierAzureHost(host) {
		return errors.New("Azure OpenAI 供应商依赖原始主机选择 api-key 鉴权，暂不支持接管")
	}
	if supplierBedrockHost(host) {
		return errors.New("Bedrock 供应商依赖 AWS 请求签名，暂不支持接管")
	}
	if host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai") {
		return errors.New("OpenCode 供应商依赖原始主机会话头，暂不支持接管")
	}
	if proto == "decide" {
		if dialect := supplierDecideDialect(raw); dialect != "systemone" {
			return fmt.Errorf("决策端点使用 %s 协议，依赖原始主机与路径，暂不支持接管", dialect)
		}
	}
	return nil
}

func supplierUniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0]
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func supplierAzureHost(host string) bool {
	for _, suffix := range []string{".openai.azure.com", ".cognitiveservices.azure.com", ".services.ai.azure.com"} {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

func supplierBedrockHost(host string) bool {
	return (strings.HasPrefix(host, "bedrock-runtime.") && strings.HasSuffix(host, ".amazonaws.com")) ||
		(strings.HasPrefix(host, "bedrock-mantle.") && strings.HasSuffix(host, ".api.aws"))
}

func supplierDecideDialect(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := supplierParseRawURL(base)
	if err != nil {
		return "unknown"
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case strings.HasSuffix(base, "/v4/ai"):
		return "vercel-eval"
	case host == "ai-gateway.vercel.sh" || strings.HasSuffix(base, "/typesafe") || strings.Contains(base, "/typesafe/v1"):
		return "vercel"
	case host == "api.cloudflare.com" || strings.Contains(base+"/", "/client/v4/"):
		return "cloudflare"
	}
	return "systemone"
}

// supplierHeaderState reports the provider's headers container as absent,
// null, or object. It also rejects any existing probe marker header,
// case-insensitively, because forwarding both casings would duplicate the
// header and because failure to reserve it would make probe detection
// ambiguous.
func supplierHeaderState(provider map[string]json.RawMessage) (string, error) {
	raw, ok := provider["headers"]
	if !ok {
		return supplierHeaderAbsent, nil
	}
	if supplierJSONNull(raw) {
		return supplierHeaderNull, nil
	}
	var headers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &headers); err != nil {
		return "", errors.New("headers 不是 JSON 对象")
	}
	for name := range headers {
		if strings.EqualFold(name, supplierSessionHeader) {
			return "", fmt.Errorf("供应商 headers 已包含 Guard 保留头 %s（大小写不敏感），请先移除", supplierSessionHeader)
		}
	}
	return supplierHeaderContainerObject, nil
}

func supplierProtectedSnapshot(original supplierManagedSnapshot, origin, token string) (supplierManagedSnapshot, error) {
	after := make(supplierManagedSnapshot, len(original)+1)
	for _, proto := range supplierProtocolOrder {
		raw, ok := original[proto]
		if !ok {
			continue
		}
		if !supplierProtocolActive(raw) {
			// Inactive bases stay exactly as the user wrote them. Keeping the
			// key makes a later fill-in a visible managed-field conflict.
			after[proto] = append(json.RawMessage(nil), raw...)
			continue
		}
		guarded, err := json.Marshal(supplierGuardURL(origin, token, proto))
		if err != nil {
			return nil, err
		}
		after[proto] = guarded
	}
	marker, _ := json.Marshal(supplierSessionValue)
	after[supplierHeaderSnapshotKey()] = marker
	return after, nil
}

func supplierGuardURL(origin, token, protocol string) string {
	base := strings.TrimRight(origin, "/") + "/upstream/" + token + "/" + protocol
	switch protocol {
	case "chat", "responses", "decide":
		return base + "/v1"
	case "gemini":
		return base + "/v1beta"
	default: // anthropic is the root /v1/messages is asked at
		return base
	}
}

// supplierNormalizeVendorBase mirrors Magpie's runtime normalization just
// enough to validate and detect loops. It returns the normalized URL, while
// the raw original is never replaced by this form in the ledger.
func supplierNormalizeVendorBase(protocol, raw string) (*url.URL, error) {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	if value == "" {
		return nil, errors.New("地址为空")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	if protocol == "gemini" {
		value = supplierGeminiBase(value)
	}
	if protocol == "anthropic" {
		for _, suffix := range []string{"/v1/messages", "/v1"} {
			if base, ok := strings.CutSuffix(value, suffix); ok && strings.Contains(base, "://") && len(base) > len("https://") {
				value = base
				break
			}
		}
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("只支持 http/https，收到 %s", parsed.Scheme)
	}
	if parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return nil, errors.New("地址缺少主机")
	}
	if parsed.User != nil {
		return nil, errors.New("地址不能包含用户信息")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, errors.New("地址不能包含查询参数")
	}
	if parsed.Fragment != "" {
		return nil, errors.New("地址不能包含片段")
	}
	return parsed, nil
}

func supplierGeminiBase(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return ""
	}
	value = strings.TrimRight(supplierGeminiMethod.ReplaceAllString(value, ""), "/")
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" && strings.Trim(parsed.Path, "/") == "" {
		return value + "/v1beta"
	}
	return value
}

func supplierRejectVendorLoop(vendor, origin *url.URL) error {
	if origin != nil && supplierSameOrigin(vendor, origin) {
		return errors.New("地址是 Guard 自身")
	}
	path := strings.ToLower(strings.TrimSuffix(vendor.Path, "/"))
	if path == "/upstream" || strings.HasPrefix(path, "/upstream/") {
		return errors.New("地址是 Guard 的转发入口")
	}
	if path == "/provider" || strings.HasPrefix(path, "/provider/") {
		return errors.New("地址是 Guard 的 Provider 入口")
	}
	return nil
}

func supplierParseGuardOrigin(raw string) (*url.URL, string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, "", errors.New("origin 为空")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, "", err
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, "", fmt.Errorf("origin 必须是 http/https，收到 %s", parsed.Scheme)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return nil, "", errors.New("origin 缺少主机")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, "", errors.New("origin 不能包含用户信息、查询参数或片段")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, "", errors.New("origin 不能包含路径")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	if !loopback {
		return nil, "", fmt.Errorf("origin 必须是回环地址，收到 %s", parsed.Host)
	}
	canonicalHost := host
	if port := parsed.Port(); port != "" {
		canonicalHost = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		canonicalHost = "[" + host + "]"
	}
	return parsed, scheme + "://" + canonicalHost, nil
}

func supplierSameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && supplierHostPortKey(a) == supplierHostPortKey(b)
}

func supplierHostPortKey(u *url.URL) string {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

func supplierOriginEqual(a, b string) bool {
	if a == b {
		return true
	}
	ua, _, errA := supplierParseGuardOrigin(a)
	ub, _, errB := supplierParseGuardOrigin(b)
	if errA != nil || errB != nil {
		return false
	}
	return supplierSameOrigin(ua, ub)
}

func supplierIsProtocol(protocol string) bool {
	for _, known := range supplierProtocolOrder {
		if protocol == known {
			return true
		}
	}
	return false
}

func supplierHeaderSnapshotKey() string {
	return "headers." + supplierSessionHeader
}

func supplierHeaderObject(provider map[string]json.RawMessage, create bool) (map[string]json.RawMessage, error) {
	raw, ok := provider["headers"]
	if !ok || supplierJSONNull(raw) {
		if !create {
			return map[string]json.RawMessage{}, nil
		}
		return make(map[string]json.RawMessage), nil
	}
	var headers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &headers); err != nil {
		return nil, errors.New("headers 不是 JSON 对象")
	}
	if headers == nil {
		headers = make(map[string]json.RawMessage)
	}
	return headers, nil
}

func supplierStoreHeaderObject(provider map[string]json.RawMessage, headers map[string]json.RawMessage) error {
	raw, err := json.Marshal(headers)
	if err != nil {
		return err
	}
	provider["headers"] = raw
	return nil
}

func supplierRestoreEmptyHeaders(provider map[string]json.RawMessage, headerState string) {
	switch headerState {
	case supplierHeaderNull:
		provider["headers"] = json.RawMessage("null")
	case supplierHeaderContainerObject:
		provider["headers"] = json.RawMessage("{}")
	default: // absent, or an older ledger without a recorded state
		delete(provider, "headers")
	}
}

func supplierSetProviderField(provider map[string]json.RawMessage, key string, value json.RawMessage) error {
	if name, ok := strings.CutPrefix(key, "headers."); ok {
		headers, err := supplierHeaderObject(provider, true)
		if err != nil {
			return err
		}
		if strings.EqualFold(name, supplierSessionHeader) {
			// The reserved probe header is single-valued: never leave an
			// alternate casing behind to duplicate it on the wire.
			for existing := range headers {
				if strings.EqualFold(existing, supplierSessionHeader) {
					delete(headers, existing)
				}
			}
		}
		headers[name] = append(json.RawMessage(nil), value...)
		return supplierStoreHeaderObject(provider, headers)
	}
	provider[key] = append(json.RawMessage(nil), value...)
	return nil
}

func supplierDeleteProviderField(provider map[string]json.RawMessage, key, headerState string) error {
	if name, ok := strings.CutPrefix(key, "headers."); ok {
		headers, err := supplierHeaderObject(provider, false)
		if err != nil {
			return err
		}
		if strings.EqualFold(name, supplierSessionHeader) {
			for existing := range headers {
				if strings.EqualFold(existing, supplierSessionHeader) {
					delete(headers, existing)
				}
			}
		} else {
			delete(headers, name)
		}
		if len(headers) == 0 {
			// No user headers remain: restore the exact original container
			// (absent, null, or {}) instead of always dropping the key.
			supplierRestoreEmptyHeaders(provider, headerState)
			return nil
		}
		return supplierStoreHeaderObject(provider, headers)
	}
	delete(provider, key)
	return nil
}

func supplierGetProviderField(provider map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if name, ok := strings.CutPrefix(key, "headers."); ok {
		headers, err := supplierHeaderObject(provider, false)
		if err != nil {
			return nil, false
		}
		value, present := headers[name]
		return value, present
	}
	value, ok := provider[key]
	return value, ok
}

// supplierApplyTransition replaces exactly the controlled fields: every field
// listed in afterSnapshot gets its new raw value (including a new nested
// header), every field only in beforeSnapshot is removed. All other provider
// fields and all other header names remain untouched.
func supplierApplyTransition(provider map[string]json.RawMessage, before, after supplierManagedSnapshot, headerState string) error {
	keys := supplierSnapshotUnionKeys(before, after)
	for _, key := range keys {
		if value, ok := after[key]; ok {
			if err := supplierSetProviderField(provider, key, value); err != nil {
				return err
			}
			continue
		}
		if _, ok := before[key]; ok {
			if err := supplierDeleteProviderField(provider, key, headerState); err != nil {
				return err
			}
		}
	}
	return nil
}

func supplierSnapshotOf(provider map[string]json.RawMessage, keys []string) supplierManagedSnapshot {
	out := make(supplierManagedSnapshot, len(keys))
	for _, key := range keys {
		if value, ok := supplierGetProviderField(provider, key); ok {
			out[key] = append(json.RawMessage(nil), value...)
		}
	}
	return out
}

// supplierSnapshotKeys always covers every supported protocol, plus whatever
// else the managed snapshot controls. supplierSnapshotOf only copies keys that
// exist, so a protocol absent at takeover has no entry, while a protocol that
// was null, empty, or active at takeover keeps its presence. Comparing that
// live view against the ledger therefore catches a protocol added later even
// though it was never part of the original snapshot.
func supplierSnapshotKeys(snapshot supplierManagedSnapshot) []string {
	seen := make(map[string]bool, len(snapshot)+len(supplierProtocolOrder))
	for _, proto := range supplierProtocolOrder {
		seen[proto] = true
	}
	for key := range snapshot {
		seen[key] = true
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func supplierSnapshotUnionKeys(snapshots ...supplierManagedSnapshot) []string {
	seen := make(map[string]bool)
	for _, snapshot := range snapshots {
		for key := range snapshot {
			seen[key] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func supplierSnapshotEqual(expected, live supplierManagedSnapshot) bool {
	if len(expected) != len(live) {
		return false
	}
	for key, expectedRaw := range expected {
		liveRaw, ok := live[key]
		if !ok || !supplierRawEqual(expectedRaw, liveRaw) {
			return false
		}
	}
	return true
}

// supplierConflictDiffKeys reports externally changed managed fields while
// excluding the reserved probe header. Only this plugin reads or writes that
// header, so an edit there must not block routing or restoration; transitions
// rewrite it (or remove every casing on restore).
func supplierConflictDiffKeys(expected, live supplierManagedSnapshot) []string {
	keys := supplierSnapshotUnionKeys(expected, live)
	var diff []string
	for _, key := range keys {
		if key == supplierHeaderSnapshotKey() {
			continue
		}
		expectedRaw, expectedOK := expected[key]
		liveRaw, liveOK := live[key]
		if expectedOK != liveOK || (expectedOK && !supplierRawEqual(expectedRaw, liveRaw)) {
			diff = append(diff, key)
		}
	}
	return diff
}

func supplierConflictEqual(expected, live supplierManagedSnapshot) bool {
	return len(supplierConflictDiffKeys(expected, live)) == 0
}

func supplierRawEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var av, bv any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	if err := da.Decode(&av); err != nil {
		return bytes.Equal(a, b)
	}
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if err := db.Decode(&bv); err != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(av, bv)
}

func supplierJSONNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) > 0 && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func supplierInsecureTLS(provider map[string]json.RawMessage) bool {
	raw, ok := provider["insecureTLS"]
	if !ok {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}

func supplierRawString(snapshot supplierManagedSnapshot, key string) (string, error) {
	raw, ok := snapshot[key]
	if !ok || supplierJSONNull(raw) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("供应商字段 %s 的值不是字符串", key)
	}
	return value, nil
}

func supplierRawAccountProxies(snapshot supplierManagedSnapshot, key string) (map[string]string, error) {
	raw, ok := snapshot[key]
	if !ok || supplierJSONNull(raw) {
		return nil, nil
	}
	var value map[string]string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("供应商字段 %s 的值不是对象", key)
	}
	return value, nil
}

func supplierCloneSnapshot(snapshot supplierManagedSnapshot) supplierManagedSnapshot {
	if snapshot == nil {
		return nil
	}
	out := make(supplierManagedSnapshot, len(snapshot))
	for key, value := range snapshot {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func supplierCloneEntry(entry *supplierLedgerEntry) *supplierLedgerEntry {
	if entry == nil {
		return nil
	}
	return &supplierLedgerEntry{
		ProviderID:  entry.ProviderID,
		Token:       entry.Token,
		Origin:      entry.Origin,
		Protected:   entry.Protected,
		InsecureTLS: entry.InsecureTLS,
		HeaderState: entry.HeaderState,
		Original:    supplierCloneSnapshot(entry.Original),
		After:       supplierCloneSnapshot(entry.After),
	}
}

func joinSupplierErrors(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "；" + next
}
