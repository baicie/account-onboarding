package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/storage"
)

type opsExternalRow struct {
	ID                string
	RunID             string
	Provider          string
	OperationType     string
	Status            string
	NeedsReview       bool
	CreatedAtMS       int64
	Version           int64
	Resolution        string
	ResolutionReason  string
	ResolvedBy        string
	EvidenceReference string
}

type opsExternalPage struct {
	pageData
	Items       []opsExternalRow
	Status      string
	Provider    string
	RunID       string
	NextCursor  string
	TotalNeeds  int64
	StatusCount map[string]int64
	HasNext     bool
}

func parseInt64Form(r *http.Request, key string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(key)), 10, 64)
	if err != nil || v <= 0 {
		return 0, errs.New(errs.ClassValidation, "ui.ops", "invalid "+key)
	}
	return v, nil
}

func (s *Server) handleOpsExternalPage(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if msg := validateExternalFilters(status, provider, runID, cursor); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	page, err := s.Store.ListExternalOperations(r.Context(), storage.ExternalOperationFilter{
		Status: status, Provider: provider, RunID: runID, Limit: 50, Cursor: cursor,
	})
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			http.Error(w, "invalid pagination cursor", http.StatusBadRequest)
			return
		}
		http.Error(w, "could not load external operations", http.StatusInternalServerError)
		return
	}
	data := opsExternalPage{
		pageData: pageData{Title: s.t("page.reconciliation"), Version: Version, CSRFToken: info.CSRF,
			SignedIn: true, Actor: info.Actor, Error: r.URL.Query().Get("error"),
			Notice: r.URL.Query().Get("notice")},
		Status: status, Provider: provider, RunID: runID, NextCursor: page.NextCursor,
		TotalNeeds: page.TotalNeedsReconciliation, StatusCount: page.StatusCounts,
		HasNext: page.NextCursor != "",
	}
	for _, op := range page.Items {
		data.Items = append(data.Items, opsExternalRow{
			ID: op.ID, RunID: op.RunID, Provider: op.Provider, OperationType: op.OperationType,
			Status: op.Status, NeedsReview: op.Status == storage.ExternalOutcomeUnknown ||
				op.Status == storage.ExternalReconciliationReq,
			CreatedAtMS: op.CreatedAtMS, Version: op.Version,
			Resolution: op.Resolution, ResolutionReason: op.ResolutionReason,
			ResolvedBy: op.ResolvedBy, EvidenceReference: op.EvidenceReference,
		})
	}
	if err := s.pages["ops_external"].render(w, http.StatusOK, data); err != nil {
		s.Log.Error("render reconciliation page", "error", err)
	}
}

func (s *Server) handleOpsExternalResolveUI(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	id := r.PathValue("id")
	version, err := parseInt64Form(r, "expected_version")
	if err != nil {
		http.Redirect(w, r, "/ui/ops/external?error="+url.QueryEscape("invalid expected version"), http.StatusSeeOther)
		return
	}
	_, err = s.Store.ResolveExternalOperation(r.Context(), id, storage.ExternalResolution{
		Resolution:        strings.ToUpper(strings.TrimSpace(r.FormValue("resolution"))),
		Reason:            r.FormValue("reason"),
		EvidenceReference: r.FormValue("evidence_reference"),
		ExpectedVersion:   version,
		Actor:             info.Actor,
		RequestedAtMS:     time.Now().UTC().UnixMilli(),
	})
	back := url.Values{}
	if v := strings.TrimSpace(r.FormValue("return_status")); v != "" {
		back.Set("status", v)
	}
	if v := strings.TrimSpace(r.FormValue("return_provider")); v != "" {
		back.Set("provider", v)
	}
	if v := strings.TrimSpace(r.FormValue("return_cursor")); v != "" {
		back.Set("cursor", v)
	}
	if v := strings.TrimSpace(r.FormValue("return_run_id")); v != "" {
		back.Set("run_id", v)
	}
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			back.Set("error", "conflict: operation version changed or is already resolved; reload before retrying")
		} else {
			back.Set("error", errs.SummaryOf(err))
		}
		http.Redirect(w, r, "/ui/ops/external?"+back.Encode(), http.StatusSeeOther)
		return
	}
	back.Set("notice", "review recorded")
	http.Redirect(w, r, "/ui/ops/external?"+back.Encode(), http.StatusSeeOther)
}

func validateExternalFilters(status, provider, runID, cursor string) string {
	if len(status) > 64 || len(provider) > 128 || len(runID) > 128 || len(cursor) > 512 {
		return "filter or cursor is too long"
	}
	if status != "" {
		for _, st := range []string{storage.ExternalPrepared, storage.ExternalInFlight, storage.ExternalConfirmed,
			storage.ExternalOutcomeUnknown, storage.ExternalReconciliationReq, storage.ExternalFailedDefinite, storage.ExternalResolved} {
			if status == st {
				return ""
			}
		}
		return "unknown external operation status filter"
	}
	return ""
}

type auditRow struct {
	ID         int64
	Actor      string
	Action     string
	CreatedAt  int64
	Resolution string
	Reason     string
	Evidence   string
	FromStatus string
	ToStatus   string
}

type opsExternalDetailPage struct {
	pageData
	Op             storage.ExternalOperation
	Audits         []auditRow
	ReturnStatus   string
	ReturnProvider string
	ReturnCursor   string
	ReturnRunID    string
}

func (s *Server) handleOpsExternalDetail(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	id := r.PathValue("id")
	op, err := s.Store.GetExternalOperationByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	entries, err := s.Store.ListExternalOperationAudit(r.Context(), id, 100)
	if err != nil {
		http.Error(w, "could not load audit history", http.StatusInternalServerError)
		return
	}
	data := opsExternalDetailPage{
		pageData: pageData{Title: s.t("page.operation_prefix") + op.OperationKey, Version: Version, CSRFToken: info.CSRF,
			SignedIn: true, Actor: info.Actor, Error: r.URL.Query().Get("error"),
			Notice: r.URL.Query().Get("notice")},
		Op: op, ReturnStatus: r.URL.Query().Get("status"),
		ReturnProvider: r.URL.Query().Get("provider"), ReturnCursor: r.URL.Query().Get("cursor"),
		ReturnRunID: r.URL.Query().Get("run_id"),
	}
	for _, e := range entries {
		row := auditRow{ID: e.ID, Actor: e.Actor, Action: e.Action, CreatedAt: e.CreatedAtMS}
		row.Resolution, _ = e.Detail["resolution"].(string)
		row.Reason, _ = e.Detail["reason"].(string)
		row.Evidence, _ = e.Detail["evidence_reference"].(string)
		row.FromStatus, _ = e.Detail["from_status"].(string)
		row.ToStatus, _ = e.Detail["to_status"].(string)
		data.Audits = append(data.Audits, row)
	}
	if err := s.pages["ops_external_detail"].render(w, http.StatusOK, data); err != nil {
		s.Log.Error("render operation detail", "error", err)
	}
}
