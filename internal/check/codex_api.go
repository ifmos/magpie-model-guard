package check

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func (s *Service) apiCodexAccounts(w http.ResponseWriter, r *http.Request) {
	if s.codex == nil {
		writeJSON(w, map[string]any{"configured": false, "error": "未配置本机 Magpie 目录，无法读取 Codex 账号"})
		return
	}
	writeJSON(w, s.codex.view())
}

func (s *Service) apiCodexTarget(w http.ResponseWriter, r *http.Request) {
	if s.codex == nil {
		errJSON(w, http.StatusServiceUnavailable, "未配置本机 Magpie 目录")
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
		Model     string `json:"model"`
		Enabled   *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if req.Enabled == nil {
		errJSON(w, http.StatusBadRequest, "缺少 enabled")
		return
	}
	if err := s.codex.upsertTarget(req.AccountID, req.Model, *req.Enabled); err != nil {
		errJSON(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, s.codex.view())
}

func (s *Service) apiCodexDelete(w http.ResponseWriter, r *http.Request) {
	if s.codex == nil {
		errJSON(w, http.StatusServiceUnavailable, "未配置本机 Magpie 目录")
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体无效")
		return
	}
	if err := s.codex.deleteTarget(req.AccountID); err != nil {
		errJSON(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, s.codex.view())
}

func (s *Service) apiCodexAction(w http.ResponseWriter, r *http.Request) {
	if s.codex == nil {
		errJSON(w, http.StatusServiceUnavailable, "未配置本机 Magpie 目录")
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
		Action    string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体无效")
		return
	}
	var err error
	switch req.Action {
	case "probe":
		err = s.codex.trigger(req.AccountID)
	case "recover":
		err = s.codex.manualRecover(req.AccountID)
	default:
		errJSON(w, http.StatusBadRequest, "未知操作")
		return
	}
	if err != nil {
		errJSON(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, s.codex.view())
}

// apiCodexResults returns recent probe history for one Codex account.
func (s *Service) apiCodexResults(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	if accountID == "" {
		errJSON(w, http.StatusBadRequest, "缺少 account_id")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	results, next, err := s.store.CodexAccountResults(accountID, limit, r.URL.Query().Get("before"))
	if errors.Is(err, errBadCursor) {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if results == nil {
		results = []*CodexAccountResult{}
	}
	writeJSON(w, pageResponse[*CodexAccountResult]{Items: results, Next: next})
}
