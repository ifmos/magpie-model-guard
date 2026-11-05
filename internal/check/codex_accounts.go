package check

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// Account isolation uses Magpie's own provider account switch (the same
// on/paused fields its GUI and pool command write), never a Gateway Key
// allowlist. A disabled account is turned back on only for the few seconds a
// retest needs, then disabled again unless it passed.

const codexAccountControlsKey = "codex_account_controls"

var (
	errCodexLoginChanged      = errors.New("Codex account state changed")
	errCodexFallbackPreserved = errors.New("fallback account edit preserved")
)

type codexAccountController struct {
	mu         sync.Mutex
	svc        *Service
	store      *Store
	configDir  string
	lastError  string
	rosterAt   time.Time
	roster     []CodexAccountInfo
	probingIDs map[string]bool
	syncMu     sync.Mutex
}

func newCodexAccountController(svc *Service, store *Store, configDir string) *codexAccountController {
	return &codexAccountController{svc: svc, store: store, configDir: configDir, probingIDs: map[string]bool{}}
}

func (c *codexAccountController) errorText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.lastError)
}

func (c *codexAccountController) accounts(refresh bool) ([]CodexAccountInfo, error) {
	c.mu.Lock()
	if !refresh && !c.rosterAt.IsZero() && time.Since(c.rosterAt) < 15*time.Second && c.roster != nil {
		out := append([]CodexAccountInfo(nil), c.roster...)
		c.mu.Unlock()
		return out, nil
	}
	configDir := c.configDir
	c.mu.Unlock()
	if strings.TrimSpace(configDir) == "" {
		return nil, errors.New("未配置 Magpie 配置目录")
	}
	accounts, err := discoverCodexAccounts(configDir)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.roster = accounts
	c.rosterAt = time.Now()
	c.mu.Unlock()
	return accounts, nil
}

func (c *codexAccountController) setError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.lastError = ""
	} else {
		c.lastError = err.Error()
	}
}

type codexAccountControl struct {
	User            string `json:"user,omitempty"`
	FallbackID      string `json:"fallback_id,omitempty"`
	FallbackUser    string `json:"fallback_user,omitempty"`
	FallbackChanged bool   `json:"fallback_changed,omitempty"`
	FallbackOn      bool   `json:"fallback_on,omitempty"`
	FallbackPaused  bool   `json:"fallback_paused,omitempty"`
	// Applied is false while the account switches still need a retry, and
	// Probing marks the short window an isolated account is enabled for a
	// retest. Both survive a restart.
	Applied bool `json:"applied,omitempty"`
	Probing bool `json:"probing,omitempty"`
}

type codexAccountControls struct {
	Accounts map[string]codexAccountControl `json:"accounts"`
}

type codexLoginState struct {
	ID     string
	User   string
	On     bool
	Paused bool
}

type codexLoginChange struct {
	ID     string
	User   string
	On     bool
	Paused bool
	// Check makes the write conditional on the values last observed under
	// Magpie's own lock. It keeps a concurrent user or Magpie edit from
	// being overwritten.
	Check      bool
	FromOn     bool
	FromPaused bool
}

func (c *codexAccountController) loadControls() (codexAccountControls, error) {
	st := codexAccountControls{Accounts: map[string]codexAccountControl{}}
	ok, err := c.store.getKV(codexAccountControlsKey, &st)
	if err != nil {
		return st, fmt.Errorf("无法读取账号隔离状态：%w", err)
	}
	if !ok || st.Accounts == nil {
		st.Accounts = map[string]codexAccountControl{}
	}
	return st, nil
}

func (c *codexAccountController) saveControls(st *codexAccountControls) error {
	if st.Accounts == nil {
		st.Accounts = map[string]codexAccountControl{}
	}
	if err := c.store.setKV(codexAccountControlsKey, *st); err != nil {
		return fmt.Errorf("保存账号隔离状态失败：%w", err)
	}
	return nil
}

func (c *codexAccountController) probing(accountID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probingIDs[strings.ToLower(accountID)]
}

func (c *codexAccountController) setProbing(accountID string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probingIDs == nil {
		c.probingIDs = map[string]bool{}
	}
	if on {
		c.probingIDs[strings.ToLower(accountID)] = true
	} else {
		delete(c.probingIDs, strings.ToLower(accountID))
	}
}

func readCodexLoginStates(configDir string) (map[string]codexLoginState, error) {
	path := codexLoginsPath(configDir)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]codexLoginState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("无法读取本机 Magpie 账号信息：%w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Magpie 账号文件权限过宽，拒绝写入账号状态；请将其权限设为 0600")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取本机 Magpie 账号信息：%w", err)
	}
	if len(raw) > 8<<20 {
		return nil, errors.New("Magpie 账号信息超出安全上限")
	}
	var rows []struct {
		ID     string `json:"id"`
		Agent  string `json:"agent"`
		User   string `json:"user"`
		Hidden string `json:"hidden"`
		On     bool   `json:"on"`
		Paused bool   `json:"paused"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, errors.New("Magpie 账号文件格式不正确")
	}
	out := map[string]codexLoginState{}
	for _, row := range rows {
		if row.Agent != "codex" || strings.TrimSpace(row.Hidden) != "" {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(row.ID))
		if id == "" {
			id = codexFallbackLoginID(row.User)
		}
		if id == "" {
			continue
		}
		out[id] = codexLoginState{ID: id, User: row.User, On: row.On, Paused: row.Paused}
	}
	return out, nil
}

func codexFallbackLoginID(user string) string {
	user = strings.TrimSpace(user)
	if user == "" {
		return ""
	}
	// Mirrors Magpie's loginID fallback for an entry written before the
	// stable account id was assigned.
	sum := sha256.Sum256([]byte("magpie login\ncodex\n" + strings.ToLower(user)))
	return hex.EncodeToString(sum[:8])
}

func (c *codexAccountController) updateLogins(ctx context.Context, changes []codexLoginChange) error {
	if len(changes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lock := flock.New(codexLoginsPath(c.configDir) + ".lock")
	ok, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return fmt.Errorf("无法锁定 Magpie 账号文件：%w", err)
	}
	if !ok {
		return errors.New("Magpie 账号文件正被占用，请稍后重试")
	}
	defer func() { _ = lock.Unlock() }()

	path := codexLoginsPath(c.configDir)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("Magpie 账号文件不存在")
	}
	if err != nil {
		return fmt.Errorf("无法读取 Magpie 账号文件：%w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("Magpie 账号文件权限过宽，拒绝修改；请将其权限设为 0600")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("无法读取 Magpie 账号文件：%w", err)
	}
	if len(raw) > 8<<20 {
		return errors.New("Magpie 账号文件超出安全上限")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return errors.New("Magpie 账号文件格式不正确")
	}
	index := map[string]int{}
	for i, row := range rows {
		agent := rawJSONString(row["agent"])
		id := strings.ToLower(strings.TrimSpace(rawJSONString(row["id"])))
		if id == "" {
			id = codexFallbackLoginID(rawJSONString(row["user"]))
		}
		if agent == "codex" && id != "" {
			index[id] = i
		}
	}
	// The expected-state check and the write are one critical section.
	for _, change := range changes {
		id := strings.ToLower(strings.TrimSpace(change.ID))
		if id == "" {
			return errors.New("账号缺少稳定 ID")
		}
		i, ok := index[id]
		if !ok {
			return fmt.Errorf("Magpie 中找不到 Codex 账号 %q", change.User)
		}
		on, paused := rawJSONBool(rows[i]["on"]), rawJSONBool(rows[i]["paused"])
		if on == change.On && paused == change.Paused {
			continue
		}
		if change.Check && (on != change.FromOn || paused != change.FromPaused) {
			return fmt.Errorf("%w: Codex 账号 %s 在写入前被修改，未覆盖该修改", errCodexLoginChanged, change.User)
		}
		rows[i]["on"] = json.RawMessage(strconvBool(change.On))
		rows[i]["paused"] = json.RawMessage(strconvBool(change.Paused))
	}
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入 Magpie 账号状态失败：%w", err)
	}
	return nil
}

func strconvBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func rawJSONString(v json.RawMessage) string {
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

func rawJSONBool(v json.RawMessage) bool {
	var b bool
	_ = json.Unmarshal(v, &b)
	return b
}

func fallbackUsable(ctl codexAccountControl, accounts []CodexAccountInfo, targetsByID map[string]*CodexAccountTarget) bool {
	if ctl.FallbackID == "" {
		return false
	}
	if t := targetsByID[strings.ToLower(ctl.FallbackID)]; t != nil && t.Isolated {
		return false
	}
	for _, a := range accounts {
		if strings.EqualFold(a.ID, ctl.FallbackID) {
			return a.Lapsed == ""
		}
	}
	return false
}

func chooseCodexFallback(target *CodexAccountTarget, accounts []CodexAccountInfo, targetsByID map[string]*CodexAccountTarget) *CodexAccountInfo {
	if target == nil {
		return nil
	}
	isolated := map[string]bool{}
	for id, t := range targetsByID {
		if t != nil && t.Isolated {
			isolated[strings.ToLower(id)] = true
		}
	}
	var best *CodexAccountInfo
	score := -1
	for i := range accounts {
		a := &accounts[i]
		if strings.EqualFold(a.ID, target.AccountID) || isolated[strings.ToLower(a.ID)] || a.Lapsed != "" {
			continue
		}
		s := 0
		if a.On {
			s += 4
		}
		if t := targetsByID[strings.ToLower(a.ID)]; t == nil || t.State == CodexStatePassed || t.State == CodexStateUnknown {
			s += 2
		}
		if a.Lapsed == "" {
			s++
		}
		if s > score {
			score, best = s, a
		}
	}
	return best
}

// syncAccountIsolation reconciles provider account switches with targets that
// are currently isolated. It never writes Gateway Keys. One account's failure
// is kept in the controls ledger and does not stop the others from being
// reconciled, nor does it stop probes of accounts that are still available.
func (c *codexAccountController) syncAccountIsolation(ctx context.Context) error {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	c.setError(nil)
	accounts, err := c.accounts(true)
	if err != nil {
		c.setError(err)
		return err
	}
	targets, err := c.store.CodexAccountTargets()
	if err != nil {
		c.setError(err)
		return err
	}
	byID := map[string]*CodexAccountTarget{}
	isolated := map[string]*CodexAccountTarget{}
	for _, t := range targets {
		if t == nil {
			continue
		}
		byID[strings.ToLower(t.AccountID)] = t
		if t.Isolated {
			isolated[strings.ToLower(t.AccountID)] = t
		}
	}
	controls, err := c.loadControls()
	if err != nil {
		c.setError(err)
		return err
	}
	if len(controls.Accounts) == 0 && len(isolated) == 0 {
		return nil
	}
	live, err := readCodexLoginStates(c.configDir)
	if err != nil {
		c.setError(err)
		return err
	}
	var firstErr error
	changed := false

	// A recovered, deleted, or no-longer-isolated target gets its account and
	// fallback back. The control record stays until the write succeeds, so a
	// crash keeps the fallback's original values.
	for id, ctl := range controls.Accounts {
		t := byID[strings.ToLower(id)]
		if t != nil && t.Isolated && !t.Missing {
			continue
		}
		if err := c.restoreAccountControl(ctx, id, ctl, isolated, live, &controls); err != nil {
			if errors.Is(err, errCodexFallbackPreserved) {
				delete(controls.Accounts, id)
				changed = true
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		delete(controls.Accounts, id)
		changed = true
	}
	if changed {
		if live, err = readCodexLoginStates(c.configDir); err != nil {
			c.setError(err)
			return err
		}
	}

	for id, t := range isolated {
		if t == nil || t.Missing || c.probing(id) {
			continue
		}
		ctl, ok := controls.Accounts[id]
		if ok {
			if _, liveOK := live[strings.ToLower(id)]; !liveOK {
				if err := c.restoreAccountControl(ctx, id, ctl, isolated, live, &controls); err != nil {
					if errors.Is(err, errCodexFallbackPreserved) {
						delete(controls.Accounts, id)
						changed = true
					}
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				delete(controls.Accounts, id)
				changed = true
				if live, err = readCodexLoginStates(c.configDir); err != nil {
					c.setError(err)
					return err
				}
				continue
			}
			if ctl.Probing {
				// A probe was interrupted by a restart or a failed cleanup.
				// Finish its disable before treating the account as edited.
				if err := c.setAccountForProbe(ctx, t, false); err != nil {
					ctl.Probing = false
					ctl.Applied = errors.Is(err, errCodexLoginChanged)
					controls.Accounts[id] = ctl
					changed = true
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				ctl.Probing, ctl.Applied = false, true
				controls.Accounts[id] = ctl
				changed = true
				if live, err = readCodexLoginStates(c.configDir); err != nil {
					c.setError(err)
					return err
				}
				continue
			}
			if !ctl.Applied {
				if !fallbackUsable(ctl, accounts, byID) {
					fallback := chooseCodexFallback(t, accounts, byID)
					if fallback == nil {
						err := errors.New("没有其他可用 Codex 账号承接流量，无法停用该账号")
						if firstErr == nil {
							firstErr = err
						}
						continue
					}
					old := ctl
					ctl = codexAccountControl{
						User: t.User, FallbackID: fallback.ID, FallbackUser: fallback.User,
						FallbackOn: fallback.On,
					}
					if fs, ok := live[strings.ToLower(fallback.ID)]; ok {
						ctl.FallbackOn, ctl.FallbackPaused = fs.On, fs.Paused
					}
					ctl.FallbackChanged = !ctl.FallbackOn || ctl.FallbackPaused
					oldFallbackNeeded := false
					if old.FallbackChanged && old.FallbackID != "" && old.FallbackID != fallback.ID {
						if depID := otherFallbackDependency(&controls, isolated, old.FallbackID, id); depID != "" {
							dep := controls.Accounts[depID]
							if !dep.FallbackChanged {
								dep.FallbackChanged = true
								dep.FallbackOn, dep.FallbackPaused = old.FallbackOn, old.FallbackPaused
								controls.Accounts[depID] = dep
							}
							oldFallbackNeeded = true
						}
					}
					controls.Accounts[id] = ctl
					if err := c.saveControls(&controls); err != nil {
						c.setError(err)
						return err
					}
					if !oldFallbackNeeded && old.FallbackChanged && old.FallbackID != "" && old.FallbackID != fallback.ID {
						if _, ok := live[strings.ToLower(old.FallbackID)]; ok {
							if ot := isolated[strings.ToLower(old.FallbackID)]; ot == nil || !ot.Isolated {
								_ = c.updateLogins(ctx, []codexLoginChange{{
									ID: old.FallbackID, User: old.FallbackUser,
									On: old.FallbackOn, Paused: old.FallbackPaused,
									Check: true, FromOn: true, FromPaused: false,
								}})
							}
						}
					}
				}
				if err := c.applyAccountControl(ctx, t, ctl, live); err != nil {
					ctl.Applied = errors.Is(err, errCodexLoginChanged)
					controls.Accounts[id] = ctl
					changed = true
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				if live, err = readCodexLoginStates(c.configDir); err != nil {
					c.setError(err)
					return err
				}
				if err := verifyAccountControl(id, ctl, live, isolated); err != nil {
					ctl.Applied = true
					controls.Accounts[id] = ctl
					changed = true
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				ctl.Applied = true
				controls.Accounts[id] = ctl
				changed = true
				continue
			}
			if err := verifyAccountControl(id, ctl, live, isolated); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			continue
		}
		fallback := chooseCodexFallback(t, accounts, byID)
		if fallback == nil {
			err := errors.New("没有其他可用 Codex 账号承接流量，无法停用最后一个账号")
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ctl = codexAccountControl{
			User: t.User, FallbackID: fallback.ID, FallbackUser: fallback.User,
			FallbackOn: fallback.On,
		}
		if fs, ok := live[strings.ToLower(fallback.ID)]; ok {
			ctl.FallbackOn, ctl.FallbackPaused = fs.On, fs.Paused
		}
		ctl.FallbackChanged = !ctl.FallbackOn || ctl.FallbackPaused
		// Journal the fallback's original values before changing either
		// account. A crash can then retry the switch without losing them.
		controls.Accounts[id] = ctl
		if err := c.saveControls(&controls); err != nil {
			c.setError(err)
			return err
		}
		changed = false
		if err := c.applyAccountControl(ctx, t, ctl, live); err != nil {
			ctl.Applied = errors.Is(err, errCodexLoginChanged)
			controls.Accounts[id] = ctl
			changed = true
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if live, err = readCodexLoginStates(c.configDir); err != nil {
			c.setError(err)
			return err
		}
		if err := verifyAccountControl(id, ctl, live, isolated); err != nil {
			ctl.Applied = true
			controls.Accounts[id] = ctl
			changed = true
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ctl.Applied = true
		controls.Accounts[id] = ctl
		changed = true
	}
	if changed {
		if err := c.saveControls(&controls); err != nil {
			c.setError(err)
			return err
		}
	}
	if firstErr != nil {
		c.setError(firstErr)
		return firstErr
	}
	return nil
}

func verifyAccountControl(id string, ctl codexAccountControl, live map[string]codexLoginState, isolated map[string]*CodexAccountTarget) error {
	st, ok := live[strings.ToLower(id)]
	if !ok {
		return nil
	}
	if st.On || !st.Paused {
		return fmt.Errorf("Codex 账号 %s 的状态被外部修改；已停止自动覆盖，请手动解除或重新隔离", ctl.User)
	}
	if ctl.FallbackID != "" {
		fb, ok := live[strings.ToLower(ctl.FallbackID)]
		if !ok || !fb.On || fb.Paused {
			return fmt.Errorf("备用 Codex 账号 %s 未处于启用状态，隔离可能未生效；请恢复该账号或手动解除隔离", ctl.FallbackUser)
		}
		if t := isolated[strings.ToLower(ctl.FallbackID)]; t != nil && t.Isolated {
			return fmt.Errorf("备用 Codex 账号 %s 也被隔离，当前账号的暂停可能失效", ctl.FallbackUser)
		}
	}
	return nil
}

func (c *codexAccountController) applyAccountControl(ctx context.Context, t *CodexAccountTarget, ctl codexAccountControl, live map[string]codexLoginState) error {
	changes := []codexLoginChange{{
		ID: t.AccountID, User: t.User, On: false, Paused: true,
		Check: true,
	}}
	if st, ok := live[strings.ToLower(t.AccountID)]; ok {
		changes[0].FromOn, changes[0].FromPaused = st.On, st.Paused
	}
	if ctl.FallbackID != "" {
		change := codexLoginChange{
			ID: ctl.FallbackID, User: ctl.FallbackUser, On: true, Paused: false,
			Check: true,
		}
		if st, ok := live[strings.ToLower(ctl.FallbackID)]; ok {
			change.FromOn, change.FromPaused = st.On, st.Paused
		}
		changes = append(changes, change)
	}
	return c.updateLogins(ctx, changes)
}

func (c *codexAccountController) restoreAccountControl(ctx context.Context, id string, ctl codexAccountControl, isolated map[string]*CodexAccountTarget, live map[string]codexLoginState, controls *codexAccountControls) error {
	if _, ok := live[strings.ToLower(id)]; ok {
		if err := c.updateLogins(ctx, []codexLoginChange{{
			ID: id, User: ctl.User, On: true, Paused: false,
			Check: true, FromOn: false, FromPaused: true,
		}}); err != nil {
			return err
		}
	}
	if !ctl.FallbackChanged || ctl.FallbackID == "" {
		return nil
	}
	if depID := otherFallbackDependency(controls, isolated, ctl.FallbackID, id); depID != "" {
		// Another isolated account still needs this fallback. Move the
		// original baseline to that control before retiring this one.
		dep := controls.Accounts[depID]
		if !dep.FallbackChanged {
			dep.FallbackChanged = true
			dep.FallbackOn, dep.FallbackPaused = ctl.FallbackOn, ctl.FallbackPaused
			controls.Accounts[depID] = dep
		}
		return nil
	}
	if t := isolated[strings.ToLower(ctl.FallbackID)]; t != nil && t.Isolated {
		return nil
	}
	if _, ok := live[strings.ToLower(ctl.FallbackID)]; !ok {
		// The fallback was removed or hidden; there is nothing left to
		// restore, and keeping the control would block future isolations.
		return nil
	}
	err := c.updateLogins(ctx, []codexLoginChange{{
		ID: ctl.FallbackID, User: ctl.FallbackUser,
		On: ctl.FallbackOn, Paused: ctl.FallbackPaused,
		Check: true, FromOn: true, FromPaused: false,
	}})
	if errors.Is(err, errCodexLoginChanged) {
		return fmt.Errorf("%w: %v", errCodexFallbackPreserved, err)
	}
	return err
}

func otherFallbackDependency(controls *codexAccountControls, isolated map[string]*CodexAccountTarget, fallbackID, except string) string {
	for id, ctl := range controls.Accounts {
		if id == except || !strings.EqualFold(ctl.FallbackID, fallbackID) {
			continue
		}
		if t := isolated[id]; t != nil && t.Isolated && !t.Missing {
			return id
		}
	}
	return ""
}

// setAccountForProbe changes one isolated account for the retest window. The
// check fails if a user or Magpie changed it after the caller's last look.
func (c *codexAccountController) setAccountForProbe(ctx context.Context, t *CodexAccountTarget, on bool) error {
	if t == nil {
		return errors.New("账号不存在")
	}
	change := codexLoginChange{
		ID: t.AccountID, User: t.User, On: on, Paused: !on,
		Check: true, FromOn: !on, FromPaused: on,
	}
	return c.updateLogins(ctx, []codexLoginChange{change})
}

// beginProbe records and performs a temporary enable. It returns false when
// the account is already available (for example when isolation could not be
// applied because no fallback exists); such a probe needs no cleanup.
func (c *codexAccountController) beginProbe(ctx context.Context, t *CodexAccountTarget) (bool, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	states, err := readCodexLoginStates(c.configDir)
	if err != nil {
		return false, err
	}
	st, ok := states[strings.ToLower(t.AccountID)]
	if !ok {
		return false, errors.New("Magpie 中找不到该 Codex 账号")
	}
	if st.On && !st.Paused {
		return false, nil
	}
	controls, err := c.loadControls()
	if err != nil {
		return false, err
	}
	ctl, ok := controls.Accounts[strings.ToLower(t.AccountID)]
	if !ok || !ctl.Applied {
		return false, errors.New("账号隔离尚未生效，无法安全复测")
	}
	if fb, ok := states[strings.ToLower(ctl.FallbackID)]; !ok || !fb.On || fb.Paused {
		return false, fmt.Errorf("备用账号 %s 未启用，无法安全复测", ctl.FallbackUser)
	}
	ctl.Probing = true
	controls.Accounts[strings.ToLower(t.AccountID)] = ctl
	if err := c.saveControls(&controls); err != nil {
		return false, err
	}
	c.setProbing(t.AccountID, true)
	if err := c.setAccountForProbe(ctx, t, true); err != nil {
		ctl.Probing = false
		controls.Accounts[strings.ToLower(t.AccountID)] = ctl
		_ = c.saveControls(&controls)
		c.setProbing(t.AccountID, false)
		return false, err
	}
	return true, nil
}

// endProbe disables an isolated account again after its retest. It holds the
// same lock as recovery, so a manual recovery that completed first is not
// overwritten using an earlier "still isolated" decision. A failed disable is
// kept in the ledger as Applied=false for the next reconciliation.
func (c *codexAccountController) endProbe(ctx context.Context, t *CodexAccountTarget, enabled bool) error {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	defer c.setProbing(t.AccountID, false)
	controls, err := c.loadControls()
	if err != nil {
		return err
	}
	id := strings.ToLower(t.AccountID)
	if !enabled {
		if ctl, ok := controls.Accounts[id]; ok && ctl.Probing {
			ctl.Probing = false
			controls.Accounts[id] = ctl
			return c.saveControls(&controls)
		}
		return nil
	}
	cur, err := c.store.CodexAccountTarget(t.AccountID)
	if err != nil {
		return err
	}
	if cur == nil || !cur.Isolated {
		// Recovery or deletion won the race. Leave the account enabled and
		// let recovery's own reconciliation restore the fallback.
		if ctl, ok := controls.Accounts[id]; ok && ctl.Probing {
			ctl.Probing = false
			controls.Accounts[id] = ctl
			return c.saveControls(&controls)
		}
		return nil
	}
	disableErr := c.setAccountForProbe(ctx, t, false)
	ctl, ok := controls.Accounts[id]
	if ok {
		ctl.Probing = false
		ctl.Applied = disableErr == nil || errors.Is(disableErr, errCodexLoginChanged)
		controls.Accounts[id] = ctl
		if err := c.saveControls(&controls); err != nil && disableErr == nil {
			disableErr = err
		}
	}
	return disableErr
}
