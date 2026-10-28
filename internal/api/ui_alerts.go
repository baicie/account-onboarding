package api

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/storage"
)

type alertRow struct {
	ID, Fingerprint, Rule, Severity, Message, Status, DeliveryID string
	Attempts                                                     int
	NextAttemptMS, LeaseExpiresMS, CreatedAtMS, UpdatedAtMS      int64
	LastError                                                    string
	DeliveredAtMS                                                *int64
	LeaseHeld                                                    bool
}

func toAlertRow(it storage.AlertOutboxItem) alertRow {
	return alertRow{ID: it.ID, Fingerprint: it.Fingerprint, Rule: it.Rule, Severity: it.Severity,
		Message: it.Message, Status: it.Status, DeliveryID: it.DeliveryID, Attempts: it.Attempts,
		NextAttemptMS: it.NextAttemptMS, LeaseExpiresMS: it.LeaseExpiresMS, LastError: it.LastError,
		CreatedAtMS: it.CreatedAtMS, UpdatedAtMS: it.UpdatedAtMS, DeliveredAtMS: it.DeliveredAtMS,
		LeaseHeld: it.Status == storage.AlertOutboxInFlight && it.LeaseExpiresMS > time.Now().UTC().UnixMilli()}
}

func (s *Server) alertRedirect(w http.ResponseWriter, r *http.Request, vals url.Values) {
	http.Redirect(w, r, "/ui/alerts/outbox?"+vals.Encode(), http.StatusSeeOther)
}

// handleAlertReconcileUI evaluates current alerts and updates the durable
// outbox. It never sends a notification; Dispatch is a separate action.

// handleAlertDispatchUI sends only currently due outbox rows. It is fail-closed
// when no external transport is configured.

// handleAlertResolveUI closes active outbox rows for one rule. It never sends.

func errsSummary(err error) string {
	if err == nil {
		return ""
	}
	if len(err.Error()) > 300 {
		return err.Error()[:300]
	}
	return err.Error()
}

// handleAlertRearmUI is intentionally separate from Resolve: it turns
// DELIVERED/DEAD_LETTER rows into RESOLVED so a later Reconcile may enqueue a
// fresh active row. Delivery history is preserved and never rewritten.

// handleAlertOutboxDetail mirrors the server-rendered alert detail page for the React
// console: the same id validation, the same store reads, JSON instead of HTML.
func (s *Server) handleAlertOutboxDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) > 128 || !strings.HasPrefix(id, "al-") {
		writeError(w, r, errs.New(errs.ClassValidation, "alert.detail", "invalid alert id"))
		return
	}
	it, err := s.Store.GetAlertOutbox(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, errs.New(errs.ClassNotFound, "alert.detail", "alert not found"))
			return
		}
		writeError(w, r, errs.Wrap(errs.ClassInternal, "alert.detail", "could not load alert outbox", err))
		return
	}
	deliveries, err := s.Store.ListAlertDeliveries(r.Context(), id)
	if err != nil {
		writeError(w, r, errs.Wrap(errs.ClassInternal, "alert.detail", "could not load delivery history", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": toAlertRow(it), "deliveries": deliveries})
}
