package check

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

// maxResults is the retention cap for probe results (newest kept).
const maxResults = 500

// Store persists guard state in SQLite.
type Store struct {
	db *sql.DB
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
  fail_streak      INTEGER NOT NULL DEFAULT 0
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
  detail     TEXT NOT NULL DEFAULT '',
  tokens     INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  source     TEXT NOT NULL DEFAULT 'manual'
);
CREATE INDEX IF NOT EXISTS idx_results_model ON results(model, created_at DESC);
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
	}
	return err
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
	GatewayURL         string `json:"gateway_url"`
	GatewayKey         string `json:"gateway_key"`
	DailyLimit         int    `json:"daily_limit"`          // probe requests per model per day
	TimeoutSec         int    `json:"timeout_sec"`          // per probe request
	CoolMin            int    `json:"cool_min"`             // unavailable cool-down minutes
	DefaultIntervalMin int    `json:"default_interval_min"` // default scheduled interval
	ProbeMaxTokens     int    `json:"probe_max_tokens"`
}

func defaultConfig() GuardConfig {
	return GuardConfig{
		DailyLimit:         120,
		TimeoutSec:         180,
		CoolMin:            15,
		DefaultIntervalMin: 30,
		ProbeMaxTokens:     8192,
	}
}

// LoadConfig reads config, applying defaults for zero fields.
func (s *Store) LoadConfig() (GuardConfig, error) {
	c := defaultConfig()
	_, err := s.getKV("config", &c)
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
}

// blockedAt is independent of monitoring: stopping probes never releases a hold.
func (t *Target) blockedAt(now time.Time) bool {
	if t.AllowUntil.After(now) {
		return false
	}
	return t.ManualHold || t.State == StateFailed || t.State == StateReview || t.State == StateUnavailable
}

func scanTarget(row interface{ Scan(...any) error }) (*Target, error) {
	var (
		t                    Target
		promptIDs            string
		next, checked, allow int64
	)
	err := row.Scan(&t.Model, &t.Enabled, &promptIDs, &t.IntervalMin, &t.FailRecheckMin,
		&next, &t.State, &t.Reason, &checked, &allow, &t.FailStreak, &t.ManualHold)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(promptIDs), &t.PromptIDs)
	t.NextCheckAt = time.Unix(next, 0).UTC()
	t.CheckedAt = time.Unix(checked, 0).UTC()
	t.AllowUntil = time.Unix(allow, 0).UTC()
	t.Blocked = t.blockedAt(time.Now())
	return &t, nil
}

const targetCols = `model,enabled,prompt_ids,interval_min,fail_recheck_min,next_check_at,state,reason,checked_at,allow_until,fail_streak,manual_hold`

// SaveTarget inserts or updates a target.
func (s *Store) SaveTarget(t *Target) error {
	if t.PromptIDs == nil {
		t.PromptIDs = []string{}
	}
	ids, _ := json.Marshal(t.PromptIDs)
	_, err := s.db.Exec(`INSERT INTO targets(`+targetCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
	  ON CONFLICT(model) DO UPDATE SET
	    enabled=excluded.enabled, prompt_ids=excluded.prompt_ids,
	    interval_min=excluded.interval_min, fail_recheck_min=excluded.fail_recheck_min,
	    next_check_at=excluded.next_check_at, state=excluded.state, reason=excluded.reason,
	    checked_at=excluded.checked_at, allow_until=excluded.allow_until, fail_streak=excluded.fail_streak,
	    manual_hold=excluded.manual_hold`,
		t.Model, t.Enabled, string(ids), t.IntervalMin, t.FailRecheckMin,
		t.NextCheckAt.Unix(), t.State, t.Reason, t.CheckedAt.Unix(), t.AllowUntil.Unix(), t.FailStreak, t.ManualHold)
	t.Blocked = t.blockedAt(time.Now())
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
	ID        string    `json:"id"`
	Model     string    `json:"model"`
	PromptID  string    `json:"prompt_id"`
	Answer    string    `json:"answer"`
	Verdict   string    `json:"verdict"` // passed|failed|unavailable|manual
	Detail    string    `json:"detail"`
	Tokens    int       `json:"tokens"`
	CreatedAt time.Time `json:"created_at"`
	Source    string    `json:"source"` // manual|scheduled|triggered
}

// AppendResult stores a probe result and prunes history beyond the retention
// window (keep the newest maxResults rows).
func (s *Store) AppendResult(r *Result) error {
	if _, err := s.db.Exec(`INSERT INTO results(id,model,prompt_id,answer,verdict,detail,tokens,created_at,source)
	  VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Model, r.PromptID, r.Answer, r.Verdict, r.Detail, r.Tokens, r.CreatedAt.Unix(), r.Source); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM results WHERE id NOT IN (
	  SELECT id FROM results ORDER BY created_at DESC LIMIT ?)`, maxResults)
	return err
}

// Results lists recent results, optionally filtered by model and prompt.
func (s *Store) Results(model, promptID string, limit int) ([]*Result, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id,model,prompt_id,answer,verdict,detail,tokens,created_at,source FROM results`
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
		if err := rows.Scan(&r.ID, &r.Model, &r.PromptID, &r.Answer, &r.Verdict, &r.Detail, &r.Tokens, &created, &r.Source); err != nil {
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
	  WHERE model=? AND created_at>=? AND verdict<>'manual-count'`,
		model, dayStartUnix(now)).Scan(&n)
	return n, err
}

func dayStartUnix(now time.Time) int64 {
	utc := now.UTC()
	y, m, d := utc.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
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
