package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

const (
	minIdempotencyLen = 8
	maxIdempotencyLen = 128
)

type createRunResponse struct {
	workflow.RunSummary
	Links map[string]string `json:"links"`
}

// createRun is shared by the JSON API and the HTML UI.
func (s *Server) createRun(ctx context.Context, req workflow.CreateRunRequest, idem, actor string) (workflow.RunSummary, bool, error) {
	idem = strings.TrimSpace(idem)
	if len(idem) < minIdempotencyLen || len(idem) > maxIdempotencyLen {
		return workflow.RunSummary{}, false, errs.New(errs.ClassValidation, "run.create",
			fmt.Sprintf("Idempotency-Key must be %d..%d characters", minIdempotencyLen, maxIdempotencyLen))
	}
	req.IdempotencyKey = idem
	params, err := s.Engine.PrepareRun(req, "operator", time.Now().UTC())
	if err != nil {
		return workflow.RunSummary{}, false, err
	}
	run, created, err := s.Store.CreateRun(ctx, params)
	if err != nil {
		if errors.Is(err, storage.ErrIdempotencyConflict) {
			return workflow.RunSummary{}, false, errs.New(errs.ClassConflict, "run.create",
				"idempotency key was reused with a different request body")
		}
		s.Log.ErrorContext(ctx, "run creation failed", "error", err)
		return workflow.RunSummary{}, false, errs.Wrap(errs.ClassInternal, "run.create", "could not create run", err)
	}
	if err := s.Store.Audit(ctx, actor, "run.create", run.ID, map[string]any{
		"template": run.Template, "mode": run.Mode, "created": created,
	}); err != nil {
		s.Log.WarnContext(ctx, "audit write failed", "action", "run.create", "run_id", run.ID)
	}
	return s.Engine.Summarize(ctx, run), created, nil
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req workflow.CreateRunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if headerKey == "" {
		writeError(w, r, errs.New(errs.ClassValidation, "run.create", "Idempotency-Key header is required"))
		return
	}
	if req.IdempotencyKey != "" && req.IdempotencyKey != headerKey {
		writeError(w, r, errs.New(errs.ClassValidation, "run.create",
			"body idempotency_key must match the Idempotency-Key header"))
		return
	}
	sum, created, err := s.createRun(r.Context(), req, headerKey, "operator")
	if err != nil {
		writeError(w, r, err)
		return
	}
	_ = created
	resp := createRunResponse{
		RunSummary: sum,
		Links:      map[string]string{"self": "/v1/runs/" + sum.ID},
	}
	w.Header().Set("Location", "/v1/runs/"+sum.ID)
	writeJSON(w, http.StatusAccepted, resp)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := strings.ToUpper(strings.TrimSpace(q.Get("status")))
	if status != "" && !knownRunState(status) {
		writeError(w, r, errs.New(errs.ClassValidation, "run.list", "unknown status filter"))
		return
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeError(w, r, errs.New(errs.ClassValidation, "run.list", "limit must be 1..100"))
			return
		}
		limit = n
	}
	template := q.Get("template")
	page, err := s.Store.ListRuns(r.Context(), storage.RunListFilter{
		Status: status, Template: template, Limit: limit, Cursor: q.Get("cursor"),
	})
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			writeError(w, r, errs.New(errs.ClassValidation, "run.list", "invalid cursor"))
			return
		}
		writeError(w, r, errs.Wrap(errs.ClassInternal, "run.list", "could not list runs", err))
		return
	}
	info, _ := authInfoFrom(r)
	items := make([]workflow.RunSummary, 0, len(page.Items))
	for _, run := range page.Items {
		items = append(items, s.summarizeFor(r.Context(), run, info.Role))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": page.NextCursor,
	})
}

type stepDTO struct {
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	AttemptCount  int64   `json:"attempt_count"`
	FailureCount  int64   `json:"failure_count"`
	ErrorClass    string  `json:"error_class,omitempty"`
	ErrorSummary  string  `json:"error_summary,omitempty"`
	WaitingReason string  `json:"waiting_reason,omitempty"`
	NextRetryAt   *string `json:"next_retry_at,omitempty"`
	StartedAt     *string `json:"started_at,omitempty"`
	FinishedAt    *string `json:"finished_at,omitempty"`
}

func toStepDTOs(steps []storage.Step) []stepDTO {
	out := make([]stepDTO, 0, len(steps))
	for _, st := range steps {
		out = append(out, stepDTO{
			Name:          st.Name,
			Status:        st.Status,
			AttemptCount:  st.AttemptCount,
			FailureCount:  st.FailureCount,
			ErrorClass:    st.ErrorClass,
			ErrorSummary:  st.ErrorSummary,
			WaitingReason: st.WaitingReason,
			NextRetryAt:   formatMS(st.NextRetryAtMS),
			StartedAt:     formatMS(st.StartedAtMS),
			FinishedAt:    formatMS(st.FinishedAtMS),
		})
	}
	return out
}

func formatMS(ms *int64) *string {
	if ms == nil || *ms == 0 {
		return nil
	}
	s := time.UnixMilli(*ms).UTC().Format(time.RFC3339)
	return &s
}

type runDetailResponse struct {
	workflow.RunSummary
	Steps []stepDTO         `json:"steps"`
	Links map[string]string `json:"links"`
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.Store.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			notFoundResponse(w, r)
			return
		}
		writeError(w, r, errs.Wrap(errs.ClassInternal, "run.get", "could not load run", err))
		return
	}
	steps, err := s.Store.ListSteps(r.Context(), id)
	if err != nil {
		writeError(w, r, errs.Wrap(errs.ClassInternal, "run.get", "could not load steps", err))
		return
	}
	info, _ := authInfoFrom(r)
	resp := runDetailResponse{
		RunSummary: s.summarizeFor(r.Context(), run, info.Role),
		Steps:      toStepDTOs(steps),
		Links: map[string]string{
			"self":   "/v1/runs/" + id,
			"events": "/v1/runs/" + id + "/events",
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

type eventDTO struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	Step      string         `json:"step,omitempty"`
	Payload   map[string]any `json:"payload"`
	CreatedAt string         `json:"created_at"`
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetRun(r.Context(), id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			notFoundResponse(w, r)
			return
		}
		writeError(w, r, errs.Wrap(errs.ClassInternal, "run.events", "could not load run", err))
		return
	}
	var after int64
	if raw := r.URL.Query().Get("after_seq"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeError(w, r, errs.New(errs.ClassValidation, "run.events", "invalid after_seq"))
			return
		}
		after = v
	}
	events, err := s.Store.ListEvents(r.Context(), id, after, 200)
	if err != nil {
		writeError(w, r, errs.Wrap(errs.ClassInternal, "run.events", "could not load events", err))
		return
	}
	items := make([]eventDTO, 0, len(events))
	for _, ev := range events {
		items = append(items, eventDTO{
			Seq: ev.Seq, Type: ev.Type, Step: ev.Step, Payload: ev.Payload,
			CreatedAt: ev.CreatedAt().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.Store.RequestResume(r.Context(), id)
	if err != nil {
		s.writeRunActionError(w, r, "run.resume", err)
		return
	}
	if err := s.Store.Audit(r.Context(), "operator", "run.resume", id, nil); err != nil {
		s.Log.WarnContext(r.Context(), "audit write failed", "action", "run.resume", "run_id", id)
	}
	writeJSON(w, http.StatusAccepted, createRunResponse{
		RunSummary: s.Engine.Summarize(r.Context(), run),
		Links:      map[string]string{"self": "/v1/runs/" + id},
	})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.Store.RequestCancel(r.Context(), id)
	if err != nil {
		s.writeRunActionError(w, r, "run.cancel", err)
		return
	}
	if err := s.Store.Audit(r.Context(), "operator", "run.cancel", id, nil); err != nil {
		s.Log.WarnContext(r.Context(), "audit write failed", "action", "run.cancel", "run_id", id)
	}
	writeJSON(w, http.StatusAccepted, createRunResponse{
		RunSummary: s.Engine.Summarize(r.Context(), run),
		Links:      map[string]string{"self": "/v1/runs/" + id},
	})
}

func (s *Server) writeRunActionError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		notFoundResponse(w, r)
	case errors.Is(err, storage.ErrConflict):
		writeError(w, r, errs.New(errs.ClassConflict, op, errs.SummaryOf(err)))
	case errors.Is(err, storage.ErrIdempotencyConflict):
		writeError(w, r, errs.New(errs.ClassConflict, op, "idempotency conflict"))
	default:
		writeError(w, r, errs.Wrap(errs.ClassInternal, op, "run action failed", err))
	}
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.Store.ListAudit(r.Context(), 100)
	if err != nil {
		writeError(w, r, errs.Wrap(errs.ClassInternal, "audit.list", "could not load audit log", err))
		return
	}
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		items = append(items, map[string]any{
			"id":         e.ID,
			"actor":      e.Actor,
			"action":     e.Action,
			"run_id":     e.RunID,
			"detail":     e.Detail,
			"created_at": time.UnixMilli(e.CreatedAtMS).UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func knownRunState(status string) bool {
	for _, s := range state.AllRunStates {
		if string(s) == status {
			return true
		}
	}
	return false
}
