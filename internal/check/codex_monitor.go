package check

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// scheduleCodexAccounts launches at most one account probe at a time. The
// caller owns no Service lock.
func (s *Service) scheduleCodexAccounts() {
	s.mu.Lock()
	if s.closing || s.codex == nil || s.codexBusy {
		s.mu.Unlock()
		return
	}
	c := s.codex
	s.mu.Unlock()

	if err := c.syncRoster(); err != nil {
		c.setError(err)
		return
	}
	if err := c.syncAccountIsolation(s.ctx); err != nil {
		c.setError(err)
	}
	now := time.Now().UTC()
	due, err := c.store.DueCodexAccountTargets(now)
	if err != nil {
		c.setError(err)
		return
	}
	if len(due) == 0 {
		return
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].NextCheckAt.Equal(due[j].NextCheckAt) {
			return due[i].NextCheckAt.Before(due[j].NextCheckAt)
		}
		return due[i].AccountID < due[j].AccountID
	})
	t := due[0]
	s.mu.Lock()
	if s.closing || s.codexBusy {
		s.mu.Unlock()
		return
	}
	s.codexBusy = true
	s.mu.Unlock()
	s.launchCodexProbe(t.AccountID, false)
}

func (s *Service) launchCodexProbe(accountID string, manual bool) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.codexBusy = false
			s.mu.Unlock()
		}()
		if err := s.codex.runProbe(s.ctx, accountID, manual); err != nil {
			if t, berr := s.store.CodexAccountTarget(accountID); berr == nil && t != nil && t.State == CodexStateChecking {
				model := t.Model
				if t.Isolated {
					t.State = CodexStateIsolated
				} else {
					t.State = CodexStateUnknown
				}
				t.Reason = "检测中断，等待重试：" + err.Error()
				t.NextCheckAt = time.Now().UTC()
				_, _ = s.store.SaveCodexAccountProbeResult(t, model)
			}
			s.codex.setError(err)
		}
	}()
}

func (c *codexAccountController) recoverInterrupted() error {
	targets, err := c.store.CodexAccountTargets()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, t := range targets {
		if t == nil || t.State != CodexStateChecking {
			continue
		}
		if t.Isolated {
			t.State = CodexStateIsolated
		} else {
			t.State = CodexStateUnknown
		}
		t.Reason = "上次检测被中断，等待重试"
		t.NextCheckAt = now
		if err := c.store.SaveCodexAccountTarget(t); err != nil {
			return err
		}
	}
	return nil
}

func (c *codexAccountController) syncRoster() error {
	accounts, err := c.accounts(true)
	if err != nil {
		return err
	}
	live := make(map[string]CodexAccountInfo, len(accounts))
	for _, a := range accounts {
		live[strings.ToLower(a.ID)] = a
	}
	targets, err := c.store.CodexAccountTargets()
	if err != nil {
		return err
	}
	for _, t := range targets {
		if t == nil {
			continue
		}
		a, ok := live[strings.ToLower(t.AccountID)]
		missing := !ok
		user := t.User
		if ok {
			user = a.User
		}
		if missing != t.Missing || user != t.User {
			if err := c.store.UpdateCodexAccountRoster(t.AccountID, user, missing); err != nil {
				return err
			}
		}
	}
	return nil
}

func codexAutomaticPrompt(prompts []*Prompt, lastID string) *Prompt {
	if lastID != "" {
		for _, p := range prompts {
			if p != nil && p.ID == lastID && p.Automatic {
				return p
			}
		}
	}
	for _, p := range prompts {
		if p != nil && p.Automatic {
			return p
		}
	}
	return nil
}

// runProbe executes one account probe and applies the account-only isolation
// state machine. It never touches the model gate.
func (c *codexAccountController) runProbe(parent context.Context, accountID string, manual bool) error {
	t, err := c.store.CodexAccountTarget(accountID)
	if err != nil {
		return err
	}
	if t == nil || !t.Enabled || t.Missing {
		return errors.New("Codex 账号监测目标不存在、已停用或账号已消失")
	}
	if err := validateCodexModel(t.Model); err != nil {
		return err
	}
	accounts, err := c.accounts(true)
	if err != nil {
		return err
	}
	if !codexPinUnique(accounts, t.User) {
		return c.finishUnavailable(t, "存在同名 Codex 账号，无法唯一固定检测账号；请为账号设置不同名称后再试")
	}
	cfg, err := c.store.LoadConfig()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.GatewayURL) == "" {
		return errors.New("未配置网关")
	}
	if n, err := c.store.CountCodexProbesToday(accountID, time.Now()); err == nil && n >= cfg.DailyLimit {
		return c.finishUnavailable(t, "今日探针预算已用尽")
	}
	prompts, err := c.store.Prompts()
	if err != nil {
		return err
	}
	p := codexAutomaticPrompt(prompts, t.LastPromptID)
	if p == nil {
		return c.finishUnavailable(t, "没有可自动判分的检测题目")
	}
	now := time.Now().UTC()
	started, err := c.store.MarkCodexAccountChecking(t.AccountID, t.Model, "账号检测进行中，等待模型响应…", t.State, t.CheckedAt)
	if err != nil {
		return err
	}
	if !started {
		return errors.New("检测目标已变化，结果已丢弃")
	}
	t.State = CodexStateChecking
	t.Reason = "账号检测进行中，等待模型响应…"
	t.NextCheckAt = time.Time{}

	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	retestWasIsolated := t.Isolated
	probeWasTemporary := false
	if retestWasIsolated {
		// Reconcile first, but a failure for another target must not block
		// this account's retest if its own switch is ready.
		syncErr := c.syncAccountIsolation(parent)
		if syncErr != nil {
			c.setError(syncErr)
		}
		enabled, prepErr := c.beginProbe(parent, t)
		if prepErr == nil {
			probeWasTemporary = enabled
		} else {
			states, readErr := readCodexLoginStates(c.configDir)
			st, ok := states[strings.ToLower(t.AccountID)]
			if readErr != nil || !ok || !st.On || st.Paused {
				// The account is still switched off and its own isolation
				// could not be reconciled; keep it checking-free for a retry.
				t.State = CodexStateIsolated
				reason := prepErr
				if syncErr != nil {
					reason = syncErr
				}
				t.Reason = "复测前同步账号状态失败，等待重试：" + reason.Error()
				t.NextCheckAt = time.Now().UTC()
				_, _ = c.store.SaveCodexAccountProbeResult(t, t.Model)
				return reason
			}
			// Isolation could not be applied (for example the last account),
			// but the account is available: test it as it is.
		}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	answer, tokens, callErr := CallPinned(ctx, ProbeConfig{
		GatewayURL: cfg.GatewayURL, GatewayKey: cfg.GatewayKey,
	}, t.Model, t.User, p.Text, cfg.ProbeEffort)
	cancel()
	if probeWasTemporary {
		// Turn it off immediately after the response. endProbe serializes
		// with manual recovery and keeps the control record for retries.
		if err := c.endProbe(context.Background(), t, true); err != nil {
			c.setError(err)
		}
	}

	verdict, detail := VerdictUnavailable, ""
	if callErr != nil {
		detail = "检测不可用：" + errReason(callErr)
	} else {
		passed, manualGrade, gradeErr := p.Grade(answer)
		switch {
		case gradeErr != nil:
			detail = "判分错误：" + gradeErr.Error()
		case manualGrade:
			detail = "检测题未配置自动判分"
		case passed:
			verdict, detail = VerdictPassed, "账号检测通过"
		default:
			verdict, detail = VerdictFailed, "账号检测未通过"
		}
	}
	if len(answer) > 8192 {
		answer = answer[:8192]
	}
	result := &CodexAccountResult{
		ID: randomHex(8), AccountID: t.AccountID, User: t.User,
		Model: t.Model, PromptID: p.ID, Answer: answer,
		Verdict: verdict, Detail: detail, Tokens: tokens, CreatedAt: now,
	}
	if err := c.store.AppendCodexResult(result); err != nil {
		c.setError(err)
	}

	current, err := c.store.CodexAccountTarget(accountID)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if current.Missing || !current.Enabled || current.Model != t.Model {
		if current.State == CodexStateChecking {
			if current.Isolated {
				current.State = CodexStateIsolated
			} else {
				current.State = CodexStateUnknown
			}
			current.Reason = "检测完成后目标已变化，结果已丢弃"
			current.NextCheckAt = time.Now().UTC()
			_, _ = c.store.SaveCodexAccountProbeResult(current, t.Model)
		}
		return nil
	}
	if current.State != CodexStateChecking {
		return nil
	}
	now = time.Now().UTC()
	wasIsolated := current.Isolated
	policy := ResolveTimingPolicy(nil, cfg.TimingPolicy)
	current.CheckedAt = now
	current.LastPromptID = p.ID
	current.LastAnswer = answer
	current.LastTokens = tokens
	if verdict == VerdictFailed || verdict == VerdictUnavailable {
		current.Reason = detail
	} else {
		current.Reason = detail
	}
	switch verdict {
	case VerdictPassed:
		current.FailStreak = 0
		current.Isolated, current.IsolationUntil = false, time.Time{}
		current.State = CodexStatePassed
		current.NextCheckAt = now.Add(time.Duration(policy.NormalMin) * time.Minute)
		current.Reason = "账号检测通过"
	case VerdictFailed:
		current.FailStreak++
		if current.Isolated {
			current.State = CodexStateIsolated
			current.NextCheckAt = now.Add(time.Duration(policy.IsolationLaterMin) * time.Minute)
			current.IsolationUntil = current.NextCheckAt
			current.Reason = "隔离复测未通过，继续隔离"
		} else if current.FailStreak >= codexFailThreshold {
			current.Isolated, current.State = true, CodexStateIsolated
			current.NextCheckAt = now.Add(time.Duration(policy.IsolationFirstMin) * time.Minute)
			current.IsolationUntil = current.NextCheckAt
			current.Reason = "连续检测未通过，已隔离账号"
		} else {
			current.State = CodexStateSuspect
			current.NextCheckAt = now.Add(time.Duration(policy.ErrorRetryMin) * time.Minute)
			current.Reason = fmt.Sprintf("首次检测未通过，%d 分钟后复测", policy.ErrorRetryMin)
		}
	default:
		if current.Isolated {
			current.State = CodexStateIsolated
			current.NextCheckAt = now.Add(time.Duration(policy.UnavailableMin) * time.Minute)
			current.IsolationUntil = current.NextCheckAt
			current.Reason = "隔离复测不可用，继续隔离：" + detail
		} else {
			current.State = CodexStateUnavailable
			current.NextCheckAt = now.Add(time.Duration(policy.UnavailableMin) * time.Minute)
			current.Reason = detail
		}
	}
	committed, err := c.store.SaveCodexAccountProbeResult(current, t.Model)
	if err != nil {
		return err
	}
	if !committed {
		// A manual recovery, model change, disable, or roster update won the
		// race. Its state stands; clear only a checking state with no result.
		state := CodexStateUnknown
		if current.Isolated {
			state = CodexStateIsolated
		}
		_ = c.store.ResetCodexAccountChecking(current.AccountID, state,
			"检测结果因目标变化被丢弃，等待重试", time.Now().UTC())
		return nil
	}
	if current.Isolated != wasIsolated {
		if err := c.syncAccountIsolation(parent); err != nil {
			c.setError(err)
			states, readErr := readCodexLoginStates(c.configDir)
			st, ok := states[strings.ToLower(current.AccountID)]
			switch {
			case current.Isolated && (readErr != nil || !ok || st.On || !st.Paused):
				current.Reason = "停用账号失败，将自动重试：" + err.Error()
				_ = c.store.UpdateCodexAccountReason(current, current.Reason)
			case !current.Isolated && (readErr != nil || !ok || !st.On || st.Paused):
				// The model passed, but re-enabling its account failed. Leave
				// the target passed so the next reconciliation retries.
				current.Reason = "恢复账号失败，将自动重试：" + err.Error()
				_ = c.store.UpdateCodexAccountReason(current, current.Reason)
			}
			return err
		}
	}
	return nil
}

func (c *codexAccountController) finishUnavailable(t *CodexAccountTarget, detail string) error {
	if t == nil {
		return errors.New(detail)
	}
	wasState, wasChecked := t.State, t.CheckedAt
	wasChecking := wasState == CodexStateChecking
	cfg, err := c.store.LoadConfig()
	if err != nil {
		return err
	}
	policy := ResolveTimingPolicy(nil, cfg.TimingPolicy)
	now := time.Now().UTC()
	t.State = CodexStateUnavailable
	t.Reason = detail
	t.CheckedAt = now
	t.NextCheckAt = now.Add(time.Duration(policy.UnavailableMin) * time.Minute)
	if t.Isolated {
		t.State = CodexStateIsolated
		t.IsolationUntil = t.NextCheckAt
	}
	if wasChecking {
		// If this is still the running probe's target, commit conditionally.
		committed, err := c.store.SaveCodexAccountProbeResult(t, t.Model)
		if err != nil {
			return err
		}
		if !committed {
			state := CodexStateUnknown
			if t.Isolated {
				state = CodexStateIsolated
			}
			_ = c.store.ResetCodexAccountChecking(t.AccountID, state,
				"检测结果因目标变化被丢弃，等待重试", time.Now().UTC())
		}
		return nil
	}
	_, err = c.store.UpdateCodexAccountPreflightFailure(t, wasState, wasChecked)
	return err
}

func (c *codexAccountController) resolveAccount(ref string) (CodexAccountInfo, error) {
	ref = strings.TrimSpace(ref)
	if err := validateCodexAccountID(ref); err != nil {
		return CodexAccountInfo{}, err
	}
	accounts, err := c.accounts(true)
	if err != nil {
		return CodexAccountInfo{}, err
	}
	for _, a := range accounts {
		if strings.EqualFold(a.ID, ref) || strings.EqualFold(a.User, ref) {
			if !codexPinUnique(accounts, a.User) {
				return CodexAccountInfo{}, fmt.Errorf("存在多个同名 Codex 账号 %q，无法唯一固定检测账号", a.User)
			}
			return a, nil
		}
	}
	return CodexAccountInfo{}, fmt.Errorf("Magpie 中找不到 Codex 账号 %q", ref)
}

func codexPinUnique(accounts []CodexAccountInfo, user string) bool {
	n := 0
	for _, a := range accounts {
		if strings.EqualFold(a.User, user) {
			n++
		}
	}
	return n == 1
}

func (c *codexAccountController) targetByRef(ref string) (*CodexAccountTarget, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("账号不能为空")
	}
	if t, err := c.store.CodexAccountTarget(strings.ToLower(ref)); err != nil || t != nil {
		return t, err
	}
	targets, err := c.store.CodexAccountTargets()
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if t != nil && (strings.EqualFold(t.AccountID, ref) || strings.EqualFold(t.User, ref)) {
			return t, nil
		}
	}
	return nil, nil
}

func (c *codexAccountController) upsertTarget(accountRef, model string, enabled bool) error {
	a, err := c.resolveAccount(accountRef)
	if err != nil {
		return err
	}
	model = strings.TrimSpace(model)
	if enabled {
		if err := validateCodexModel(model); err != nil {
			return err
		}
	} else if model != "" {
		if err := validateCodexModel(model); err != nil {
			return err
		}
	}
	t, err := c.store.CodexAccountTarget(a.ID)
	if err != nil {
		return err
	}
	if t == nil {
		t = &CodexAccountTarget{AccountID: a.ID, State: CodexStateUnknown, NextCheckAt: time.Now().UTC()}
	}
	if !enabled && t.Isolated {
		return errors.New("账号仍被隔离，不能通过停用监测跳过恢复；请等待复测通过或手动解除隔离")
	}
	if t.Model != "" && t.Model != model {
		// The in-flight probe checks the model it started with and discards its
		// result; reset the presentation so a changed target cannot stay
		// "checking" forever.
		t.State = CodexStateUnknown
		t.NextCheckAt = time.Now().UTC()
		t.LastPromptID = ""
	}
	if !enabled && t.State == CodexStateChecking {
		t.State = CodexStateUnknown
	}
	t.User, t.Model = a.User, model
	t.Enabled = enabled
	t.Missing = false
	if !enabled {
		t.NextCheckAt = time.Time{}
		t.Reason = "监测已停用"
	} else if t.NextCheckAt.IsZero() && !t.Isolated {
		t.NextCheckAt = time.Now().UTC()
	}
	if err := c.store.SaveCodexAccountTarget(t); err != nil {
		return err
	}
	if enabled {
		if err := c.syncAccountIsolation(context.Background()); err != nil {
			return err
		}
	}
	return nil
}

func (c *codexAccountController) deleteTarget(accountRef string) error {
	t, err := c.targetByRef(accountRef)
	if err != nil {
		return err
	}
	if t == nil {
		return errors.New("未找到监测目标")
	}
	if t.Isolated {
		return errors.New("账号仍被隔离，不能删除监测目标")
	}
	return c.store.DeleteCodexAccountTarget(t.AccountID)
}

func (c *codexAccountController) manualRecover(accountRef string) error {
	t, err := c.targetByRef(accountRef)
	if err != nil {
		return err
	}
	if t == nil {
		return errors.New("未找到监测目标")
	}
	was := t.Isolated
	t.Isolated, t.IsolationUntil = false, time.Time{}
	t.FailStreak = 0
	t.State = CodexStatePassed
	t.Reason = "已手动解除隔离"
	t.CheckedAt = time.Now().UTC()
	cfg, err := c.store.LoadConfig()
	if err != nil {
		return err
	}
	policy := ResolveTimingPolicy(nil, cfg.TimingPolicy)
	t.NextCheckAt = t.CheckedAt.Add(time.Duration(policy.NormalMin) * time.Minute)
	if err := c.store.SaveCodexAccountTarget(t); err != nil {
		return err
	}
	if was {
		if err := c.syncAccountIsolation(context.Background()); err != nil {
			t.Reason = "解除隔离写入失败，将自动重试：" + err.Error()
			_ = c.store.UpdateCodexAccountReason(t, t.Reason)
			return err
		}
	}
	return nil
}

func (c *codexAccountController) trigger(accountRef string) error {
	a, err := c.resolveAccount(accountRef)
	if err != nil {
		return err
	}
	t, err := c.store.CodexAccountTarget(a.ID)
	if err != nil {
		return err
	}
	if t == nil || !t.Enabled || t.Missing {
		return errors.New("账号监测未启用")
	}
	if err := validateCodexModel(t.Model); err != nil {
		return err
	}
	if err := c.syncAccountIsolation(context.Background()); err != nil {
		c.setError(err)
	}
	s := c.svc
	s.mu.Lock()
	if s.closing || s.codexBusy {
		s.mu.Unlock()
		return errors.New("已有账号检测正在执行")
	}
	s.codexBusy = true
	s.mu.Unlock()
	s.launchCodexProbe(a.ID, true)
	return nil
}

func (c *codexAccountController) view() map[string]any {
	out := map[string]any{
		"configured": strings.TrimSpace(c.configDir) != "",
	}
	accounts, err := c.accounts(true)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	_ = c.syncRoster()
	targets, err := c.store.CodexAccountTargets()
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	byID := map[string]*CodexAccountTarget{}
	for _, t := range targets {
		byID[t.AccountID] = t
	}
	rows := make([]map[string]any, 0, len(accounts))
	liveIDs := map[string]bool{}
	for _, a := range accounts {
		liveIDs[a.ID] = true
		row := map[string]any{
			"id": a.ID, "user": a.User, "plan": a.Plan,
			"lapsed": a.Lapsed, "on": a.On, "paused": a.Paused, "own": a.Own,
			"target": byID[a.ID],
		}
		rows = append(rows, row)
	}
	for _, t := range targets {
		if t == nil || liveIDs[t.AccountID] {
			continue
		}
		rows = append(rows, map[string]any{
			"id": t.AccountID, "user": t.User, "missing": true, "target": t,
		})
	}
	out["accounts"] = rows
	out["isolated"] = sortedIsolated(targets)
	out["last_error"] = c.errorText()
	return out
}

func sortedIsolated(targets []*CodexAccountTarget) []string {
	var out []string
	for _, t := range targets {
		if t != nil && t.Isolated {
			out = append(out, t.AccountID)
		}
	}
	sort.Strings(out)
	return out
}
