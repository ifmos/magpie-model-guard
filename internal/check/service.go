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

	running    map[string]*runningProbe // model -> active run
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	consoleMu  sync.Mutex
	consoleKey string
	health     string
	closing    bool
	routes     *groupController
	routing    GroupRoutingStatus
}

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
		store:   store,
		gate:    gatePath,
		running: map[string]*runningProbe{},
		ctx:     ctx,
		cancel:  cancel,
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
	return strings.TrimSpace(strings.Join([]string{s.health, s.routing.Error}, " "))
}

// NewConsoleToken rotates and returns the management token.
func (s *Service) NewConsoleToken() string {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	s.consoleKey = randomHex(24)
	return s.consoleKey
}

// CheckConsoleToken validates a bearer token.
func (s *Service) CheckConsoleToken(token string) bool {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	return s.consoleKey != "" && token == s.consoleKey
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
	ManualHold bool   `json:"manual_hold"`
	State      string `json:"state"`
	AllowUntil int64  `json:"allow_until"`
	Reason     string `json:"reason"`
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
			ManualHold: t.ManualHold,
			State:      t.State,
			AllowUntil: t.AllowUntil.Unix(),
			Reason:     t.Reason,
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
 const blocked=t.manual_hold||t.state==="failed"||t.state==="review"||t.state==="unavailable";
 if(!blocked)return body;
 const reason=t.manual_hold||t.state==="review"?"人工禁用":t.state==="unavailable"?"检测不可用（探针请求失败），已保守禁用":"降智检测未通过，模型已禁用";
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
	ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
	defer cancel()
	status, err := s.routes.sync(ctx, blocked)
	if err != nil {
		status.Error = "路由组同步失败：" + err.Error()
	}
	s.routing = status
	return err
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
	return map[string]any{
		"config":   cfg,
		"targets":  targets,
		"prompts":  prompts,
		"checking": checking,
		"health":   s.Health(),
		"routing":  routing,
	}, nil
}

// Results lists active in-flight probes followed by persisted probe results,
// optionally filtered by model and prompt.
func (s *Service) Results(model, promptID string, limit int) ([]*Result, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	s.mu.Lock()
	active := make([]*Result, 0, len(s.running))
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
		return active[:limit], nil
	}

	stored, err := s.store.Results(model, promptID, limit-len(active))
	if err != nil {
		return nil, err
	}
	out := make([]*Result, 0, len(active)+len(stored))
	out = append(out, active...)
	out = append(out, stored...)
	return out, nil
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
	return s.publishGate()
}

// DeleteTarget removes a target (gate stops blocking it).
func (s *Service) DeleteTarget(model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.DeleteTarget(model); err != nil {
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
	old, _ := s.store.LoadConfig()
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
	if c.GatewayKey == "" {
		return errors.New("请输入网关 Key；本机默认 Key 为 magpie")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.SaveConfig(c); err != nil {
		return err
	}
	targets, err := s.store.Targets()
	if err != nil {
		return err
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

// ConfigureInitial only fills config when empty (auto-discovery).
func (s *Service) ConfigureInitial(c GuardConfig) error {
	old, err := s.store.LoadConfig()
	if err != nil {
		return err
	}
	if old.GatewayURL != "" {
		return nil
	}
	return s.Configure(c)
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
