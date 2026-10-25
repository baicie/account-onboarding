package api

import (
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
	NextCursor  string
	TotalNeeds  int64
	StatusCount map[string]int64
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
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	page, err := s.Store.ListExternalOperations(r.Context(), storage.ExternalOperationFilter{
		Status: status, Provider: provider, Limit: 50, Cursor: cursor,
	})
	if err != nil {
		http.Error(w, "could not load external operations", http.StatusInternalServerError)
		return
	}
	data := opsExternalPage{
		pageData: pageData{Title: "Reconciliation", Version: Version, CSRFToken: info.CSRF,
			SignedIn: true, Actor: info.Actor},
		Status: status, Provider: provider, NextCursor: page.NextCursor,
		TotalNeeds: page.TotalNeedsReconciliation, StatusCount: page.StatusCounts,
	}
	for _, op := range page.Items {
		data.Items = append(data.Items, opsExternalRow{
			ID: op.ID, RunID: op.RunID, Provider: op.Provider, OperationType: op.OperationType,
			Status: op.Status, NeedsReview: op.Status == storage.ExternalOutcomeUnknown ||
				op.Status == storage.ExternalReconciliationReq || op.Status == storage.ExternalInFlight ||
				op.Status == storage.ExternalPrepared,
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
	if err != nil {
		http.Redirect(w, r, "/ui/ops/external?error="+url.QueryEscape(errs.SummaryOf(err)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/ops/external?notice=resolution+recorded", http.StatusSeeOther)
}
