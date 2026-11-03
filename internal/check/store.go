package check

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// maxResults is the retention cap for probe results (newest kept).
const maxResults = 500

// maxSupplierEvents is the retention cap for the protection log.
const maxSupplierEvents = 1000

// Store persists guard state in SQLite.
type Store struct {
	db        *sql.DB
	rerouteMu sync.Mutex
}

// OpenStore opens (creating if needed) the state database.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: single writer
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS kv (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS targets (
  model           TEXT PRIMARY KEY,
  enabled         INTEGER NOT NULL DEFAULT 1,
  prompt_ids       TEXT NOT NULL DEFAULT '[]',
  interval_min    INTEGER NOT NULL DEFAULT 30,
  fail_recheck_min INTEGER NOT NULL DEFAULT 0,
  next_check_at    INTEGER NOT NULL DEFAULT 0,
  state            TEXT NOT NULL DEFAULT 'unknown',
  reason           TEXT NOT NULL DEFAULT '',
  checked_at       INTEGER NOT NULL DEFAULT 0,
  allow_until      INTEGER NOT NULL DEFAULT 0,
  fail_streak      INTEGER NOT NULL DEFAULT 0,
  schedule_json    TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS prompts (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  text      TEXT NOT NULL,
  builtin   INTEGER NOT NULL DEFAULT 0,
  grader    TEXT NOT NULL DEFAULT 'none',
  expect    TEXT NOT NULL DEFAULT '',
  automatic INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS results (
  id         TEXT PRIMARY KEY,
  model      TEXT NOT NULL,
  prompt_id  TEXT NOT NULL,
  answer     TEXT NOT NULL DEFAULT '',
  verdict    TEXT NOT NULL,
  detail       TEXT NOT NULL DEFAULT '',
  actual_model TEXT NOT NULL DEFAULT '',
  rule_id     TEXT NOT NULL DEFAULT '',
  tokens       INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  source       TEXT NOT NULL DEFAULT 'manual'
);
CREATE INDEX IF NOT EXISTS idx_results_model ON results(model, created_at DESC);
CREATE TABLE IF NOT EXISTS supplier_events (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  kind          TEXT NOT NULL,
  provider_id   TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  actual_model  TEXT NOT NULL DEFAULT '',
  action        TEXT NOT NULL DEFAULT '',
  rule_id       TEXT NOT NULL DEFAULT '',
  detail        TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_supplier_events_created ON supplier_events(created_at DESC);
`)
	if err != nil {
		return err
	}
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('targets') WHERE name='manual_hold'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := s.db.Exec(`ALTER TABLE targets ADD COLUMN manual_hold INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		_, err = s.db.Exec(`UPDATE targets SET manual_hold=1 WHERE state='review'`)
		if err != nil {
			return err
		}
	}
	var sjExists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('targets') WHERE name='schedule_json'`).Scan(&sjExists); err != nil {
		return err
	}
	if sjExists == 0 {
		if _, err := s.db.Exec(`ALTER TABLE targets ADD COLUMN schedule_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
			return err
		}
	}
	var rrExists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('targets') WHERE name='response_rules_json'`).Scan(&rrExists); err != nil {
		return err
	}
	if rrExists == 0 {
		if _, err := s.db.Exec(`ALTER TABLE targets ADD COLUMN response_rules_json TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	var amExists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('results') WHERE name='actual_model'`).Scan(&amExists); err != nil {
		return err
	}
	if amExists == 0 {
		if _, err := s.db.Exec(`ALTER TABLE results ADD COLUMN actual_model TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	var ridExists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('results') WHERE name='rule_id'`).Scan(&ridExists); err != nil {
		return err
	}
	if ridExists == 0 {
		if _, err := s.db.Exec(`ALTER TABLE results ADD COLUMN rule_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	var rhExists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('targets') WHERE name='rule_hold_until'`).Scan(&rhExists); err != nil {
		return err
	}
	if rhExists == 0 {
		if _, err := s.db.Exec(`ALTER TABLE targets ADD COLUMN rule_hold_until INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	var serExists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('supplier_events') WHERE name='rule_id'`).Scan(&serExists); err != nil {
		return err
	}
	if serExists == 0 {
		if _, err := s.db.Exec(`ALTER TABLE supplier_events ADD COLUMN rule_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	// Business-reply rule hits used to be stored as probe results. Move them
	// into the protection log so probe history and supplier protection stay
	// independent; the action column is recovered from the old verdict/detail.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO supplier_events(kind,provider_id,model,actual_model,action,rule_id,detail,created_at)
	  SELECT 'action','',model,actual_model,
	    CASE WHEN verdict='failed' THEN 'disable' WHEN detail LIKE '%改道%' THEN 'reroute' ELSE 'record' END,
	    rule_id,detail,created_at
	  FROM results WHERE prompt_id='response-model' OR source='response'`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM results WHERE prompt_id='response-model' OR source='response'`); err != nil {
		return err
	}
	return tx.Commit()
}

// KV helpers.

func (s *Store) getKV(key string, dst any) (bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), dst)
}

func (s *Store) setKV(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO kv(key,value) VALUES(?,?)
	  ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(b))
	return err
}

// GuardConfig is the persisted service configuration.
type GuardConfig struct {
	GatewayURL         string       `json:"gateway_url"`
	GatewayKey         string       `json:"gateway_key"`
	DailyLimit         int          `json:"daily_limit"`          // probe requests per model per day
	TimeoutSec         int          `json:"timeout_sec"`          // per probe request
	CoolMin            int          `json:"cool_min"`             // unavailable cool-down minutes
	DefaultIntervalMin int          `json:"default_interval_min"` // default scheduled interval
	ProbeMaxTokens     int          `json:"probe_max_tokens"`
	ScheduleMode       string       `json:"schedule_mode"` // "fixed" | "staged", default "fixed"
	TimingPolicy       TimingPolicy `json:"timing_policy"`

	// Response-model judgment: match the real model in replies.
	ResponseModelMode  string              `json:"response_model_mode"` // "off" | "on"
	ResponseRules      []ResponseModelRule `json:"response_rules,omitempty"`
	ResponseRerouteMin int                 `json:"response_reroute_min"` // minutes a rerouted model stays excluded
	ResponseHoldMin    int                 `json:"response_hold_min"`    // minutes a rule-blocked target stays blocked

	// Response-model retries for supplier protection. The JSON name is kept
	// for configs saved before the direct-provider mode was removed.
	ProviderAttempts int `json:"provider_attempts"`
}

func defaultConfig() GuardConfig {
	return GuardConfig{
		DailyLimit:         120,
		TimeoutSec:         180,
		CoolMin:            15,
		DefaultIntervalMin: 30,
		ProbeMaxTokens:     8192,
		ScheduleMode:       ScheduleModeFixed,
		TimingPolicy:       DefaultTimingPolicy(),
		ResponseModelMode:  ResponseModelOff,
		ResponseRerouteMin: 30,
		ResponseHoldMin:    30,
		ProviderAttempts:   2,
	}
}

// LoadConfig reads config, applying defaults for zero fields.
func (s *Store) LoadConfig() (GuardConfig, error) {
	c := defaultConfig()
	_, err := s.getKV("config", &c)
	if c.ScheduleMode == "" {
		c.ScheduleMode = ScheduleModeFixed
	}
	if c.ResponseModelMode == "" {
		c.ResponseModelMode = ResponseModelOff
	}
	if c.ResponseRerouteMin <= 0 {
		c.ResponseRerouteMin = 30
	}
	if c.ResponseHoldMin <= 0 {
		c.ResponseHoldMin = 30
	}
	if c.ProviderAttempts <= 0 {
		c.ProviderAttempts = 2
	}
	c.TimingPolicy = ResolveTimingPolicy(nil, c.TimingPolicy)
	return c, err
}

// SaveConfig persists config.
func (s *Store) SaveConfig(c GuardConfig) error { return s.setKV("config", c) }

// Alias returns the probe alias secret; created on first use.
func (s *Store) Alias() (string, error) {
	var a string
	ok, err := s.getKV("alias", &a)
	if err != nil {
		return "", err
	}
	if ok && a != "" {
		return a, nil
	}
	a = randomHex(16)
	if err := s.setKV("alias", a); err != nil {
		return "", err
	}
	return a, nil
}

// Target is a monitored model.
type Target struct {
	Model          string    `json:"model"`
	Enabled        bool      `json:"enabled"`
	PromptIDs      []string  `json:"prompt_ids"`
	IntervalMin    int       `json:"interval_min"`
	FailRecheckMin int       `json:"fail_recheck_min"`
	NextCheckAt    time.Time `json:"next_check_at"`
	State          string    `json:"state"` // unknown|checking|passed|failed|unavailable|review
	Reason         string    `json:"reason"`
	CheckedAt      time.Time `json:"checked_at"`
	AllowUntil     time.Time `json:"allow_until"`
	FailStreak     int       `json:"fail_streak"`
	ManualHold     bool      `json:"manual_hold"`
	Blocked        bool      `json:"blocked"`
	BlockSource    string    `json:"block_source,omitempty"` // manual|rule|probe

	// RuleHoldUntil blocks the target after a business-reply rule hit. It is
	// independent of the probe state machine: a passing probe never lifts it,
	// only its own expiry (or an operator) does.
	RuleHoldUntil time.Time `json:"rule_hold_until,omitempty"`
	// Staged schedule configuration and runtime fields:
	ScheduleMode      string        `json:"schedule_mode"`
	TimingPolicy      *TimingPolicy `json:"timing_policy,omitempty"`
	ScheduleStage     string        `json:"schedule_stage"`
	RecoveryPasses    int           `json:"recovery_passes"`
	IsolationRechecks int           `json:"isolation_rechecks"`
	RetryPromptID     string        `json:"retry_prompt_id"`

	// Response-model judgment override: inherit global or use custom rules.
	ResponseRuleMode   string              `json:"response_rule_mode,omitempty"`
	ResponseModelRules []ResponseModelRule `json:"response_model_rules,omitempty"`
}

// targetResponseRulesPayload is persisted beside the schedule fields.
type targetResponseRulesPayload struct {
	Mode  string              `json:"mode,omitempty"`
	Rules []ResponseModelRule `json:"rules,omitempty"`
}

type targetSchedulePayload struct {
	ScheduleMode      string        `json:"schedule_mode,omitempty"`
	TimingPolicy      *TimingPolicy `json:"timing_policy,omitempty"`
	ScheduleStage     string        `json:"schedule_stage,omitempty"`
	RecoveryPasses    int           `json:"recovery_passes,omitempty"`
	IsolationRechecks int           `json:"isolation_rechecks,omitempty"`
	RetryPromptID     string        `json:"retry_prompt_id,omitempty"`
}

func marshalResponseRulesJSON(t *Target) string {
	if t.ResponseRuleMode == "" && len(t.ResponseModelRules) == 0 {
		return ""
	}
	b, _ := json.Marshal(targetResponseRulesPayload{Mode: t.ResponseRuleMode, Rules: t.ResponseModelRules})
	return string(b)
}

func unmarshalResponseRulesJSON(t *Target, raw string) {
	if raw == "" {
		return
	}
	var p targetResponseRulesPayload
	if err := json.Unmarshal([]byte(raw), &p); err == nil {
		t.ResponseRuleMode = p.Mode
		t.ResponseModelRules = p.Rules
	}
}

func marshalScheduleJSON(t *Target) string {
	payload := targetSchedulePayload{
		ScheduleMode:      t.ScheduleMode,
		TimingPolicy:      t.TimingPolicy,
		ScheduleStage:     t.ScheduleStage,
		RecoveryPasses:    t.RecoveryPasses,
		IsolationRechecks: t.IsolationRechecks,
		RetryPromptID:     t.RetryPromptID,
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

func unmarshalScheduleJSON(t *Target, raw string) {
	if raw == "" || raw == "{}" {
		if t.ScheduleStage == "" {
			t.ScheduleStage = DefaultStageForState(t.State)
		}
		return
	}
	var p targetSchedulePayload
	if err := json.Unmarshal([]byte(raw), &p); err == nil {
		t.ScheduleMode = p.ScheduleMode
		t.TimingPolicy = p.TimingPolicy
		t.ScheduleStage = p.ScheduleStage
		t.RecoveryPasses = p.RecoveryPasses
		t.IsolationRechecks = p.IsolationRechecks
		t.RetryPromptID = p.RetryPromptID
	}
	if t.ScheduleStage == "" {
		t.ScheduleStage = DefaultStageForState(t.State)
	}
}

// Block sources shown in the console. One blocked set is shared by the request
// gate, the supplier proxy and route isolation; the source records who set it.
const (
	BlockSourceManual = "manual"
	BlockSourceRule   = "rule"
	BlockSourceProbe  = "probe"
)

// blockedAt is independent of monitoring: stopping probes never releases a
// hold. A rule hold also survives a passing probe; only manual allow or its
// own expiry releases it.
func (t *Target) blockedAt(now time.Time) bool {
	if t.AllowUntil.After(now) {
		return false
	}
	if t.ManualHold || t.RuleHoldUntil.After(now) {
		return true
	}
	return t.State == StateFailed || t.State == StateReview || t.State == StateUnavailable
}

// blockSourceOf names what currently blocks the target, "" when it is open.
func (t *Target) blockSourceOf(now time.Time) string {
	switch {
	case t.ManualHold:
		return BlockSourceManual
	case t.RuleHoldUntil.After(now):
		return BlockSourceRule
	case t.State == StateFailed || t.State == StateReview || t.State == StateUnavailable:
		return BlockSourceProbe
	}
	return ""
}

func scanTarget(row interface{ Scan(...any) error }) (*Target, error) {
	var (
		t                          Target
		promptIDs                  string
		scheduleJSON               string
		responseRulesJSON          string
		next, checked, allow, hold int64
	)
	err := row.Scan(&t.Model, &t.Enabled, &promptIDs, &t.IntervalMin, &t.FailRecheckMin,
		&next, &t.State, &t.Reason, &checked, &allow, &t.FailStreak, &t.ManualHold, &hold, &scheduleJSON, &responseRulesJSON)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(promptIDs), &t.PromptIDs)
	t.NextCheckAt = time.Unix(next, 0).UTC()
	t.CheckedAt = time.Unix(checked, 0).UTC()
	t.AllowUntil = time.Unix(allow, 0).UTC()
	t.RuleHoldUntil = time.Unix(hold, 0).UTC()
	now := time.Now()
	t.Blocked = t.blockedAt(now)
	t.BlockSource = t.blockSourceOf(now)
	unmarshalScheduleJSON(&t, scheduleJSON)
	unmarshalResponseRulesJSON(&t, responseRulesJSON)
	return &t, nil
}

const targetCols = `model,enabled,prompt_ids,interval_min,fail_recheck_min,next_check_at,state,reason,checked_at,allow_until,fail_streak,manual_hold,rule_hold_until,schedule_json,response_rules_json`

// SaveTarget inserts or updates a target.
func (s *Store) SaveTarget(t *Target) error {
	if t.PromptIDs == nil {
		t.PromptIDs = []string{}
	}
	ids, _ := json.Marshal(t.PromptIDs)
	sj := marshalScheduleJSON(t)
	rr := marshalResponseRulesJSON(t)
	_, err := s.db.Exec(`INSERT INTO targets(`+targetCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	  ON CONFLICT(model) DO UPDATE SET
	    enabled=excluded.enabled, prompt_ids=excluded.prompt_ids,
	    interval_min=excluded.interval_min, fail_recheck_min=excluded.fail_recheck_min,
	    next_check_at=excluded.next_check_at, state=excluded.state, reason=excluded.reason,
	    checked_at=excluded.checked_at, allow_until=excluded.allow_until, fail_streak=excluded.fail_streak,
	    manual_hold=excluded.manual_hold, rule_hold_until=excluded.rule_hold_until,
	    schedule_json=excluded.schedule_json, response_rules_json=excluded.response_rules_json`,
		t.Model, t.Enabled, string(ids), t.IntervalMin, t.FailRecheckMin,
		t.NextCheckAt.Unix(), t.State, t.Reason, t.CheckedAt.Unix(), t.AllowUntil.Unix(), t.FailStreak,
		t.ManualHold, t.RuleHoldUntil.Unix(), sj, rr)
	now := time.Now()
	t.Blocked = t.blockedAt(now)
	t.BlockSource = t.blockSourceOf(now)
	return err
}

// DeleteTarget removes a target.
func (s *Store) DeleteTarget(model string) error {
	_, err := s.db.Exec(`DELETE FROM targets WHERE model=?`, model)
	return err
}

// Target gets one target; returns (nil, nil) when absent.
func (s *Store) Target(model string) (*Target, error) {
	row := s.db.QueryRow(`SELECT `+targetCols+` FROM targets WHERE model=?`, model)
	t, err := scanTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// Targets lists all targets ordered by model.
func (s *Store) Targets() ([]*Target, error) {
	rows, err := s.db.Query(`SELECT ` + targetCols + ` FROM targets ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DueTargets lists enabled targets whose next check time has passed and are
// not mid-check.
func (s *Store) DueTargets(now time.Time) ([]*Target, error) {
	rows, err := s.db.Query(`SELECT `+targetCols+` FROM targets
	  WHERE enabled=1 AND state<>'checking' AND next_check_at<=? ORDER BY next_check_at`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Result is one probe outcome.
type Result struct {
	ID          string    `json:"id"`
	Model       string    `json:"model"`
	PromptID    string    `json:"prompt_id"`
	Answer      string    `json:"answer"`
	Verdict     string    `json:"verdict"` // passed|failed|unavailable|manual
	Detail      string    `json:"detail"`
	ActualModel string    `json:"actual_model,omitempty"`
	RuleID      string    `json:"rule_id,omitempty"`
	Tokens      int       `json:"tokens"`
	CreatedAt   time.Time `json:"created_at"`
	Source      string    `json:"source"` // manual|scheduled|triggered|response
}

// AppendResult stores a probe result and prunes history beyond the retention
// window (keep the newest maxResults rows).
func (s *Store) AppendResult(r *Result) error {
	if _, err := s.db.Exec(`INSERT INTO results(id,model,prompt_id,answer,verdict,detail,actual_model,rule_id,tokens,created_at,source)
	  VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Model, r.PromptID, r.Answer, r.Verdict, r.Detail, r.ActualModel, r.RuleID, r.Tokens, r.CreatedAt.Unix(), r.Source); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM results WHERE id NOT IN (
	  SELECT id FROM results ORDER BY created_at DESC LIMIT ?)`, maxResults)
	return err
}

// SupplierEvent is one protection activity entry shown in the console log.
type SupplierEvent struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"` // protect|restore|retry|action|error
	ProviderID  string    `json:"provider_id,omitempty"`
	Model       string    `json:"model,omitempty"`
	ActualModel string    `json:"actual_model,omitempty"`
	Action      string    `json:"action,omitempty"`
	RuleID      string    `json:"rule_id,omitempty"`
	Detail      string    `json:"detail"`
	CreatedAt   time.Time `json:"created_at"`
}

// AppendSupplierEvent stores one protection event and prunes old entries.
func (s *Store) AppendSupplierEvent(ev *SupplierEvent) error {
	if ev == nil || strings.TrimSpace(ev.Detail) == "" {
		return nil
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	if _, err := s.db.Exec(`INSERT INTO supplier_events(kind,provider_id,model,actual_model,action,rule_id,detail,created_at)
	  VALUES(?,?,?,?,?,?,?,?)`,
		ev.Kind, ev.ProviderID, ev.Model, ev.ActualModel, ev.Action, ev.RuleID, ev.Detail, ev.CreatedAt.Unix()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM supplier_events WHERE id NOT IN (
	  SELECT id FROM supplier_events ORDER BY id DESC LIMIT ?)`, maxSupplierEvents)
	return err
}

// SupplierEvents lists recent protection events, newest first.
func (s *Store) SupplierEvents(limit int) ([]*SupplierEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,kind,provider_id,model,actual_model,action,rule_id,detail,created_at
	  FROM supplier_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SupplierEvent
	for rows.Next() {
		var (
			ev      SupplierEvent
			created int64
		)
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.ProviderID, &ev.Model, &ev.ActualModel, &ev.Action, &ev.RuleID, &ev.Detail, &created); err != nil {
			return nil, err
		}
		ev.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, &ev)
	}
	return out, rows.Err()
}

// Results lists recent results, optionally filtered by model and prompt.
func (s *Store) Results(model, promptID string, limit int) ([]*Result, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id,model,prompt_id,answer,verdict,detail,actual_model,rule_id,tokens,created_at,source FROM results`
	args := []any{}
	where := ""
	if model != "" {
		where += ` WHERE model=?`
		args = append(args, model)
		if promptID != "" {
			where += ` AND prompt_id=?`
			args = append(args, promptID)
		}
	} else if promptID != "" {
		where += ` WHERE prompt_id=?`
		args = append(args, promptID)
	}
	q += where + ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Result
	for rows.Next() {
		var (
			r       Result
			created int64
		)
		if err := rows.Scan(&r.ID, &r.Model, &r.PromptID, &r.Answer, &r.Verdict, &r.Detail, &r.ActualModel, &r.RuleID, &r.Tokens, &created, &r.Source); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CountProbesToday counts probe results for one model since UTC midnight.
func (s *Store) CountProbesToday(model string, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM results
	  WHERE model=? AND created_at>=? AND verdict<>'manual-count' AND source<>'response'`,
		model, dayStartUnix(now)).Scan(&n)
	return n, err
}

func dayStartUnix(now time.Time) int64 {
	utc := now.UTC()
	y, m, d := utc.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

const responseReroutesKey = "response_reroutes"

// AddResponseReroute excludes a real model from route groups until the given
// time, so the next request can take another member.
func (s *Store) AddResponseReroute(model string, until time.Time) error {
	s.rerouteMu.Lock()
	defer s.rerouteMu.Unlock()
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	routes := map[string]int64{}
	ok, err := s.getKV(responseReroutesKey, &routes)
	if err != nil {
		// Never replace an unreadable isolation set with an empty map.
		return err
	}
	if !ok || routes == nil {
		routes = map[string]int64{}
	}
	routes[model] = until.Unix()
	return s.setKV(responseReroutesKey, routes)
}

// ResponseReroutes lists unexpired rerouted models and prunes expired ones.
func (s *Store) ResponseReroutes(now time.Time) ([]string, error) {
	s.rerouteMu.Lock()
	defer s.rerouteMu.Unlock()
	routes := map[string]int64{}
	ok, err := s.getKV(responseReroutesKey, &routes)
	if err != nil || !ok {
		return nil, err
	}
	changed := false
	var out []string
	for model, until := range routes {
		if until > now.Unix() {
			out = append(out, model)
		} else {
			delete(routes, model)
			changed = true
		}
	}
	if changed {
		if err := s.setKV(responseReroutesKey, routes); err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// SeedBuiltins inserts builtin prompts that are missing, and migrates unedited
// legacy candy prompts from contains/21 to number/21 without overwriting user edits.
func (s *Store) SeedBuiltins() error {
	for _, b := range Builtins() {
		var (
			existingID string
			name       string
			text       string
			grader     string
			expect     string
			builtinVal int
			autoVal    int
		)
		err := s.db.QueryRow(`SELECT id,name,text,builtin,grader,expect,automatic FROM prompts WHERE id=?`, b.ID).
			Scan(&existingID, &name, &text, &builtinVal, &grader, &expect, &autoVal)
		if errors.Is(err, sql.ErrNoRows) {
			automatic := b.ID == "candy"
			p := Prompt{ID: b.ID, Name: b.Name, Text: b.Text, Builtin: true, Automatic: automatic}
			if automatic {
				p.Grader = GraderNumber
				p.Expect = "21"
			} else {
				p.Grader = GraderNone
			}
			builtin := 1
			auto := 0
			if p.Automatic {
				auto = 1
			}
			if _, insertErr := s.db.Exec(`INSERT INTO prompts(id,name,text,builtin,grader,expect,automatic)
			  VALUES(?,?,?,?,?,?,?)`,
				p.ID, p.Name, p.Text, builtin, string(p.Grader), p.Expect, auto); insertErr != nil {
				return insertErr
			}
			continue
		}
		if err != nil {
			return err
		}

		// Only migrate unedited original candy (contains/21 with canonical text/name) to number.
		if b.ID == "candy" && name == b.Name && text == b.Text && GraderKind(grader) == GraderContains && expect == "21" {
			if _, updateErr := s.db.Exec(`UPDATE prompts SET grader=? WHERE id=?`, string(GraderNumber), b.ID); updateErr != nil {
				return updateErr
			}
		}
	}
	return nil
}

// Prompt gets one prompt; returns (nil, nil) when absent.
func (s *Store) Prompt(id string) (*Prompt, error) {
	var (
		p         Prompt
		builtin   int
		grader    string
		automatic int
	)
	err := s.db.QueryRow(`SELECT id,name,text,builtin,grader,expect,automatic FROM prompts WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.Text, &builtin, &grader, &p.Expect, &automatic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Builtin = builtin != 0
	p.Grader = GraderKind(grader)
	p.Automatic = automatic != 0
	return &p, nil
}

// DeletePrompt removes a custom prompt (builtin ones are restored on start).
func (s *Store) DeletePrompt(id string) error {
	_, err := s.db.Exec(`DELETE FROM prompts WHERE id=? AND builtin=0`, id)
	return err
}

// SavePrompt inserts or updates a prompt.
func (s *Store) SavePrompt(p *Prompt) error {
	builtin := 0
	if p.Builtin {
		builtin = 1
	}
	automatic := 0
	if p.Automatic {
		automatic = 1
	}
	_, err := s.db.Exec(`INSERT INTO prompts(id,name,text,builtin,grader,expect,automatic)
	  VALUES(?,?,?,?,?,?,?)
	  ON CONFLICT(id) DO UPDATE SET
	    name=excluded.name, text=excluded.text, grader=excluded.grader,
	    expect=excluded.expect, automatic=excluded.automatic`,
		p.ID, p.Name, p.Text, builtin, string(p.Grader), p.Expect, automatic)
	return err
}

// Prompts lists all prompts ordered by builtin desc, name.
func (s *Store) Prompts() ([]*Prompt, error) {
	rows, err := s.db.Query(`SELECT id,name,text,builtin,grader,expect,automatic FROM prompts
	  ORDER BY builtin DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Prompt
	for rows.Next() {
		var (
			p         Prompt
			builtin   int
			grader    string
			automatic int
		)
		if err := rows.Scan(&p.ID, &p.Name, &p.Text, &builtin, &grader, &p.Expect, &automatic); err != nil {
			return nil, err
		}
		p.Builtin = builtin != 0
		p.Grader = GraderKind(grader)
		p.Automatic = automatic != 0
		out = append(out, &p)
	}
	return out, rows.Err()
}
