package check

import (
	"database/sql"
	"errors"
	"time"
)

// Codex account monitoring states. Isolated is the authoritative routing
// hold; the state string is only its presentation plus probe progress.
const (
	CodexStateUnknown     = "unknown"
	CodexStateChecking    = "checking"
	CodexStatePassed      = "passed"
	CodexStateSuspect     = "suspect"
	CodexStateIsolated    = "isolated"
	CodexStateUnavailable = "unavailable"
)

const (
	codexFailThreshold = 2
	maxCodexResults    = 1000
)

// CodexAccountTarget is one monitored Magpie Codex login with its own model.
type CodexAccountTarget struct {
	AccountID      string    `json:"account_id"`
	User           string    `json:"user"`
	Model          string    `json:"model"`
	Enabled        bool      `json:"enabled"`
	State          string    `json:"state"`
	Reason         string    `json:"reason"`
	CheckedAt      time.Time `json:"checked_at"`
	NextCheckAt    time.Time `json:"next_check_at"`
	FailStreak     int       `json:"fail_streak"`
	Isolated       bool      `json:"isolated"`
	IsolationUntil time.Time `json:"isolation_until"`
	LastPromptID   string    `json:"last_prompt_id"`
	LastAnswer     string    `json:"last_answer"`
	LastTokens     int       `json:"last_tokens"`
	Missing        bool      `json:"missing"`
}

// CodexAccountResult is one account probe. It never contains credentials.
type CodexAccountResult struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	User      string    `json:"user"`
	Model     string    `json:"model"`
	PromptID  string    `json:"prompt_id"`
	Answer    string    `json:"answer"`
	Verdict   string    `json:"verdict"`
	Detail    string    `json:"detail"`
	Tokens    int       `json:"tokens"`
	CreatedAt time.Time `json:"created_at"`
}

const codexTargetCols = `account_id,user,model,enabled,state,reason,checked_at,next_check_at,fail_streak,isolated,isolation_until,last_prompt_id,last_answer,last_tokens,missing`

func scanCodexTarget(row interface{ Scan(...any) error }) (*CodexAccountTarget, error) {
	var t CodexAccountTarget
	var checked, next, until int64
	err := row.Scan(&t.AccountID, &t.User, &t.Model, &t.Enabled, &t.State, &t.Reason,
		&checked, &next, &t.FailStreak, &t.Isolated, &until, &t.LastPromptID, &t.LastAnswer, &t.LastTokens, &t.Missing)
	if err != nil {
		return nil, err
	}
	t.CheckedAt = unixTime(checked)
	t.NextCheckAt = unixTime(next)
	t.IsolationUntil = unixTime(until)
	return &t, nil
}

func unixTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// SaveCodexAccountTarget inserts or updates one account target.
func (s *Store) SaveCodexAccountTarget(t *CodexAccountTarget) error {
	_, err := s.db.Exec(`INSERT INTO codex_account_targets(`+codexTargetCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	  ON CONFLICT(account_id) DO UPDATE SET
	    user=excluded.user, model=excluded.model, enabled=excluded.enabled,
	    state=excluded.state, reason=excluded.reason, checked_at=excluded.checked_at,
	    next_check_at=excluded.next_check_at, fail_streak=excluded.fail_streak,
	    isolated=excluded.isolated, isolation_until=excluded.isolation_until,
	    last_prompt_id=excluded.last_prompt_id, last_answer=excluded.last_answer,
	    last_tokens=excluded.last_tokens, missing=excluded.missing`,
		t.AccountID, t.User, t.Model, t.Enabled, t.State, t.Reason,
		unixOrZero(t.CheckedAt), unixOrZero(t.NextCheckAt), t.FailStreak, t.Isolated,
		unixOrZero(t.IsolationUntil), t.LastPromptID, t.LastAnswer, t.LastTokens, t.Missing)
	return err
}

// UpdateCodexAccountPreflightFailure records a failure that happened before a
// probe claimed the target. It updates no identity or roster fields, and it
// never inserts a row deleted while the probe was being prepared.
func (s *Store) UpdateCodexAccountPreflightFailure(t *CodexAccountTarget, expectedState string, expectedChecked time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE codex_account_targets SET
	    checked_at=?, state=?, reason=?, next_check_at=?, isolated=?, isolation_until=?
	  WHERE account_id=? AND enabled=1 AND missing=0 AND model=? AND user=? AND state=? AND checked_at=?`,
		unixOrZero(t.CheckedAt), t.State, t.Reason, unixOrZero(t.NextCheckAt),
		t.Isolated, unixOrZero(t.IsolationUntil), t.AccountID, t.Model, t.User,
		expectedState, unixOrZero(expectedChecked))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ResetCodexAccountChecking clears a checking state that has no committed
// result, without touching any other field.
func (s *Store) ResetCodexAccountChecking(accountID, state, reason string, next time.Time) error {
	_, err := s.db.Exec(`UPDATE codex_account_targets SET state=?, reason=?, next_check_at=?
	  WHERE account_id=? AND state=?`,
		state, reason, unixOrZero(next), accountID, CodexStateChecking)
	return err
}

// UpdateCodexAccountReason updates a committed probe result's reason only if
// the same model and state are still current, so a later user action wins.
func (s *Store) UpdateCodexAccountReason(t *CodexAccountTarget, reason string) error {
	_, err := s.db.Exec(`UPDATE codex_account_targets SET reason=?
	  WHERE account_id=? AND model=? AND state=? AND checked_at=?`,
		reason, t.AccountID, t.Model, t.State, unixOrZero(t.CheckedAt))
	return err
}

// MarkCodexAccountChecking starts a probe only if the target is still the
// same enabled model, so a manual recovery, disable, or model change cannot be
// overwritten by a stale probe start.
func (s *Store) MarkCodexAccountChecking(accountID, model, reason string) (bool, error) {
	res, err := s.db.Exec(`UPDATE codex_account_targets SET state=?, reason=?, next_check_at=0
	  WHERE account_id=? AND model=? AND enabled=1 AND missing=0 AND state<>?`,
		CodexStateChecking, reason, accountID, model, CodexStateChecking)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SaveCodexAccountProbeResult commits probe-owned fields only while the
// target is still the same model and still checking. A manual recovery, model
// change, disable, or delete wins the race and the stale result is discarded.
func (s *Store) SaveCodexAccountProbeResult(t *CodexAccountTarget, model string) (bool, error) {
	res, err := s.db.Exec(`UPDATE codex_account_targets SET
	    checked_at=?, state=?, reason=?, next_check_at=?, fail_streak=?,
	    isolated=?, isolation_until=?, last_prompt_id=?, last_answer=?, last_tokens=?
	  WHERE account_id=? AND state=? AND model=? AND missing=0`,
		unixOrZero(t.CheckedAt), t.State, t.Reason, unixOrZero(t.NextCheckAt), t.FailStreak,
		t.Isolated, unixOrZero(t.IsolationUntil), t.LastPromptID, t.LastAnswer, t.LastTokens,
		t.AccountID, CodexStateChecking, model)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CodexAccountTarget reads one account target; (nil,nil) when absent.
func (s *Store) CodexAccountTarget(accountID string) (*CodexAccountTarget, error) {
	row := s.db.QueryRow(`SELECT `+codexTargetCols+` FROM codex_account_targets WHERE account_id=?`, accountID)
	t, err := scanCodexTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// CodexAccountTargets lists all account targets ordered by user.
func (s *Store) CodexAccountTargets() ([]*CodexAccountTarget, error) {
	rows, err := s.db.Query(`SELECT ` + codexTargetCols + ` FROM codex_account_targets ORDER BY user, account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CodexAccountTarget
	for rows.Next() {
		t, err := scanCodexTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateCodexAccountRoster updates only the account metadata a roster refresh
// owns. It deliberately leaves probe state, isolation, and their timestamps
// alone, so a concurrent probe completion or management action cannot be
// overwritten by a stale full-row snapshot.
func (s *Store) UpdateCodexAccountRoster(accountID, user string, missing bool) error {
	_, err := s.db.Exec(`UPDATE codex_account_targets SET user=?, missing=? WHERE account_id=?`,
		user, missing, accountID)
	return err
}

// DueCodexAccountTargets lists enabled, non-missing targets whose next check
// is due and which are not checking.
func (s *Store) DueCodexAccountTargets(now time.Time) ([]*CodexAccountTarget, error) {
	rows, err := s.db.Query(`SELECT `+codexTargetCols+` FROM codex_account_targets
	  WHERE enabled=1 AND missing=0 AND state<>? AND next_check_at<=?
	  ORDER BY next_check_at, account_id`, CodexStateChecking, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CodexAccountTarget
	for rows.Next() {
		t, err := scanCodexTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteCodexAccountTarget removes one target.
func (s *Store) DeleteCodexAccountTarget(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM codex_account_targets WHERE account_id=?`, accountID)
	return err
}

// AppendCodexResult saves one account probe result and prunes old rows.
func (s *Store) AppendCodexResult(r *CodexAccountResult) error {
	if r == nil {
		return nil
	}
	if r.ID == "" {
		r.ID = randomHex(8)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if _, err := s.db.Exec(`INSERT INTO codex_account_results(id,account_id,user,model,prompt_id,answer,verdict,detail,tokens,created_at)
	  VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.AccountID, r.User, r.Model, r.PromptID, r.Answer, r.Verdict, r.Detail, r.Tokens, r.CreatedAt.Unix()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM codex_account_results WHERE id NOT IN (
	  SELECT id FROM codex_account_results ORDER BY created_at DESC, rowid DESC LIMIT ?)`, maxCodexResults)
	return err
}

// CountCodexProbesToday counts account probes since local midnight.
func (s *Store) CountCodexProbesToday(accountID string, now time.Time) (int, error) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM codex_account_results WHERE account_id=? AND created_at>=?`,
		accountID, start.Unix()).Scan(&n)
	return n, err
}
