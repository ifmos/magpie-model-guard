package check

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// auth wraps a handler with bearer-token auth.
func (s *Service) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !s.CheckConsoleToken(token) {
			errJSON(w, http.StatusUnauthorized, "未授权")
			return
		}
		next(w, r)
	}
}

// registerAPI wires all management endpoints.
func registerAPI(mux *http.ServeMux, s *Service) {
	h := func(fn http.HandlerFunc) http.Handler { return s.auth(fn) }

	mux.Handle("GET /api/state", h(s.apiState))
	mux.Handle("GET /api/models", h(s.apiModels))
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
	if err := s.ConfigureOpt(req.GuardConfig, recalc); err != nil {
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
	results, err := s.store.Results(q.Get("model"), q.Get("prompt"), limit)
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
