package check

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// auth admits the plugin control token or a browser session cookie.
func auth(access *Access, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if cookie, err := r.Cookie("mg_session"); err == nil && token == "" {
			token = cookie.Value
		}
		if !access.Control(token) && !access.Session(token) {
			errJSON(w, http.StatusUnauthorized, "未授权")
			return
		}
		next(w, r)
	}
}

// registerAPI wires all management endpoints.
func registerAPI(mux *http.ServeMux, s *Service, access *Access, console *Console) {
	h := func(fn http.HandlerFunc) http.Handler { return auth(access, fn) }

	mux.Handle("GET /api/state", h(s.apiState))
	mux.Handle("GET /api/models", h(s.apiModels))
	mux.Handle("GET /api/suppliers", h(s.apiSuppliers))
	mux.Handle("GET /api/supplier-events", h(s.apiSupplierEvents))
	mux.Handle("POST /api/suppliers/protection", h(s.apiSupplierProtection))
	mux.Handle("GET /api/codex/accounts", h(s.apiCodexAccounts))
	mux.Handle("POST /api/codex/accounts/target", h(s.apiCodexTarget))
	mux.Handle("POST /api/codex/accounts/delete", h(s.apiCodexDelete))
	mux.Handle("POST /api/codex/accounts/action", h(s.apiCodexAction))
	mux.Handle("POST /api/config", h(s.apiConfig))
	mux.Handle("POST /api/config/initial", h(s.apiConfigInitial))
	mux.Handle("GET /api/targets", h(s.apiTargets))
	mux.Handle("POST /api/targets", h(s.apiTargetUpsert))
	mux.Handle("POST /api/targets/delete", h(s.apiTargetDelete))
	mux.Handle("POST /api/targets/action", h(s.apiTargetAction))
	mux.Handle("POST /api/probe", h(s.apiProbe))
	mux.Handle("GET /api/results", h(s.apiResults))
	mux.Handle("GET /api/prompts", h(s.apiPrompts))
	mux.Handle("POST /api/prompts", h(s.apiPromptUpsert))
	mux.Handle("POST /api/prompts/delete", h(s.apiPromptDelete))
	mux.Handle("GET /api/health", h(s.apiHealth))
	mux.Handle("POST /api/response-model", controlRequired(access, s.apiResponseModel))
	mux.Handle("POST /api/console", controlRequired(access, s.apiConsoleOpen(console)))
	mux.Handle("POST /api/console/close", h(s.apiConsoleClose(console)))
}

// apiSupplierEvents returns the recent supplier protection log.
func (s *Service) apiSupplierEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.SupplierEvents(limit)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, events)
}

func (s *Service) apiState(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Snapshot()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, snap)
}

func (s *Service) apiModels(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	routes := s.routes
	s.mu.Unlock()
	if routes == nil {
		errJSON(w, 503, "未配置本机 Magpie 目录，请使用 -magpie-config 指定")
		return
	}
	models, err := routes.listModels(r.Context())
	if err != nil {
		errJSON(w, 502, err.Error())
		return
	}
	writeJSON(w, models)
}

func (s *Service) apiConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GuardConfig
		RecalculateSchedule bool `json:"recalculate_schedule"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		errJSON(w, 400, "请求体无效")
		return
	}
	recalc := req.RecalculateSchedule || r.URL.Query().Get("recalculate_schedule") == "true"
	if err := s.configure(req.GuardConfig, recalc); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	s.TargetsDueNow()
	writeJSON(w, map[string]string{"ok": "saved"})
}

func (s *Service) apiConfigInitial(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GuardConfig
		RecalculateSchedule bool `json:"recalculate_schedule"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		errJSON(w, 400, "请求体无效")
		return
	}
	if err := s.ConfigureInitial(req.GuardConfig); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "saved"})
}

func (s *Service) apiTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.store.Targets()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, targets)
}

func (s *Service) apiTargetUpsert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target
		RecalculateSchedule bool `json:"recalculate_schedule"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		errJSON(w, 400, "请求体无效")
		return
	}
	recalc := req.RecalculateSchedule || r.URL.Query().Get("recalculate_schedule") == "true"
	if err := s.UpsertTargetOpt(&req.Target, recalc); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, req.Target)
}

func (s *Service) apiTargetDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil || req.Model == "" {
		errJSON(w, 400, "请求体无效")
		return
	}
	if err := s.DeleteTarget(req.Model); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "deleted"})
}

func (s *Service) apiTargetAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil || req.Model == "" || req.Action == "" {
		errJSON(w, 400, "请求体无效")
		return
	}
	if err := s.TargetAction(req.Model, req.Action); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	s.TargetsDueNow()
	writeJSON(w, map[string]string{"ok": "done"})
}

func (s *Service) apiProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		PromptID string `json:"prompt_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil || req.Model == "" {
		errJSON(w, 400, "请求体无效")
		return
	}
	res, err := s.RunManualProbe(req.Model, req.PromptID)
	if err != nil {
		status := 500
		if res != nil {
			status = 502
		}
		errJSON(w, status, err.Error())
		return
	}
	writeJSON(w, res)
}

func (s *Service) apiResults(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	results, err := s.Results(q.Get("model"), q.Get("prompt"), limit)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, results)
}

func (s *Service) apiPrompts(w http.ResponseWriter, r *http.Request) {
	prompts, err := s.store.Prompts()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, prompts)
}

func (s *Service) apiPromptUpsert(w http.ResponseWriter, r *http.Request) {
	var p Prompt
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&p); err != nil {
		errJSON(w, 400, "请求体无效")
		return
	}
	if err := s.UpsertPrompt(&p); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, p)
}

func (s *Service) apiPromptDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil || req.ID == "" {
		errJSON(w, 400, "请求体无效")
		return
	}
	if err := s.DeletePrompt(req.ID); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "deleted"})
}

func (s *Service) apiHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"health": s.Health(), "time": time.Now().UTC()})
}

// apiResponseModel accepts a completed business reply's real model from the
// Magpie async hook. Actions run in the background so the hook never waits on
// routing reconciliation.
func (s *Service) apiResponseModel(w http.ResponseWriter, r *http.Request) {
	var ev ResponseModelEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&ev); err != nil {
		errJSON(w, 400, "请求体无效")
		return
	}
	go func() {
		_ = s.HandleResponseModel(ev)
	}()
	writeJSON(w, map[string]bool{"accepted": true})
}

// controlRequired admits only the plugin control token, not browser sessions.
func controlRequired(access *Access, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !access.Control(token) {
			errJSON(w, http.StatusForbidden, "仅插件可调用此接口")
			return
		}
		next(w, r)
	})
}

// apiConsoleOpen issues a local or remote one-time console link.
func (s *Service) apiConsoleOpen(console *Console) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if console == nil {
			errJSON(w, http.StatusServiceUnavailable, "控制台管理未启用")
			return
		}
		var input struct {
			Remote bool   `json:"remote"`
			Binary string `json:"binary"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&input); err != nil {
			errJSON(w, 400, "请求体无效")
			return
		}
		if input.Binary != "" {
			console.SetBinary(input.Binary)
		}
		link, err := console.Open(r.Context(), input.Remote)
		if err != nil {
			errJSON(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, map[string]string{"url": link})
	}
}

// apiConsoleClose tears the tunnel down after acknowledging.
func (s *Service) apiConsoleClose(console *Console) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if console != nil {
			// Let the tunnel deliver the acknowledgement before ending transport.
			time.AfterFunc(300*time.Millisecond, console.Close)
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}
