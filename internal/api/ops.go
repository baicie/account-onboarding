package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/alerts"
	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

func errsValidation(msg string) error {
	return errs.New(errs.ClassValidation, "ops.external", msg)
}

// handleListExternalOperations exposes a paginated, filtered reconciliation
// view. Counts are global (never derived from the current page), and only
// non-secret operation metadata is returned.
func (s *Server) handleListExternalOperations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeError(w, r, errsValidation("limit must be 1..200"))
			return
		}
		limit = n
	}
	page, err := s.Store.ListExternalOperations(r.Context(), storage.ExternalOperationFilter{
		Status:   q.Get("status"),
		Provider: q.Get("provider"),
		RunID:    q.Get("run_id"),
		Limit:    limit,
		Cursor:   q.Get("cursor"),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

type resolveExternalOperationRequest struct {
	Resolution        string `json:"resolution"`
	Reason            string `json:"reason"`
	ExpectedVersion   int64  `json:"expected_version"`
	EvidenceReference string `json:"evidence_reference"`
}

// handleResolveExternalOperation records an audited manual decision. It never
// triggers automatic retries or deletes remote resources.
func (s *Server) handleResolveExternalOperation(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, r, errsValidation("operation id is required"))
		return
	}
	var req resolveExternalOperationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, r, errsValidation("resolution reason is required"))
		return
	}
	if len(req.Reason) > 2000 || len(req.EvidenceReference) > 512 ||
		strings.ContainsAny(req.Reason, "\x00\r\n") || strings.ContainsAny(req.EvidenceReference, "\x00\r\n") {
		writeError(w, r, errsValidation("resolution reason/evidence invalid or too long"))
		return
	}
	info, _ := authInfoFrom(r)
	op, err := s.Store.ResolveExternalOperation(r.Context(), id, storage.ExternalResolution{
		Resolution:        strings.ToUpper(strings.TrimSpace(req.Resolution)),
		Reason:            req.Reason,
		EvidenceReference: req.EvidenceReference,
		ExpectedVersion:   req.ExpectedVersion,
		Actor:             info.Actor,
		RequestedAtMS:     time.Now().UTC().UnixMilli(),
	})
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, r, errs.New(errs.ClassNotFound, "ops.external.resolve", "operation not found"))
			return
		}
		if errors.Is(err, storage.ErrConflict) {
			writeError(w, r, errs.New(errs.ClassConflict, "ops.external.resolve", "version conflict or operation is not resolvable"))
			return
		}
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

// handleExternalOperationAudit returns the review/resolution history for one
// operation. Admin-only; evidence/reason are already redacted on write.
func (s *Server) handleExternalOperationAudit(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if _, err := s.Store.GetExternalOperationByID(r.Context(), id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, r, errs.New(errs.ClassNotFound, "ops.external.audit", "operation not found"))
			return
		}
		writeError(w, r, err)
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	entries, err := s.Store.ListExternalOperationAudit(r.Context(), id, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": entries})
}

// handleOpsMetrics exposes bounded aggregate diagnostics. It is admin-only and
// deliberately returns fixed status/error-class labels only — never emails,
// tokens, JWT/OAuth state, addresses or passwords.
func (s *Server) handleOpsMetrics(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.OperationalMetrics(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"metrics":         m,
		"version":         Version,
		"commit":          Commit,
		"generated_at_ms": time.Now().UTC().UnixMilli(),
	})
}

func (s *Server) handleOpsAlertResolve(w http.ResponseWriter, r *http.Request) {
	rule := strings.TrimSpace(r.FormValue("rule"))
	if rule == "" {
		var req struct {
			Rule string `json:"rule"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			rule = strings.TrimSpace(req.Rule)
		}
	}
	if rule == "" {
		writeError(w, r, errsValidation("rule is required"))
		return
	}
	recovery, ok := s.AlertQueue.Resolve(rule)
	if !ok {
		writeError(w, r, errs.New(errs.ClassNotFound, "ops.alerts", "alert rule not active"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolved": true, "recovery": recovery, "status": s.AlertQueue.Status()})
}

func (s *Server) handleOpsAlertDrain(w http.ResponseWriter, r *http.Request) {
	if s.AlertQueue == nil || s.AlertNotifier == nil {
		writeError(w, r, errs.New(errs.ClassInternal, "ops.alerts", "alert execution not configured"))
		return
	}
	sent, err := s.AlertQueue.Drain(r.Context(), s.AlertNotifier)
	if err != nil {
		switch {
		case errors.Is(err, alerts.ErrRateLimited), errors.Is(err, alerts.ErrDrainInProgress):
			writeError(w, r, errs.New(errs.ClassConflict, "ops.alerts", err.Error()))
		default:
			writeError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent, "status": s.AlertQueue.Status()})
}

// handleOpsAlertReconcile is the explicit state-changing control path. GET
// /v1/ops/alerts is intentionally side-effect free.
func (s *Server) handleOpsAlertReconcile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := s.Store.OperationalMetrics(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	health, err := s.Store.Health(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	current := alerts.EvaluateAlerts(alerts.AlertInput{
		Metrics: m, Ready: health.Writable && health.SchemaVersion >= 6 &&
			health.SessionRevocationsReady && health.ExternalOperationsReady,
		ExpectedSchema: 6, NowMS: time.Now().UTC().UnixMilli(),
		BackupMaxAgeMS:          (7 * 24 * time.Hour).Milliseconds(),
		ReconciliationThreshold: 10, ProviderErrorThreshold: 20,
	})
	s.AlertQueue.Reconcile(current)
	now := time.Now().UTC().UnixMilli()
	items := make([]storage.AlertOutboxInput, 0, len(current))
	rules := make([]string, 0, len(current))
	for _, a := range current {
		items = append(items, storage.AlertOutboxInput{Fingerprint: alerts.Fingerprint(a),
			Rule: a.Rule, Severity: a.Severity, Message: a.Message})
		rules = append(rules, a.Rule)
	}
	enqueued, err := s.Store.EnqueueAlertOutbox(ctx, items, now)
	if err != nil {
		writeError(w, r, err)
		return
	}
	resolved, err := s.Store.ResolveAlertOutboxMissing(ctx, rules, now)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evaluated": len(current), "outbox_enqueued": enqueued,
		"outbox_resolved": resolved, "status": s.AlertQueue.Status()})
}

func (s *Server) handleAlertOutbox(w http.ResponseWriter, r *http.Request) {
	counts, err := s.Store.AlertOutboxStatusCounts(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	items, err := s.Store.ListAlertOutbox(r.Context(), 100)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts, "items": items,
		"durable": true, "external_transport_configured": s.AlertOutboxNotifier != nil})
}

func (s *Server) handleAlertOutboxDispatch(w http.ResponseWriter, r *http.Request) {
	if s.AlertOutboxNotifier == nil {
		writeError(w, r, errs.New(errs.ClassInternal, "ops.alerts", "external alert transport is not configured"))
		return
	}
	now := time.Now().UTC().UnixMilli()
	items, deliveries, err := s.Store.ClaimAlertOutbox(r.Context(), "api:outbox:"+security.NewID(), 10, time.Minute, now)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if len(items) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"claimed": 0, "sent": 0})
		return
	}
	batch := make([]alerts.Alert, 0, len(items))
	for _, it := range items {
		batch = append(batch, alerts.Alert{Rule: it.Rule, Severity: it.Severity, Message: it.Message})
	}
	deliverErr := alerts.DeliverBatch(r.Context(), s.AlertOutboxNotifier, batch)
	sent := 0
	for _, d := range deliveries {
		ok := deliverErr == nil
		if ok {
			sent++
		}
		if err := s.Store.CompleteAlertOutbox(r.Context(), d.ID, d.LeaseOwner, ok, now, 60000, 0, errString(deliverErr), 10); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if deliverErr != nil {
		writeError(w, r, errs.New(errs.ClassUpstreamTransient, "ops.alerts", "alert delivery failed; retry scheduled"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"claimed": len(items), "sent": sent})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) handleOpsAlertQueue(w http.ResponseWriter, r *http.Request) {
	if s.AlertQueue == nil {
		writeError(w, r, errs.New(errs.ClassInternal, "ops.alerts", "alert queue unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, s.AlertQueue.Status())
}

func (s *Server) handleOpsAlerts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := s.Store.OperationalMetrics(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	health, err := s.Store.Health(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	alerts := alerts.EvaluateAlerts(alerts.AlertInput{
		Metrics: m, Ready: health.Writable && health.SchemaVersion >= 6 &&
			health.SessionRevocationsReady && health.ExternalOperationsReady,
		ExpectedSchema: 6, NowMS: time.Now().UTC().UnixMilli(), BackupMaxAgeMS: (7 * 24 * time.Hour).Milliseconds(),
		ReconciliationThreshold: 10, ProviderErrorThreshold: 20,
	})
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "count": len(alerts), "queue": s.AlertQueue.Status()})
}
