package check

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCodexAccountAPI(t *testing.T) {
	svc, store, _, _, _ := newCodexMonitorTest(t)
	access := NewAccess("", "")
	token := access.SetControl()
	mux := http.NewServeMux()
	registerAPI(mux, svc, access, nil)
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	rec := do(http.MethodGet, "/api/codex/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET accounts: %d %s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if _, ok := view["accounts"]; !ok {
		t.Fatalf("accounts missing: %#v", view)
	}
	rec = do(http.MethodPost, "/api/codex/accounts/target", map[string]any{
		"account_id": "bbbbbbbbbbbbbbbb", "model": "codex/gpt-5.4", "enabled": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST target: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodPost, "/api/codex/accounts/target", map[string]any{
		"account_id": "bbbbbbbbbbbbbbbb", "model": "gpt-5.4", "enabled": true,
	})
	if rec.Code == http.StatusOK {
		t.Fatal("non-codex model accepted")
	}
	if err := store.AppendCodexResult(&CodexAccountResult{
		ID: "r1", AccountID: "bbbbbbbbbbbbbbbb", User: "b@example.com",
		Model: "codex/gpt-5.4", PromptID: "candy", Verdict: "passed",
	}); err != nil {
		t.Fatal(err)
	}
	rec = do(http.MethodGet, "/api/codex/accounts/results?account_id=bbbbbbbbbbbbbbbb&limit=5", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET results: %d %s", rec.Code, rec.Body.String())
	}
	var history pageResponse[CodexAccountResult]
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 1 || history.Items[0].Verdict != "passed" || history.Next != "" {
		t.Fatalf("results = %+v", history)
	}
	rec = do(http.MethodPost, "/api/codex/accounts/delete", map[string]any{"account_id": "bbbbbbbbbbbbbbbb"})
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE target: %d %s", rec.Code, rec.Body.String())
	}
}
