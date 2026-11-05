package check

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func seedResults(t *testing.T, store *Store, n int) []*Result {
	t.Helper()
	base := time.Unix(1700000000, 0).UTC()
	out := make([]*Result, 0, n)
	for i := 0; i < n; i++ {
		r := &Result{
			ID:       randomHex(8),
			Model:    "vendor/model",
			PromptID: "candy",
			Answer:   "answer",
			Verdict:  VerdictPassed,
			// Every third row shares a timestamp so the id tie-break is used.
			CreatedAt: base.Add(time.Duration(i/3) * time.Minute),
			Source:    "manual",
		}
		if err := store.AppendResult(r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestResultsPaginationCoversEveryRowOnce(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/guard.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seeded := seedResults(t, store, 7)

	seen := map[string]int{}
	before := ""
	for page := 0; page < 10; page++ {
		rows, next, err := store.Results("", "", 2, before)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 2 {
			t.Fatalf("page %d returned %d rows", page, len(rows))
		}
		for i, r := range rows {
			seen[r.ID]++
			if i > 0 {
				prev := rows[i-1]
				if prev.CreatedAt.Before(r.CreatedAt) || (prev.CreatedAt.Equal(r.CreatedAt) && prev.ID < r.ID) {
					t.Fatalf("page %d not ordered newest first: %v then %v", page, prev.ID, r.ID)
				}
			}
		}
		if next == "" {
			break
		}
		before = next
	}
	if len(seen) != len(seeded) {
		t.Fatalf("paged results saw %d of %d rows", len(seen), len(seeded))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("result %s returned %d times", id, n)
		}
	}
}

func TestResultsPaginationRejectsBadCursor(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/guard.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, cursor := range []string{"!!", "bm90LWEtY3Vyc29y", "MTIzOg"} {
		if _, _, err := store.Results("", "", 10, cursor); err != errBadCursor {
			t.Fatalf("cursor %q error = %v, want errBadCursor", cursor, err)
		}
	}
}

func TestSupplierEventsPagination(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/guard.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for i := 0; i < 5; i++ {
		if err := store.AppendSupplierEvent(&SupplierEvent{Kind: "retry", Detail: "hit"}); err != nil {
			t.Fatal(err)
		}
	}
	var ids []int64
	before := ""
	for page := 0; page < 10; page++ {
		events, next, err := store.SupplierEvents(2, before)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			ids = append(ids, ev.ID)
		}
		if next == "" {
			break
		}
		before = next
	}
	if len(ids) != 5 {
		t.Fatalf("paged events = %d, want 5", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] >= ids[i-1] {
			t.Fatalf("events not newest first: %v", ids)
		}
	}
	if _, _, err := store.SupplierEvents(2, "abc"); err != errBadCursor {
		t.Fatalf("bad id cursor error = %v", err)
	}
}

func TestCodexAccountResultsPaginationIsPerAccount(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/guard.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := time.Unix(1700000000, 0).UTC()
	for i, account := range []string{"aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"} {
		if err := store.AppendCodexResult(&CodexAccountResult{
			ID: randomHex(8), AccountID: account, User: account + "@example.com",
			Model: "codex/gpt-5.4", PromptID: "candy", Verdict: VerdictPassed,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, next, err := store.CodexAccountResults("aaaaaaaaaaaaaaaa", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || next == "" {
		t.Fatalf("first page = %d rows, next %q", len(rows), next)
	}
	for _, r := range rows {
		if r.AccountID != "aaaaaaaaaaaaaaaa" {
			t.Fatalf("page leaked another account: %+v", r)
		}
	}
	rest, next, err := store.CodexAccountResults("aaaaaaaaaaaaaaaa", 2, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || next != "" {
		t.Fatalf("second page = %d rows, next %q", len(rest), next)
	}
	if rest[0].CreatedAt.After(rows[len(rows)-1].CreatedAt) {
		t.Fatal("second page is not older than the first")
	}
}

func TestPagedListHandlers(t *testing.T) {
	svc, store, _ := newTestService(t, nil)
	seedResults(t, store, 5)
	for i := 0; i < 3; i++ {
		if err := store.AppendSupplierEvent(&SupplierEvent{Kind: "retry", Detail: "hit"}); err != nil {
			t.Fatal(err)
		}
	}
	access := NewAccess("", "")
	token := access.SetControl()
	mux := http.NewServeMux()
	registerAPI(mux, svc, access, nil)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Next  string           `json:"next"`
	}
	rec := get("/api/results?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET results: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Next == "" {
		t.Fatalf("results page = %d items, next %q", len(page.Items), page.Next)
	}
	rec = get("/api/results?limit=2&before=" + page.Next)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET results page 2: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("results page 2 = %d items", len(page.Items))
	}
	rec = get("/api/results?before=not-a-cursor")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad results cursor = %d, want 400", rec.Code)
	}
	rec = get("/api/supplier-events?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Next == "" {
		t.Fatalf("events page = %d items, next %q", len(page.Items), page.Next)
	}
	rec = get("/api/supplier-events?before=x")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad events cursor = %d, want 400", rec.Code)
	}
}

func TestResultsFullActivePageStillReachesHistory(t *testing.T) {
	svc, store, _ := newTestService(t, nil)
	seeded := seedResults(t, store, 3)
	svc.mu.Lock()
	svc.running["active/model"] = &runningProbe{
		id: "active", model: "active/model", active: true, startedAt: time.Now().UTC(),
	}
	svc.mu.Unlock()
	rows, next, err := svc.Results("", "", 1, "")
	if err != nil || len(rows) != 1 || rows[0].Verdict != StateChecking || next == "" {
		t.Fatalf("active page = %+v, %q, %v", rows, next, err)
	}
	seen := map[string]bool{}
	for next != "" {
		rows, next, err = svc.Results("", "", 1, next)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Verdict == StateChecking || seen[row.ID] {
				t.Fatalf("invalid history row: %+v", row)
			}
			seen[row.ID] = true
		}
	}
	if len(seen) != len(seeded) {
		t.Fatalf("history reached %d of %d rows", len(seen), len(seeded))
	}
}

func TestResultsCursorSurvivesNewRowsAndFilters(t *testing.T) {
	_, store, _ := newTestService(t, nil)
	at := time.Unix(1700000000, 0).UTC()
	for _, r := range []*Result{
		{ID: "a", Model: "wanted", PromptID: "p", CreatedAt: at},
		{ID: "b", Model: "wanted", PromptID: "p", CreatedAt: at},
		{ID: "c", Model: "wanted", PromptID: "p", CreatedAt: at},
		{ID: "other-model", Model: "other", PromptID: "p", CreatedAt: at},
		{ID: "other-prompt", Model: "wanted", PromptID: "q", CreatedAt: at},
	} {
		if err := store.AppendResult(r); err != nil {
			t.Fatal(err)
		}
	}
	first, next, err := store.Results("wanted", "p", 1, "")
	if err != nil || len(first) != 1 || first[0].ID != "c" || next == "" {
		t.Fatalf("first page: %+v %q %v", first, next, err)
	}
	if err := store.AppendResult(&Result{ID: "new", Model: "wanted", PromptID: "p", CreatedAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	rest, next, err := store.Results("wanted", "p", 10, next)
	if err != nil || len(rest) != 2 || rest[0].ID != "b" || rest[1].ID != "a" || next != "" {
		t.Fatalf("history after insert: %+v %q %v", rest, next, err)
	}
}
