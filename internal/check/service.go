package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Target states.
const (
	StateUnknown     = "unknown"     // never checked
	StateChecking    = "checking"    // probe in flight
	StatePassed      = "passed"      // last round passed
	StateFailed      = "failed"      // downgraded: blocked until recheck passes
	StateUnavailable = "unavailable" // probe infrastructure error
	StateReview      = "review"      // manual hold
)

// ProbeResultVerdicts.
const (
	VerdictPassed      = "passed"
	VerdictFailed      = "failed"
	VerdictUnavailable = "unavailable"
	VerdictManual      = "manual"
)

// ProbeAliasPrefix is prefixed to the model id for probe requests so the gate
// middleware can let them through without recursion.
const ProbeAliasPrefix = "model-guard-probe/"

// runningProbe tracks an in-flight detection so active checks appear in results.
type runningProbe struct {
	id        string
	model     string
	promptID  string
	detail    string
	source    string
	startedAt time.Time
	active    bool
	cancel    context.CancelFunc
}

// Service owns all state and the scheduler.
type Service struct {
	mu    sync.Mutex
	store *Store
	gate  string

	running   map[string]*runningProbe // model -> active run
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	health    string
	closing   bool
	routes    *groupController
	routing   GroupRoutingStatus
	dataDir   string
	suppliers *supplierController
	codex     *codexAccountController
	codexBusy bool

	// Response-model judgment.
	responseHits map[string]time.Time // key -> last action
	// supplierReplies records reply ids observed on synchronously judged
	// supplier responses, keyed by their prefix-normalized form, so the async
	// hook does not apply the same rule twice. It is bounded by count and TTL.
	supplierReplies map[string]time.Time
	hookMu          sync.Mutex
	hookPath        string
	hookOrigin      string
	hookControl     string
}

// Supplier reply-id correlation limits. Vendor replies are small and unique;
// the cap keeps a hostile or broken upstream from growing the registry without
// bound, and the TTL only has to outlive one async observation.
const (
	supplierReplyTTL      = 5 * time.Minute
	supplierReplyLimit    = 1024
	supplierReplyIDLength = 512
)

// NewService loads state, seeds builtins, publishes the gate and starts the
// scheduler loop.
func NewService(store *Store, gatePath string) (*Service, error) {
	return newService(store, gatePath, true)
}

// newService permits deterministic lifecycle tests without starting a ticker.
func newService(store *Store, gatePath string, scheduled bool) (*Service, error) {
	if err := store.SeedBuiltins(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		store:           store,
		gate:            gatePath,
		running:         map[string]*runningProbe{},
		responseHits:    map[string]time.Time{},
		supplierReplies: map[string]time.Time{},
		ctx:             ctx,
		cancel:          cancel,
	}
	s.mu.Lock()
	err := s.publishGate()
	s.mu.Unlock()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("publish gate: %w", err)
	}
	if scheduled {
		s.wg.Add(1)
		go s.loop()
	}
	return s, nil
}

// Close stops the scheduler and cancels running probes.
func (s *Service) Close() {
	s.mu.Lock()
	s.closing = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}

// Health reports the last publish/persist error, empty when healthy.
func (s *Service) Health() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := []string{s.health, s.routing.Error}
	if s.codex != nil {
		parts = append(parts, s.codex.errorText())
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// loop is the scheduler tick: every 10s it launches due checks.
func (s *Service) loop() {
	defer s.wg.Done()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.schedule()
			s.scheduleCodexAccounts()
		}
	}
}

func (s *Service) schedule() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return
	}
	if s.routes != nil {
		targets, err := s.store.Targets()
		if err != nil {
			s.health = err.Error()
		} else {
			_ = s.syncGroupsLocked(targets)
		}
	}
	cfg, err := s.store.LoadConfig()
	if err != nil || cfg.GatewayURL == "" {
		return
	}
	due, err := s.store.DueTargets(time.Now())
	if err != nil {
		s.health = err.Error()
		return
	}
	for _, t := range due {
		if _, busy := s.running[t.Model]; !busy {
			s.launchRoundLocked(t, cfg, "scheduled")
		}
	}
}

// launchRoundLocked starts one round while the caller owns s.mu.
func initialRoundPromptID(t *Target, cfg GuardConfig) string {
	if t == nil {
		return ""
	}
	if EffectiveScheduleMode(t.ScheduleMode, cfg.ScheduleMode) == ScheduleModeStaged && t.RetryPromptID != "" {
		return t.RetryPromptID
	}
	if len(t.PromptIDs) > 0 {
		return strings.Join(t.PromptIDs, "+")
	}
	return "auto"
}

func (s *Service) launchRoundLocked(t *Target, cfg GuardConfig, source string) {
	runCtx, cancel := context.WithCancel(s.ctx)
	rp := &runningProbe{
		id:        "checking-" + randomHex(8),
		model:     t.Model,
		promptID:  initialRoundPromptID(t, cfg),
		detail:    "探针检测进行中，等待模型响应…",
		source:    source,
		startedAt: time.Now().UTC(),
		active:    true,
		cancel:    cancel,
	}
	s.running[t.Model] = rp
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.finishRun(t.Model, rp)
		if _, err := s.runRound(runCtx, t, cfg, source); err != nil {
			s.mu.Lock()
			s.health = err.Error()
			s.mu.Unlock()
		}
	}()
}

func (s *Service) finishRun(model string, rp *runningProbe) {
	s.mu.Lock()
	if s.running[model] == rp {
		delete(s.running, model)
	}
	s.mu.Unlock()
	if rp != nil && rp.cancel != nil {
		rp.cancel()
	}
}

func (s *Service) markRunResultSaved(model string) {
	s.mu.Lock()
	if rp := s.running[model]; rp != nil {
		rp.active = false
	}
	s.mu.Unlock()
}

// gateTarget is the JSON published into the gate middleware.
type gateTarget struct {
	ManualHold    bool   `json:"manual_hold"`
	RuleHoldUntil int64  `json:"rule_hold_until"`
	State         string `json:"state"`
	AllowUntil    int64  `json:"allow_until"`
	Reason        string `json:"reason"`
}

// publishGate atomically rewrites the gate middleware JS file. Magpie picks
// up the change within a second (stamp check on plugins.json / file watch).
// Callers must hold s.mu; it only touches s.health without re-locking.
func (s *Service) publishGate() error {
	targets, err := s.store.Targets()
	if err != nil {
		return err
	}
	alias, err := s.store.Alias()
	if err != nil {
		return err
	}
	gate := map[string]gateTarget{}
	for _, t := range targets {
		gate[strings.ToLower(t.Model)] = gateTarget{
			ManualHold:    t.ManualHold,
			RuleHoldUntil: t.RuleHoldUntil.Unix(),
			State:         t.State,
			AllowUntil:    t.AllowUntil.Unix(),
			Reason:        t.Reason,
		}
	}
	b, _ := json.Marshal(gate)
	a, _ := json.Marshal(alias)
	// Rewrite the entrypoint: Magpie does not watch imported modules.
	code := fmt.Sprintf(`// Generated by model-guard; do not edit.
const targets=%s;
const alias=%s;
export function onRequest(body,ctx){
 const model=body&&typeof body.model==="string"?body.model:(ctx&&typeof ctx.model==="string"?ctx.model:"");
 if(!model)return body;
 const prefix="model-guard-probe/"+alias+"/";
 if(body&&model.startsWith(prefix)){body.model=model.slice(prefix.length);return body;}
 const id=model.trim().replace(/^magpie\//,"").toLowerCase();
 if(!Object.prototype.hasOwnProperty.call(targets,id))return body;
 const t=targets[id];
 const now=Date.now();
 if(t.allow_until>0&&t.allow_until*1000>now)return body;
 const ruleHold=t.rule_hold_until>0&&t.rule_hold_until*1000>now;
 const blocked=t.manual_hold||ruleHold||t.state==="failed"||t.state==="review"||t.state==="unavailable";
 if(!blocked)return body;
 const reason=t.manual_hold||t.state==="review"?"人工禁用":ruleHold?"业务回复模型判断命中规则，已阻断；详情见控制台保护日志":t.state==="unavailable"?"检测不可用（探针请求失败），已保守禁用":"降智检测未通过，模型已禁用";
 return ctx.reject(403,"[MODEL_GUARD_BLOCKED] "+reason+"。目标："+model+"；详情："+(t.reason||"请到 model-guard 控制台查看或重测")+"。本次请求未发往上游。");
}
`, string(b), string(a))
	if err := atomicWrite(s.gate, []byte(code)); err != nil {
		s.health = "门禁发布失败：" + err.Error()
		return err
	}
	s.health = ""
	return s.syncGroupsLocked(targets)
}

// syncGroupsLocked reconciles production groups without blocking probe access.
func (s *Service) syncGroupsLocked(targets []*Target) error {
	if s.routes == nil {
		return nil
	}
	var blocked []string
	now := time.Now()
	for _, t := range targets {
		if t.blockedAt(now) {
			blocked = append(blocked, t.Model)
		}
	}
	if rerouted, err := s.store.ResponseReroutes(now); err != nil {
		s.health = err.Error()
	} else {
		blocked = dedupeStrings(append(blocked, rerouted...))
	}
	ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
	defer cancel()
	status, err := s.routes.sync(ctx, blocked)
	if err != nil {
		status.Error = "路由组同步失败：" + err.Error()
	}
	s.routing = status
	return err
}

// syncRoutingNow reconciles routing groups after a response-rule action.
func (s *Service) syncRoutingNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.routes == nil {
		return
	}
	targets, err := s.store.Targets()
	if err != nil {
		s.health = err.Error()
		return
	}
	_ = s.syncGroupsLocked(targets)
}

func findTargetForRequest(targets []*Target, model string) *Target {
	model = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(model), "magpie/"))
	if model == "" {
		return nil
	}
	for _, t := range targets {
		if t.Model == model {
			return t
		}
	}
	for _, t := range targets {
		if strings.EqualFold(t.Model, model) {
			return t
		}
	}
	return nil
}

// errTargetMissing means a rule action referenced a target that no longer
// exists; the reply is still intercepted but no target is created.
var errTargetMissing = errors.New("监测目标不存在")

// holdTargetForResponse blocks a monitored target whose reply matched a rule.
// The probe state machine is left untouched: the hold has its own expiry and a
// passing probe must not silently lift it. Errors are returned so the caller
// can refuse to report a successful action.
func (s *Service) holdTargetForResponse(t *Target, detail string, hold time.Duration, now time.Time) error {
	if t == nil {
		return errTargetMissing
	}
	latest, err := s.store.Target(t.Model)
	if err != nil {
		return err
	}
	if latest == nil {
		return errTargetMissing
	}
	// A shorter rule hit must not cut an existing longer hold short.
	if until := now.Add(hold); until.After(latest.RuleHoldUntil) {
		latest.RuleHoldUntil = until
	}
	latest.Reason = detail
	if err := s.store.SaveTarget(latest); err != nil {
		s.health = err.Error()
		return err
	}
	return s.publishGate()
}

// applyResponseRuleLocked executes one matched business-reply rule. Callers
// hold s.mu. needsSync reports that persisted reroutes must be pushed to route
// groups; the provider path does that synchronously.
func (s *Service) applyResponseRuleLocked(cfg GuardConfig, requested, actual string, target *Target, rule ResponseModelRule, now time.Time) (bool, error) {
	return s.applyResponseRuleForRouteLocked(cfg, "", requested, actual, actual, target, rule, now)
}

// applyResponseRuleForRouteLocked records the hit in the protection log only:
// probe history stays in results, so supplier protection and probe detection
// remain independent. routeModel isolates the full Magpie model id selected by
// the supplier request while the log keeps the vendor model actually returned.
func (s *Service) applyResponseRuleForRouteLocked(cfg GuardConfig, providerID, requested, actual, routeModel string, target *Target, rule ResponseModelRule, now time.Time) (bool, error) {
	key := responseHitKey(requested, actual, rule)
	if last, ok := s.responseHits[key]; ok && now.Sub(last) < time.Minute {
		return false, nil
	}
	if len(s.responseHits) > 512 {
		for k, at := range s.responseHits {
			if now.Sub(at) > 10*time.Minute {
				delete(s.responseHits, k)
			}
		}
	}
	detail := fmt.Sprintf("业务回复模型 %s 命中规则 %s", actual, rule.Pattern)
	targetModel := requested
	if target != nil {
		targetModel = target.Model
	}
	needsSync := false
	var err error
	switch rule.Action {
	case ResponseActionDisable:
		if target == nil {
			detail += "，未找到监测目标，未阻断"
			break
		}
		hold := time.Duration(cfg.ResponseHoldMin) * time.Minute
		detail += fmt.Sprintf("，已阻断目标 %d 分钟", cfg.ResponseHoldMin)
		err = s.holdTargetForResponse(target, detail, hold, now)
		if errors.Is(err, errTargetMissing) {
			err = nil
			detail = fmt.Sprintf("业务回复模型 %s 命中规则 %s，未找到监测目标，未阻断", actual, rule.Pattern)
		}
	case ResponseActionReroute:
		detail += "，已加入后续请求改道"
		err = s.store.AddResponseReroute(routeModel, now.Add(time.Duration(cfg.ResponseRerouteMin)*time.Minute))
		needsSync = true
	case ResponseActionRecord:
		detail += "，仅记录"
	}
	if err != nil {
		// The async caller has no supplier-side logger; record its failure here.
		// Supplier callers log their own pre/post errors and pass providerID.
		if providerID == "" {
			s.logSupplierEventLocked(&SupplierEvent{
				Kind:        "error",
				Model:       targetModel,
				ActualModel: actual,
				Action:      rule.Action,
				RuleID:      rule.ID,
				Detail:      "规则动作提交失败：" + err.Error(),
				CreatedAt:   now,
			})
		}
		return false, err
	}
	s.logSupplierEventLocked(&SupplierEvent{
		Kind:        "action",
		ProviderID:  providerID,
		Model:       targetModel,
		ActualModel: actual,
		Action:      rule.Action,
		RuleID:      rule.ID,
		Detail:      detail,
		CreatedAt:   now,
	})
	// The dedupe entry is written only after the action and its log persist, so
	// a failed submission can be retried by the next matching reply.
	s.responseHits[key] = now
	return needsSync, nil
}

// logSupplierEventLocked appends one protection log entry. Callers hold s.mu;
// logging must never fail the judged request, so a write error only surfaces
// in Health.
func (s *Service) logSupplierEventLocked(ev *SupplierEvent) {
	if ev == nil {
		return
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	if err := s.store.AppendSupplierEvent(ev); err != nil {
		s.health = "保护日志写入失败：" + err.Error()
	}
}

// logSupplierEvent appends one protection log entry outside s.mu.
func (s *Service) logSupplierEvent(ev *SupplierEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logSupplierEventLocked(ev)
}

func responseHitKey(requested, actual string, rule ResponseModelRule) string {
	return requested + "|" + actual + "|" + rule.ID + "|" + rule.Action + "|" + rule.Pattern
}

// supplierReplyIDPrefixes are the protocol wrappers Magpie's native renderers
// add to an upstream reply id. A vendor reply can be translated to another
// client protocol (chatcmpl-… becomes msg_… and so on), so both the supplier
// inspector and the async hook normalize the id before comparison.
var supplierReplyIDPrefixes = [...]string{"chatcmpl-", "cmpl-", "resp_", "msg_"}

// supplierReplyIDKey returns the prefix-normalized reply id, or "" when the id
// is empty or longer than any real vendor id. Prefixes are stripped repeatedly
// because a protocol translator can stack them (resp_msg_…).
func supplierReplyIDKey(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > supplierReplyIDLength {
		return ""
	}
	for {
		trimmed := false
		for _, prefix := range supplierReplyIDPrefixes {
			if strings.HasPrefix(id, prefix) {
				id = id[len(prefix):]
				trimmed = true
				break
			}
		}
		if !trimmed {
			return id
		}
		if id == "" {
			return ""
		}
	}
}

// markSupplierReply records a reply id seen on a response Guard judged
// synchronously through a protected supplier. Callers must not hold s.mu.
func (s *Service) markSupplierReply(id string) {
	key := supplierReplyIDKey(id)
	if key == "" {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.supplierReplies == nil {
		s.supplierReplies = map[string]time.Time{}
	}
	for k, at := range s.supplierReplies {
		if now.Sub(at) > supplierReplyTTL {
			delete(s.supplierReplies, k)
		}
	}
	if _, exists := s.supplierReplies[key]; !exists {
		for len(s.supplierReplies) >= supplierReplyLimit {
			oldestKey := ""
			var oldest time.Time
			for k, at := range s.supplierReplies {
				if oldestKey == "" || at.Before(oldest) {
					oldestKey, oldest = k, at
				}
			}
			if oldestKey == "" {
				break
			}
			delete(s.supplierReplies, oldestKey)
		}
	}
	s.supplierReplies[key] = now
}

// supplierReplyObserved reports whether a matching supplier reply was judged
// synchronously within the registry window.
func (s *Service) supplierReplyObserved(id string) bool {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.supplierReplyObservedLocked(id, now)
}

// supplierReplyObservedLocked is the lookup for callers already holding s.mu,
// so the async path can combine it with its config snapshot.
func (s *Service) supplierReplyObservedLocked(id string, now time.Time) bool {
	key := supplierReplyIDKey(id)
	if key == "" {
		return false
	}
	at, ok := s.supplierReplies[key]
	if !ok {
		return false
	}
	if now.Sub(at) > supplierReplyTTL {
		delete(s.supplierReplies, key)
		return false
	}
	return true
}

// supplierGroupOrAutoRequest reports whether the original request model is a
// routing group or auto alias rather than a provider-scoped model id.
func supplierGroupOrAutoRequest(model string) bool {
	return strings.HasPrefix(model, "group/") || strings.HasPrefix(model, "auto/")
}

// HandleResponseModel applies business-reply rules reported by Magpie's async
// response hook. Actions are applied off the hook's critical path. A reply id
// already observed on a protected supplier response is a duplicate of that
// synchronous judgment, including group requests whose original model name
// hides the resolved vendor. While any supplier protection is active, an
// uncorrelated group/auto reply is skipped: the hook cannot name the supplier
// that served it, and guessing could apply a second destructive action.
func (s *Service) HandleResponseModel(ev ResponseModelEvent) error {
	requested := strings.TrimSpace(ev.RequestedModel)
	requested = strings.TrimSpace(strings.TrimPrefix(requested, "magpie/"))
	if strings.HasPrefix(requested, ProbeAliasPrefix) {
		return nil
	}
	actual := strings.TrimSpace(ev.ActualModel)
	if !ev.Success || requested == "" || actual == "" {
		return nil
	}
	responseID := strings.TrimSpace(ev.ResponseID)
	now := time.Now().UTC()
	s.mu.Lock()
	cfg, err := s.store.LoadConfig()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if responseID != "" {
		if s.supplierReplyObservedLocked(responseID, now) {
			s.mu.Unlock()
			return nil
		}
	}
	if s.suppliers != nil && supplierGroupOrAutoRequest(requested) {
		// A group can resolve to any protected supplier. Without a matched
		// reply id the hook does not say which one served it; suppressing the
		// async action is safer than applying a second destructive action.
		active, aerr := s.suppliers.active()
		if aerr != nil || active {
			s.mu.Unlock()
			return aerr
		}
	}
	if s.suppliers != nil {
		provider, _, _ := strings.Cut(requested, "/")
		protected, err := s.suppliers.protectedProvider(provider)
		if err != nil || protected {
			s.mu.Unlock()
			return err
		}
	}
	targets, err := s.store.Targets()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	target := findTargetForRequest(targets, requested)
	if target == nil {
		target = findTargetForRequest(targets, actual)
	}
	rule := MatchResponseRule(effectiveResponseRules(cfg, target), actual)
	if rule == nil {
		s.mu.Unlock()
		return nil
	}
	needsSync, err := s.applyResponseRuleLocked(cfg, requested, actual, target, *rule, now)
	s.mu.Unlock()
	if err != nil {
		s.mu.Lock()
		s.health = "响应模型动作失败：" + err.Error()
		s.mu.Unlock()
		return err
	}
	if needsSync {
		go s.syncRoutingNow()
	}
	return nil
}

// ConfigureResponseHook records the local API origin and control token used
// by the Magpie async response hook and writes the initial handshake. A write
// failure must abort startup: without it the hook cannot observe replies.
func (s *Service) ConfigureResponseHook(path, origin, control string) error {
	s.hookMu.Lock()
	s.hookPath, s.hookOrigin, s.hookControl = path, origin, control
	s.hookMu.Unlock()
	return s.RefreshResponseHook()
}

// RefreshResponseHook rewrites the hook handshake after rules or targets
// change, so the hook can skip requests when no judgment applies.
func (s *Service) RefreshResponseHook() error {
	cfg, err := s.store.LoadConfig()
	if err != nil {
		return err
	}
	targets, err := s.store.Targets()
	if err != nil {
		return err
	}
	return s.writeResponseHookLocked(cfg, targets)
}

// writeResponseHookLocked writes the handshake for cfg. Callers hold s.mu (or
// are serializing a mode change); it only locks hookMu and never reloads state.
// Provider mode owns business-reply judgment, so async observation is disabled
// while the provider endpoint is enabled.
func (s *Service) writeResponseHookLocked(cfg GuardConfig, targets []*Target) error {
	s.hookMu.Lock()
	path, origin, control := s.hookPath, s.hookOrigin, s.hookControl
	s.hookMu.Unlock()
	if path == "" || origin == "" || control == "" {
		return nil
	}
	b, err := json.Marshal(ResponseModelHook{
		Origin:  origin,
		Control: control,
		Enabled: hasResponseRules(cfg, targets),
	})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return atomicWriteFile(path, b, 0o600)
}

// atomicWrite replaces a file atomically via temp+rename.
func atomicWrite(name string, data []byte) error {
	dir := filepath.Dir(name)
	f, err := os.CreateTemp(dir, ".model-guard-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o644); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// ---- probe round ----

// runRound runs one detection round for a target:
// In staged mode, it handles single-round retries, retesting failed prompts without false passes.
// In fixed mode, it retains immediate independent secondary retry.
func (s *Service) runRound(ctx context.Context, t *Target, cfg GuardConfig, source string) (*Result, error) {
	defer s.markRunResultSaved(t.Model)
	prompts, err := s.store.Prompts()
	if err != nil {
		return nil, err
	}
	var byID = map[string]*Prompt{}
	for _, p := range prompts {
		byID[p.ID] = p
	}

	mode := EffectiveScheduleMode(t.ScheduleMode, cfg.ScheduleMode)
	now := time.Now().UTC()

	if mode == ScheduleModeStaged {
		if t.RetryPromptID != "" {
			p, ok := byID[t.RetryPromptID]
			if !ok || !p.Automatic {
				// A deleted or non-automatic retry prompt must never fake a pass.
				verdict := VerdictUnavailable
				detail := fmt.Sprintf("复测题目 %s 已被删除或非自动题目，无法判定通过", t.RetryPromptID)
				final := &Result{
					ID:        randomHex(8),
					Model:     t.Model,
					PromptID:  t.RetryPromptID,
					Answer:    "",
					Verdict:   verdict,
					Detail:    detail,
					Tokens:    0,
					CreatedAt: now,
					Source:    source,
				}
				if err := s.store.AppendResult(final); err != nil {
					return final, err
				}
				s.markRunResultSaved(t.Model)
				s.applyRoundResult(t, verdict, detail, cfg, now, t.RetryPromptID)
				return final, nil
			}

			res, err := s.probeOnce(ctx, t, p, cfg, source)
			now = time.Now().UTC()
			tokens := 0
			answer := ""
			if res != nil {
				tokens = res.Tokens
				answer = res.Answer
			}
			var verdict string
			var detail string
			if err != nil || res == nil || (res.Verdict != VerdictPassed && res.Verdict != VerdictFailed) {
				verdict = VerdictUnavailable
				if res != nil && res.Detail != "" {
					detail = "复测不可用：" + res.Detail
				} else {
					detail = "复测不可用：" + errReason(err)
				}
			} else if res.Verdict == VerdictFailed {
				verdict = VerdictFailed
				detail = fmt.Sprintf("复测题目 %s 未通过", p.Name)
			} else {
				verdict = VerdictPassed
				detail = fmt.Sprintf("复测题目 %s 判分通过", p.Name)
			}

			final := &Result{
				ID:        randomHex(8),
				Model:     t.Model,
				PromptID:  p.ID,
				Answer:    answer,
				Verdict:   verdict,
				Detail:    detail,
				Tokens:    tokens,
				CreatedAt: now,
				Source:    source,
			}
			if err := s.store.AppendResult(final); err != nil {
				return final, err
			}
			s.markRunResultSaved(t.Model)
			s.applyRoundResult(t, verdict, detail, cfg, now, t.RetryPromptID)
			return final, nil
		}

		// RetryPromptID is empty: normal prompt selection
		promptIDs := t.PromptIDs
		if len(promptIDs) == 0 {
			for _, p := range prompts {
				if p.Automatic {
					promptIDs = append(promptIDs, p.ID)
				}
			}
		}
		var automaticPrompts []*Prompt
		for _, id := range promptIDs {
			p, ok := byID[id]
			if ok && p.Automatic {
				automaticPrompts = append(automaticPrompts, p)
			}
		}
		if len(automaticPrompts) == 0 {
			verdict := VerdictUnavailable
			detail := "目标未配置自动判分题目"
			final := &Result{
				ID:        randomHex(8),
				Model:     t.Model,
				PromptID:  strings.Join(promptIDs, "+"),
				Answer:    "",
				Verdict:   verdict,
				Detail:    detail,
				Tokens:    0,
				CreatedAt: now,
				Source:    source,
			}
			if err := s.store.AppendResult(final); err != nil {
				return final, err
			}
			s.markRunResultSaved(t.Model)
			s.applyRoundResult(t, verdict, detail, cfg, now)
			return final, nil
		}

		var (
			verdict        = VerdictPassed
			detail         = "所有自动题目检测通过"
			tokens         int
			firstRes       *Result
			failedPromptID string
		)
		for _, p := range automaticPrompts {
			res, err := s.probeOnce(ctx, t, p, cfg, source)
			if res != nil && firstRes == nil {
				firstRes = res
			}
			if res != nil {
				tokens += res.Tokens
			}
			if err != nil || res == nil || (res.Verdict != VerdictPassed && res.Verdict != VerdictFailed) {
				verdict = VerdictUnavailable
				if res != nil && res.Detail != "" {
					detail = "检测不可用：" + res.Detail
				} else {
					detail = "检测不可用：" + errReason(err)
				}
				failedPromptID = p.ID
				break
			}
			if res.Verdict == VerdictFailed {
				verdict = VerdictFailed
				detail = fmt.Sprintf("题目 %s 未通过", p.Name)
				failedPromptID = p.ID
				break
			}
		}
		now = time.Now().UTC()
		final := &Result{
			ID:        randomHex(8),
			Model:     t.Model,
			PromptID:  strings.Join(promptIDs, "+"),
			Answer:    "",
			Verdict:   verdict,
			Detail:    detail,
			Tokens:    tokens,
			CreatedAt: now,
			Source:    source,
		}
		if firstRes != nil {
			final.Answer = firstRes.Answer
			final.PromptID = firstRes.PromptID
		}
		if err := s.store.AppendResult(final); err != nil {
			return final, err
		}
		s.markRunResultSaved(t.Model)
		s.applyRoundResult(t, verdict, detail, cfg, now, failedPromptID)
		return final, nil
	}

	// Mode is ScheduleModeFixed
	promptIDs := t.PromptIDs
	if len(promptIDs) == 0 {
		for _, p := range prompts {
			if p.Automatic {
				promptIDs = append(promptIDs, p.ID)
			}
		}
	}
	var (
		verdict  = VerdictPassed
		detail   string
		tokens   int
		firstRes *Result
	)
	for _, id := range promptIDs {
		p, ok := byID[id]
		if !ok {
			continue
		}
		if !p.Automatic {
			continue // manual prompts never gate; run them via the manual API
		}
		res, err := s.probeOnce(ctx, t, p, cfg, source)
		if res != nil && firstRes == nil {
			firstRes = res
		}
		if err != nil || res == nil || (res.Verdict != VerdictPassed && res.Verdict != VerdictFailed) {
			verdict = VerdictUnavailable
			if res != nil && res.Detail != "" {
				detail = "检测不可用：" + res.Detail
			} else {
				detail = "检测不可用：" + errReason(err)
			}
			break
		}
		tokens += res.Tokens
		if res.Verdict == VerdictFailed {
			// Independent retry once, like intelligence-check's milk re-probe.
			retry, rerr := s.probeOnce(ctx, t, p, cfg, source)
			if rerr != nil || retry == nil || (retry.Verdict != VerdictPassed && retry.Verdict != VerdictFailed) {
				verdict = VerdictUnavailable
				if retry != nil && retry.Detail != "" {
					detail = "复测不可用：" + retry.Detail
				} else {
					detail = "复测不可用：" + errReason(rerr)
				}
				break
			}
			tokens += retry.Tokens
			if retry.Verdict == VerdictFailed {
				verdict = VerdictFailed
				detail = fmt.Sprintf("题目 %s 连续两次未通过", p.Name)
				break
			}
		}
	}
	if verdict == VerdictPassed && firstRes == nil {
		// No automatic prompt configured: cannot judge.
		verdict = VerdictUnavailable
		detail = "目标未配置自动判分题目"
	}
	final := &Result{
		ID:        randomHex(8),
		Model:     t.Model,
		PromptID:  strings.Join(promptIDs, "+"),
		Answer:    "",
		Verdict:   verdict,
		Detail:    detail,
		Tokens:    tokens,
		CreatedAt: now,
		Source:    source,
	}
	if firstRes != nil {
		final.Answer = firstRes.Answer
		final.PromptID = firstRes.PromptID
	}
	if err := s.store.AppendResult(final); err != nil {
		return final, err
	}
	s.markRunResultSaved(t.Model)
	s.applyRoundResult(t, verdict, detail, cfg, now)
	return final, nil
}

// applyRoundResult updates the target state machine and publishes the gate.
func (s *Service) applyRoundResult(t *Target, verdict, detail string, cfg GuardConfig, now time.Time, failedPromptID ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := s.store.Target(t.Model)
	if err != nil {
		s.health = err.Error()
		return
	}
	if latest == nil {
		return // A completed probe must not recreate a deleted target.
	}

	currentCfg, err := s.store.LoadConfig()
	if err != nil {
		s.health = err.Error()
		return
	}

	startMode := EffectiveScheduleMode(t.ScheduleMode, cfg.ScheduleMode)
	currentMode := EffectiveScheduleMode(latest.ScheduleMode, currentCfg.ScheduleMode)

	if startMode != currentMode {
		// Mode changed while probe was in flight! Discard state writeback to avoid
		// applying mismatched mode transitions (e.g. single error in staged being treated as fixed).
		// Still record CheckedAt so target reflects recent probe activity without corrupting stage/state.
		latest.CheckedAt = now
		if err := s.store.SaveTarget(latest); err != nil {
			s.health = err.Error()
		}
		return
	}

	nt := *latest
	nt.CheckedAt = now
	nt.Reason = detail

	if currentMode == ScheduleModeStaged {
		failedID := ""
		if len(failedPromptID) > 0 {
			failedID = failedPromptID[0]
		}
		policy := ResolveTimingPolicy(nt.TimingPolicy, currentCfg.TimingPolicy)
		out := ApplyStagedTransition(StagedTransitionInput{
			CurrentStage:      nt.ScheduleStage,
			CurrentState:      nt.State,
			FailStreak:        nt.FailStreak,
			RecoveryPasses:    nt.RecoveryPasses,
			IsolationRechecks: nt.IsolationRechecks,
			RetryPromptID:     nt.RetryPromptID,
			FailedPromptID:    failedID,
			Verdict:           verdict,
			Detail:            detail,
			Policy:            policy,
			Now:               now,
		})
		nt.ScheduleStage = out.NextStage
		nt.State = out.NextState
		nt.FailStreak = out.FailStreak
		nt.RecoveryPasses = out.RecoveryPasses
		nt.IsolationRechecks = out.IsolationRechecks
		nt.RetryPromptID = out.RetryPromptID
		nt.NextCheckAt = out.NextCheckAt
		// Concurrent user edit / manual hold takes priority!
		nt.ManualHold = latest.ManualHold
		nt.AllowUntil = latest.AllowUntil
	} else {
		nt.ScheduleStage = DefaultStageForState(nt.State)
		nt.RecoveryPasses = 0
		nt.IsolationRechecks = 0
		nt.RetryPromptID = ""
		switch verdict {
		case VerdictPassed:
			nt.State = StatePassed
			nt.FailStreak = 0
			nt.ScheduleStage = ScheduleStageNormal
			nt.NextCheckAt = now.Add(time.Duration(nt.IntervalMin) * time.Minute)
		case VerdictFailed:
			nt.State = StateFailed
			nt.FailStreak++
			nt.ScheduleStage = ScheduleStageIsolated
			wait := nt.FailRecheckMin
			if wait <= 0 {
				wait = 60 // default failed recheck: 1 hour
			}
			nt.NextCheckAt = now.Add(time.Duration(wait) * time.Minute)
		case VerdictUnavailable:
			nt.State = StateUnavailable
			nt.ScheduleStage = ScheduleStageUnavailable
			cool := currentCfg.CoolMin
			if cool <= 0 {
				cool = 15
			}
			nt.NextCheckAt = now.Add(time.Duration(cool) * time.Minute)
		default:
			return
		}
		nt.ManualHold = latest.ManualHold
		nt.AllowUntil = latest.AllowUntil
	}

	if err := s.store.SaveTarget(&nt); err != nil {
		s.health = err.Error()
		return
	}
	if err := s.publishGate(); err != nil {
		s.health = err.Error()
	}
}

// probeOnce sends one probe request and grades it.
func (s *Service) probeOnce(ctx context.Context, t *Target, p *Prompt, cfg GuardConfig, source string) (*Result, error) {
	if cfg.GatewayURL == "" {
		return nil, errors.New("未配置网关")
	}
	if n, err := s.store.CountProbesToday(t.Model, time.Now()); err == nil && n >= cfg.DailyLimit {
		return nil, errors.New("今日探针预算已用尽")
	}
	alias, err := s.store.Alias()
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var standalone *runningProbe
	s.mu.Lock()
	if rp := s.running[t.Model]; rp != nil {
		rp.promptID = p.ID
		rp.detail = fmt.Sprintf("正在检测题目 %s，等待模型响应…", p.Name)
		rp.startedAt = time.Now().UTC()
		rp.active = true
	} else {
		standalone = &runningProbe{
			id:        "checking-" + randomHex(8),
			model:     t.Model,
			promptID:  p.ID,
			detail:    fmt.Sprintf("正在检测题目 %s，等待模型响应…", p.Name),
			source:    source,
			startedAt: time.Now().UTC(),
			active:    true,
		}
		s.running[t.Model] = standalone
	}
	s.mu.Unlock()
	if standalone != nil {
		defer s.finishRun(t.Model, standalone)
	}
	answer, tokens, err := Call(pctx, ProbeConfig{GatewayURL: cfg.GatewayURL, GatewayKey: cfg.GatewayKey},
		ProbeAliasPrefix+alias+"/"+t.Model, p.Text, nil)

	res := &Result{
		ID:        randomHex(8),
		Model:     t.Model,
		PromptID:  p.ID,
		Answer:    answer,
		Verdict:   VerdictManual,
		Tokens:    tokens,
		CreatedAt: time.Now().UTC(),
		Source:    source,
	}
	if err != nil {
		res.Verdict = VerdictUnavailable
		res.Detail = errReason(err)
		if answer == "" {
			res.Answer = ""
		}
		_ = s.store.AppendResult(res)
		return res, err
	}
	passed, manual, gerr := p.Grade(answer)
	switch {
	case gerr != nil:
		res.Verdict = VerdictUnavailable
		res.Detail = "判分错误：" + gerr.Error()
	case manual:
		res.Verdict = VerdictManual
	default:
		if passed {
			res.Verdict = VerdictPassed
		} else {
			res.Verdict = VerdictFailed
		}
	}
	_ = s.store.AppendResult(res)
	return res, nil
}

func errReason(err error) string {
	if err == nil {
		return "未知错误"
	}
	return err.Error()
}

// ---- public operations used by the API ----

// Snapshot returns a deep-copy view of state for the console.
func (s *Service) Snapshot() (map[string]any, error) {
	cfg, err := s.store.LoadConfig()
	if err != nil {
		return nil, err
	}
	cfg.GatewayKey = ""
	targets, err := s.store.Targets()
	if err != nil {
		return nil, err
	}
	prompts, err := s.store.Prompts()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	routing := s.routing
	checking := make([]string, 0, len(s.running))
	for model, rp := range s.running {
		if rp != nil && rp.active {
			checking = append(checking, model)
		}
	}
	s.mu.Unlock()
	sort.Strings(checking)
	codexSummary := map[string]any{}
	if s.codex != nil {
		if ct, err := s.store.CodexAccountTargets(); err == nil {
			monitored, isolated := 0, 0
			for _, t := range ct {
				if t.Enabled {
					monitored++
				}
				if t.Isolated {
					isolated++
				}
			}
			codexSummary = map[string]any{
				"monitored": monitored,
				"isolated":  isolated,
				"error":     s.codex.errorText(),
			}
		}
	}
	return map[string]any{
		"config":         cfg,
		"targets":        targets,
		"prompts":        prompts,
		"checking":       checking,
		"health":         s.Health(),
		"routing":        routing,
		"codex_accounts": codexSummary,
	}, nil
}

// Results lists one page of probe results: active in-flight probes (first page
// only) followed by persisted results, optionally filtered by model and prompt,
// resuming after the before cursor. It returns the cursor for the next page, or
// "" when the list has no more rows.
func (s *Service) Results(model, promptID string, limit int, before string) ([]*Result, string, error) {
	limit = pageLimit(limit, 100, 500)
	var active []*Result
	if before == "" {
		s.mu.Lock()
		for _, rp := range s.running {
			if rp == nil || !rp.active {
				continue
			}
			if model != "" && rp.model != model {
				continue
			}
			if promptID != "" && rp.promptID != promptID {
				continue
			}
			active = append(active, &Result{
				ID:        rp.id,
				Model:     rp.model,
				PromptID:  rp.promptID,
				Answer:    "",
				Verdict:   StateChecking,
				Detail:    rp.detail,
				Tokens:    0,
				CreatedAt: rp.startedAt,
				Source:    rp.source,
			})
		}
		s.mu.Unlock()

		sort.Slice(active, func(i, j int) bool {
			if !active[i].CreatedAt.Equal(active[j].CreatedAt) {
				return active[i].CreatedAt.After(active[j].CreatedAt)
			}
			return active[i].Model < active[j].Model
		})
		if len(active) >= limit {
			stored, _, err := s.store.Results(model, promptID, 1, "")
			if err != nil {
				return nil, "", err
			}
			next := ""
			if len(stored) > 0 {
				next = "history"
			}
			return active[:limit], next, nil
		}
	}
	// A page filled by in-flight probes has not consumed any stored rows.
	if before == "history" {
		before = ""
	}

	stored, next, err := s.store.Results(model, promptID, limit-len(active), before)
	if err != nil {
		return nil, "", err
	}
	out := make([]*Result, 0, len(active)+len(stored))
	out = append(out, active...)
	out = append(out, stored...)
	return out, next, nil
}

// targetIntervalMin calculates the scheduled interval in minutes.
func (s *Service) targetIntervalMin(t *Target, cfg GuardConfig) int {
	mode := EffectiveScheduleMode(t.ScheduleMode, cfg.ScheduleMode)
	if mode == ScheduleModeStaged {
		policy := ResolveTimingPolicy(t.TimingPolicy, cfg.TimingPolicy)
		stage := t.ScheduleStage
		if stage == "" {
			stage = DefaultStageForState(t.State)
		}
		switch stage {
		case ScheduleStageNormal:
			return policy.NormalMin
		case ScheduleStageRetry:
			return policy.ErrorRetryMin
		case ScheduleStageIsolated:
			if t.IsolationRechecks > 0 {
				return policy.IsolationLaterMin
			}
			return policy.IsolationFirstMin
		case ScheduleStageRecovery:
			return policy.RecoveryConfirmMin
		case ScheduleStageUnavailable:
			return policy.UnavailableMin
		default:
			return policy.NormalMin
		}
	}
	switch t.State {
	case StateFailed:
		if t.FailRecheckMin > 0 {
			return t.FailRecheckMin
		}
		return 60
	case StateUnavailable:
		if cfg.CoolMin > 0 {
			return cfg.CoolMin
		}
		return 15
	default:
		if t.IntervalMin > 0 {
			return t.IntervalMin
		}
		if cfg.DefaultIntervalMin > 0 {
			return cfg.DefaultIntervalMin
		}
		return 30
	}
}

// UpsertTarget validates and saves a target, then publishes the gate.
func (s *Service) UpsertTarget(t *Target) error {
	return s.UpsertTargetOpt(t, false)
}

// UpsertTargetOpt validates and saves a target with optional recalculation of schedule.
func (s *Service) UpsertTargetOpt(t *Target, recalculate bool) error {
	t.Model = strings.TrimSpace(t.Model)
	if t.Model == "" || len(t.Model) > 512 || strings.ContainsAny(t.Model, "\r\n") || strings.HasPrefix(t.Model, ProbeAliasPrefix) {
		return errors.New("无效模型 ID")
	}
	if err := ValidateScheduleMode(t.ScheduleMode, true); err != nil {
		return err
	}
	if err := ValidateTargetTimingPolicy(t.TimingPolicy); err != nil {
		return err
	}
	if t.IntervalMin <= 0 {
		t.IntervalMin = 30
	}
	if t.IntervalMin > MaxScheduleMin {
		t.IntervalMin = MaxScheduleMin
	}
	if t.FailRecheckMin < 0 {
		t.FailRecheckMin = 0
	}
	if t.FailRecheckMin > MaxScheduleMin {
		t.FailRecheckMin = MaxScheduleMin
	}
	for _, id := range t.PromptIDs {
		if strings.ContainsAny(id, "\r\n") || len(id) > 128 {
			return errors.New("无效题目 ID")
		}
	}
	if !validResponseRuleMode(t.ResponseRuleMode) {
		return errors.New("响应模型规则模式无效")
	}
	if t.ResponseRuleMode == ResponseRuleCustom {
		rules, err := NormalizeResponseRules(t.ResponseModelRules)
		if err != nil {
			return err
		}
		t.ResponseModelRules = rules
	} else {
		t.ResponseModelRules = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.store.LoadConfig()
	if err != nil {
		return err
	}
	existing, err := s.store.Target(t.Model)
	if err != nil {
		return err
	}
	if t.ResponseRuleMode == "" {
		if existing != nil {
			t.ResponseRuleMode = existing.ResponseRuleMode
			t.ResponseModelRules = existing.ResponseModelRules
		} else {
			t.ResponseRuleMode = ResponseRuleInherit
			t.ResponseModelRules = nil
		}
	}
	if existing != nil {
		oldMode := EffectiveScheduleMode(existing.ScheduleMode, cfg.ScheduleMode)
		newMode := EffectiveScheduleMode(t.ScheduleMode, cfg.ScheduleMode)
		if oldMode != newMode {
			// Mode switch resets counters but cannot release existing block
			t.FailStreak = 0
			t.RecoveryPasses = 0
			t.IsolationRechecks = 0
			t.RetryPromptID = ""
			t.ManualHold = existing.ManualHold
			t.RuleHoldUntil = existing.RuleHoldUntil
			t.AllowUntil = existing.AllowUntil
			t.CheckedAt = existing.CheckedAt
			t.Reason = existing.Reason
			t.State = existing.State
			if newMode == ScheduleModeStaged {
				if existing.State == StateFailed || existing.State == StateReview {
					t.ScheduleStage = ScheduleStageIsolated
				} else if existing.State == StateUnavailable {
					t.ScheduleStage = ScheduleStageUnavailable
				} else {
					t.ScheduleStage = ScheduleStageNormal
				}
			} else {
				t.ScheduleStage = DefaultStageForState(existing.State)
			}
		} else {
			// preserve runtime state on re-save
			t.State = existing.State
			t.CheckedAt = existing.CheckedAt
			t.AllowUntil = existing.AllowUntil
			t.FailStreak = existing.FailStreak
			t.ManualHold = existing.ManualHold
			t.RuleHoldUntil = existing.RuleHoldUntil
			t.Reason = existing.Reason
			t.ScheduleStage = existing.ScheduleStage
			t.RecoveryPasses = existing.RecoveryPasses
			t.IsolationRechecks = existing.IsolationRechecks
			t.RetryPromptID = existing.RetryPromptID
			if t.ScheduleStage == "" {
				t.ScheduleStage = DefaultStageForState(t.State)
			}
		}
		if !recalculate {
			t.NextCheckAt = existing.NextCheckAt
		} else {
			if _, busy := s.running[t.Model]; busy {
				t.NextCheckAt = existing.NextCheckAt
			} else {
				interval := s.targetIntervalMin(t, cfg)
				t.NextCheckAt = time.Now().UTC().Add(time.Duration(interval) * time.Minute)
			}
		}
	} else {
		if t.State == "" {
			t.State = StateUnknown
		}
		if t.ScheduleStage == "" {
			t.ScheduleStage = DefaultStageForState(t.State)
		}
		if t.NextCheckAt.IsZero() {
			t.NextCheckAt = time.Now().UTC()
		} else if recalculate {
			if _, busy := s.running[t.Model]; !busy {
				interval := s.targetIntervalMin(t, cfg)
				t.NextCheckAt = time.Now().UTC().Add(time.Duration(interval) * time.Minute)
			}
		}
	}
	if err := s.store.SaveTarget(t); err != nil {
		return err
	}
	if err := s.RefreshResponseHook(); err != nil {
		return err
	}
	return s.publishGate()
}

// DeleteTarget removes a target (gate stops blocking it).
func (s *Service) DeleteTarget(model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.DeleteTarget(model); err != nil {
		return err
	}
	if err := s.RefreshResponseHook(); err != nil {
		return err
	}
	return s.publishGate()
}

// TargetAction performs allow/hold/recheck on a target.
func (s *Service) TargetAction(model, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.store.Target(model)
	if err != nil || t == nil {
		return errors.New("模型未配置")
	}
	switch action {
	case "allow":
		t.AllowUntil = time.Now().UTC().Add(15 * time.Minute)
	case "hold", "disable":
		t.ManualHold = true
		t.AllowUntil = time.Time{}
	case "enable":
		t.ManualHold = false
		t.RuleHoldUntil = time.Time{}
		t.State = StateUnknown
		t.Reason = ""
		t.FailStreak = 0
		t.AllowUntil = time.Time{}
		t.ScheduleStage = ScheduleStageNormal
		t.RecoveryPasses = 0
		t.IsolationRechecks = 0
		t.RetryPromptID = ""
	case "monitor-off":
		t.Enabled = false
	case "monitor-on":
		t.Enabled = true
		t.NextCheckAt = time.Now().UTC()
	case "recheck":
		if s.closing {
			return errors.New("检测后台正在关闭")
		}
		if _, busy := s.running[model]; busy {
			return errors.New("该模型检测进行中")
		}
		cfg, err := s.store.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.GatewayURL == "" {
			return errors.New("请先配置网关")
		}
		s.launchRoundLocked(t, cfg, "manual")
		return nil
	default:
		return errors.New("未知操作")
	}
	if err := s.store.SaveTarget(t); err != nil {
		return err
	}
	return s.publishGate()
}

// Configure validates and saves gateway config. Zero-valued fields keep the
// saved value so partial updates from the console stay valid.
func (s *Service) Configure(c GuardConfig) error {
	return s.ConfigureOpt(c, false)
}

// ConfigureOpt validates and saves config with optional schedule recalculation.
func (s *Service) ConfigureOpt(c GuardConfig, recalculate bool) error {
	return s.configure(c, recalculate)
}

// configure merges and saves config under s.mu.
func (s *Service) configure(c GuardConfig, recalculate bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configureLocked(c, recalculate)
}

func (s *Service) configureLocked(c GuardConfig, recalculate bool) error {
	old, err := s.store.LoadConfig()
	if err != nil {
		return err
	}
	if c.GatewayKey == "" {
		c.GatewayKey = old.GatewayKey
	}
	if c.DailyLimit == 0 {
		c.DailyLimit = old.DailyLimit
	}
	if c.TimeoutSec == 0 {
		c.TimeoutSec = old.TimeoutSec
	}
	if c.CoolMin == 0 {
		c.CoolMin = old.CoolMin
	}
	if c.DefaultIntervalMin == 0 {
		c.DefaultIntervalMin = old.DefaultIntervalMin
	}
	if c.ResponseModelMode == "" {
		c.ResponseModelMode = old.ResponseModelMode
	}
	if c.ResponseModelMode == "" {
		c.ResponseModelMode = ResponseModelOff
	}
	if c.ResponseRerouteMin == 0 {
		c.ResponseRerouteMin = old.ResponseRerouteMin
	}
	if c.ResponseRerouteMin == 0 {
		c.ResponseRerouteMin = 30
	}
	if c.ResponseHoldMin == 0 {
		c.ResponseHoldMin = old.ResponseHoldMin
	}
	if c.ResponseHoldMin == 0 {
		c.ResponseHoldMin = 30
	}
	if c.ResponseRules == nil {
		c.ResponseRules = old.ResponseRules
	}
	if c.ProviderAttempts == 0 {
		c.ProviderAttempts = old.ProviderAttempts
	}
	if c.ProviderAttempts == 0 {
		c.ProviderAttempts = 2
	}
	if c.ScheduleMode == "" {
		c.ScheduleMode = old.ScheduleMode
	}
	if c.ScheduleMode == "" {
		c.ScheduleMode = ScheduleModeFixed
	}
	if err := ValidateScheduleMode(c.ScheduleMode, false); err != nil {
		return err
	}
	// Validate explicitly provided non-zero timing values BEFORE merging,
	// so out-of-range/negative values are rejected rather than silently replaced.
	if err := ValidateTargetTimingPolicy(&c.TimingPolicy); err != nil {
		return err
	}
	c.TimingPolicy = ResolveTimingPolicy(&c.TimingPolicy, old.TimingPolicy)
	if err := ValidateGlobalTimingPolicy(c.TimingPolicy); err != nil {
		return err
	}
	if c.DailyLimit < 1 || c.DailyLimit > 10000 {
		return errors.New("每日预算须为 1–10000 次")
	}
	if c.TimeoutSec < 10 || c.TimeoutSec > 600 {
		return errors.New("单次超时须为 10–600 秒")
	}
	if c.CoolMin < 1 || c.CoolMin > MaxScheduleMin {
		return fmt.Errorf("冷却时间须为 1–%d 分钟", MaxScheduleMin)
	}
	if c.DefaultIntervalMin < 1 || c.DefaultIntervalMin > MaxScheduleMin {
		return fmt.Errorf("默认检测间隔须为 1–%d 分钟", MaxScheduleMin)
	}
	if err := ValidateGateway(c.GatewayURL); err != nil {
		return err
	}
	if c.ResponseModelMode != ResponseModelOff && c.ResponseModelMode != ResponseModelOn {
		return errors.New("响应模型判断模式无效")
	}
	if c.ResponseRerouteMin < 1 || c.ResponseRerouteMin > 1440 {
		return errors.New("响应模型改道保持时间须为 1–1440 分钟")
	}
	if c.ResponseHoldMin < 1 || c.ResponseHoldMin > 1440 {
		return errors.New("规则阻断保持时间须为 1–1440 分钟")
	}
	if c.ProviderAttempts < 1 || c.ProviderAttempts > 5 {
		return errors.New("Provider 尝试次数须为 1–5 次")
	}
	rules, err := NormalizeResponseRules(c.ResponseRules)
	if err != nil {
		return err
	}
	c.ResponseRules = rules
	if c.GatewayKey == "" {
		return errors.New("请输入网关 Key；本机默认 Key 为 magpie")
	}
	targets, err := s.store.Targets()
	if err != nil {
		return err
	}
	if err := s.store.SaveConfig(c); err != nil {
		return err
	}
	// Mode and handshake move together: a late async event must never act on
	// the previous mode. Restore both when the handshake write fails.
	if err := s.writeResponseHookLocked(c, targets); err != nil {
		restoreErr := s.store.SaveConfig(old)
		hookErr := s.writeResponseHookLocked(old, targets)
		if restoreErr != nil || hookErr != nil {
			s.health = "配置回滚失败：" + errors.Join(restoreErr, hookErr).Error()
		}
		return fmt.Errorf("更新响应观察握手失败: %w", err)
	}
	now := time.Now().UTC()
	for _, tgt := range targets {
		oldMode := EffectiveScheduleMode(tgt.ScheduleMode, old.ScheduleMode)
		newMode := EffectiveScheduleMode(tgt.ScheduleMode, c.ScheduleMode)
		modeChanged := oldMode != newMode
		if modeChanged {
			// Mode switch always resets counters/stage, preserving block and NextCheckAt.
			tgt.FailStreak = 0
			tgt.RecoveryPasses = 0
			tgt.IsolationRechecks = 0
			tgt.RetryPromptID = ""
			if newMode == ScheduleModeStaged {
				if tgt.State == StateFailed || tgt.State == StateReview {
					tgt.ScheduleStage = ScheduleStageIsolated
				} else if tgt.State == StateUnavailable {
					tgt.ScheduleStage = ScheduleStageUnavailable
				} else {
					tgt.ScheduleStage = ScheduleStageNormal
				}
			} else {
				tgt.ScheduleStage = DefaultStageForState(tgt.State)
			}
		}
		_, busy := s.running[tgt.Model]
		rescheduled := recalculate && !busy
		if rescheduled {
			interval := s.targetIntervalMin(tgt, c)
			tgt.NextCheckAt = now.Add(time.Duration(interval) * time.Minute)
		}
		if modeChanged || rescheduled {
			if err := s.store.SaveTarget(tgt); err != nil {
				return err
			}
		}
	}
	return s.publishGate()
}

// ConfigureInitial only fills config when empty (auto-discovery). Once a
// gateway is configured it ignores the partial payload.
func (s *Service) ConfigureInitial(c GuardConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.store.LoadConfig()
	if err != nil {
		return err
	}
	if old.GatewayURL != "" {
		return nil
	}
	return s.configureLocked(c, false)
}

// RunManualProbe runs a prompt against a model immediately (any prompt,
// including manual ones), appending a result. It does not change target state
// unless the prompt is automatic and the target is in scheduled mode.
func (s *Service) RunManualProbe(model string, promptID string) (*Result, error) {
	t, err := s.store.Target(model)
	if err != nil || t == nil {
		return nil, errors.New("模型未配置")
	}
	prompts, err := s.store.Prompts()
	if err != nil {
		return nil, err
	}
	var p *Prompt
	for _, cand := range prompts {
		if cand.ID == promptID {
			p = cand
			break
		}
	}
	if p == nil {
		return nil, errors.New("题目不存在")
	}
	cfg, err := s.store.LoadConfig()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, errors.New("检测后台正在关闭")
	}
	if _, busy := s.running[model]; busy {
		s.mu.Unlock()
		return nil, errors.New("该模型检测进行中")
	}
	runCtx, cancel := context.WithTimeout(s.ctx, time.Duration(cfg.TimeoutSec+30)*time.Second)
	rp := &runningProbe{
		id:        "checking-" + randomHex(8),
		model:     model,
		promptID:  p.ID,
		detail:    fmt.Sprintf("正在检测题目 %s，等待模型响应…", p.Name),
		source:    "manual",
		startedAt: time.Now().UTC(),
		active:    true,
		cancel:    cancel,
	}
	s.running[model] = rp
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	defer s.finishRun(model, rp)
	res, err := s.probeOnce(runCtx, t, p, cfg, "manual")
	if err != nil {
		return res, err
	}
	return res, nil
}

// UpsertPrompt validates and saves a prompt. Builtin status is preserved by
// the server to prevent forgery.
func (s *Service) UpsertPrompt(p *Prompt) error {
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		p.ID = "custom-" + randomHex(4)
		p.Builtin = false
	} else {
		existing, err := s.store.Prompt(p.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			p.Builtin = existing.Builtin
		} else {
			p.Builtin = isBuiltinPromptID(p.ID)
		}
	}
	if err := p.Validate(); err != nil {
		return err
	}
	return s.store.SavePrompt(p)
}

// DeletePrompt removes a custom prompt.
func (s *Service) DeletePrompt(id string) error {
	p, err := s.store.Prompt(id)
	if err != nil {
		return err
	}
	if p != nil && p.Builtin {
		return errors.New("内置题目不可删除")
	}
	return s.store.DeletePrompt(id)
}

func isBuiltinPromptID(id string) bool {
	for _, b := range Builtins() {
		if b.ID == id {
			return true
		}
	}
	return false
}

// TargetsDueNow forces a scheduler pass (used after config changes).
func (s *Service) TargetsDueNow() { go s.schedule() }
