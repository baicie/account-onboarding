package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
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
	op, err := s.Store.ResolveExternalOperation(r.Context(), id, storage.ExternalResolution{
		Resolution:        strings.ToUpper(strings.TrimSpace(req.Resolution)),
		Reason:            req.Reason,
		EvidenceReference: req.EvidenceReference,
		ExpectedVersion:   req.ExpectedVersion,
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
