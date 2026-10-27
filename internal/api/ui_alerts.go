package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/alerts"
	"github.com/baicie/account-onboarding/internal/security"
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

type alertsPage struct {
	pageData
	Counts       storage.AlertOutboxStatus
	Items        []alertRow
	Status       string
	Offset       int
	NextOffset   int
	PrevOffset   int
	HasNext      bool
	HasPrev      bool
	Transport    bool
	ActiveAlerts int
}

type alertDetailPage struct {
	pageData
	Item       alertRow
	Deliveries []storage.AlertDelivery
	Transport  bool
}

func toAlertRow(it storage.AlertOutboxItem) alertRow {
	return alertRow{ID: it.ID, Fingerprint: it.Fingerprint, Rule: it.Rule, Severity: it.Severity,
		Message: it.Message, Status: it.Status, DeliveryID: it.DeliveryID, Attempts: it.Attempts,
		NextAttemptMS: it.NextAttemptMS, LeaseExpiresMS: it.LeaseExpiresMS, LastError: it.LastError,
		CreatedAtMS: it.CreatedAtMS, UpdatedAtMS: it.UpdatedAtMS, DeliveredAtMS: it.DeliveredAtMS,
		LeaseHeld: it.Status == storage.AlertOutboxInFlight && it.LeaseExpiresMS > time.Now().UTC().UnixMilli()}
}

func (s *Server) handleAlertsPage(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	offset := 0
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100000 {
			offset = n
		}
	}
	const limit = 50
	counts, err := s.Store.AlertOutboxStatusCounts(r.Context())
	if err != nil {
		s.Log.Error("alert counts", "error", err)
		http.Error(w, "could not load alert outbox", http.StatusInternalServerError)
		return
	}
	items, err := s.Store.ListAlertOutboxFilter(r.Context(), status, limit, offset)
	if err != nil {
		s.Log.Error("list alert outbox", "error", err)
		http.Error(w, "could not load alert outbox", http.StatusInternalServerError)
		return
	}
	data := alertsPage{
		pageData: pageData{Title: "Alerts", Version: s.Version, CSRFToken: info.CSRF,
			SignedIn: true, Actor: info.Actor, Error: r.URL.Query().Get("error"),
			Notice: r.URL.Query().Get("notice")},
		Counts: counts, Status: status, Offset: offset, Transport: s.AlertOutboxNotifier != nil,
		ActiveAlerts: int(counts.Pending + counts.InFlight + counts.RetryPending),
	}
	for _, it := range items {
		data.Items = append(data.Items, toAlertRow(it))
	}
	if len(items) == limit {
		data.HasNext, data.NextOffset = true, offset+limit
	}
	if offset > 0 {
		data.HasPrev, data.PrevOffset = true, offset-limit
		if data.PrevOffset < 0 {
			data.PrevOffset = 0
		}
	}
	if err := s.pages["alerts"].render(w, http.StatusOK, data); err != nil {
		s.Log.Error("render alerts page", "error", err)
	}
}

func (s *Server) handleAlertDetail(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	id := r.PathValue("id")
	it, err := s.Store.GetAlertOutbox(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	deliveries, err := s.Store.ListAlertDeliveries(r.Context(), id)
	if err != nil {
		http.Error(w, "could not load delivery history", http.StatusInternalServerError)
		return
	}
	data := alertDetailPage{pageData: pageData{Title: "Alert " + shortID(id), Version: s.Version,
		CSRFToken: info.CSRF, SignedIn: true, Actor: info.Actor,
		Error: r.URL.Query().Get("error"), Notice: r.URL.Query().Get("notice")},
		Item: toAlertRow(it), Deliveries: deliveries, Transport: s.AlertOutboxNotifier != nil}
	if err := s.pages["alerts_detail"].render(w, http.StatusOK, data); err != nil {
		s.Log.Error("render alert detail", "error", err)
	}
}

func (s *Server) alertRedirect(w http.ResponseWriter, r *http.Request, vals url.Values) {
	http.Redirect(w, r, "/ui/alerts/outbox?"+vals.Encode(), http.StatusSeeOther)
}

// handleAlertReconcileUI evaluates current alerts and updates the durable
// outbox. It never sends a notification; Dispatch is a separate action.
func (s *Server) handleAlertReconcileUI(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	ctx := r.Context()
	m, err := s.Store.OperationalMetrics(ctx)
	if err != nil {
		s.alertRedirect(w, r, url.Values{"error": {errsSummary(err)}})
		return
	}
	health, err := s.Store.Health(ctx)
	if err != nil {
		s.alertRedirect(w, r, url.Values{"error": {errsSummary(err)}})
		return
	}
	current := alerts.EvaluateAlerts(alerts.AlertInput{Metrics: m,
		Ready:          health.Writable && health.SchemaVersion >= 6 && health.SessionRevocationsReady && health.ExternalOperationsReady,
		ExpectedSchema: 6, NowMS: time.Now().UTC().UnixMilli(), BackupMaxAgeMS: (7 * 24 * time.Hour).Milliseconds(),
		ReconciliationThreshold: 10, ProviderErrorThreshold: 20})
	if s.AlertQueue != nil {
		s.AlertQueue.Reconcile(current)
	}
	now := time.Now().UTC().UnixMilli()
	items := make([]storage.AlertOutboxInput, 0, len(current))
	rules := make([]string, 0, len(current))
	for _, a := range current {
		items = append(items, storage.AlertOutboxInput{Fingerprint: alerts.Fingerprint(a), Rule: a.Rule,
			Severity: a.Severity, Message: a.Message})
		rules = append(rules, a.Rule)
	}
	enqueued, err := s.Store.EnqueueAlertOutbox(ctx, items, now)
	if err != nil {
		s.alertRedirect(w, r, url.Values{"error": {errsSummary(err)}})
		return
	}
	resolved, err := s.Store.ResolveAlertOutboxMissing(ctx, rules, now)
	if err != nil {
		s.alertRedirect(w, r, url.Values{"error": {errsSummary(err)}})
		return
	}
	_ = s.Store.Audit(ctx, info.Actor, "alert.reconcile", "", map[string]any{
		"evaluated": len(current), "enqueued": enqueued, "resolved_missing": resolved})
	s.alertRedirect(w, r, url.Values{"notice": {"reconciled: evaluated " + strconv.Itoa(len(current)) +
		", enqueued " + strconv.Itoa(enqueued) + ", resolved " + strconv.FormatInt(resolved, 10)}})
}

// handleAlertDispatchUI sends only currently due outbox rows. It is fail-closed
// when no external transport is configured.
func (s *Server) handleAlertDispatchUI(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	if s.AlertOutboxNotifier == nil {
		s.alertRedirect(w, r, url.Values{"error": {"dispatch rejected: no external alert transport is configured"}})
		return
	}
	now := time.Now().UTC().UnixMilli()
	items, deliveries, err := s.Store.ClaimAlertOutbox(r.Context(), "ui:outbox:"+security.NewID(), 10, time.Minute, now)
	if err != nil {
		s.alertRedirect(w, r, url.Values{"error": {errsSummary(err)}})
		return
	}
	byDelivery := make(map[string]storage.AlertOutboxItem, len(items))
	for _, it := range items {
		byDelivery[it.DeliveryID] = it
	}
	sent := 0
	for _, d := range deliveries {
		it, ok := byDelivery[d.ID]
		if !ok {
			continue
		}
		one := []alerts.Alert{{Rule: it.Rule, Severity: it.Severity, Message: it.Message}}
		var derr error
		if dn, ok := s.AlertOutboxNotifier.(alerts.DeliveryNotifier); ok {
			derr = dn.NotifyDelivery(r.Context(), alerts.DeliveryEnvelope{DeliveryID: d.ID, AlertID: it.ID, Alerts: one})
		} else {
			derr = alerts.DeliverBatch(r.Context(), s.AlertOutboxNotifier, one)
		}
		if derr == nil {
			sent++
		}
		maxAttempts := 10
		if alerts.IsPermanentDeliveryError(derr) {
			maxAttempts = 1
		}
		if err := s.Store.CompleteAlertOutbox(r.Context(), d.ID, d.LeaseOwner, derr == nil,
			time.Now().UTC().UnixMilli(), 60000, 0, errString(derr), maxAttempts); err != nil {
			s.alertRedirect(w, r, url.Values{"error": {errsSummary(err)}})
			return
		}
	}
	_ = s.Store.Audit(r.Context(), info.Actor, "alert.dispatch", "", map[string]any{"claimed": len(items), "sent": sent})
	notice := "dispatch complete: " + strconv.Itoa(sent) + "/" + strconv.Itoa(len(items)) + " delivered"
	s.alertRedirect(w, r, url.Values{"notice": {notice}})
}

// handleAlertResolveUI closes active outbox rows for one rule. It never sends.
func (s *Server) handleAlertResolveUI(w http.ResponseWriter, r *http.Request) {
	info, _ := authInfoFrom(r)
	rule := strings.TrimSpace(r.FormValue("rule"))
	if rule == "" {
		s.alertRedirect(w, r, url.Values{"error": {"resolve requires a rule"}})
		return
	}
	n, err := s.Store.ResolveAlertOutbox(r.Context(), rule, time.Now().UTC().UnixMilli())
	if err != nil {
		s.alertRedirect(w, r, url.Values{"error": {errsSummary(err)}})
		return
	}
	if s.AlertQueue != nil {
		s.AlertQueue.Resolve(rule)
	}
	_ = s.Store.Audit(r.Context(), info.Actor, "alert.resolve", "", map[string]any{"rule": rule, "resolved": n})
	s.alertRedirect(w, r, url.Values{"notice": {"resolved " + strconv.FormatInt(n, 10) + " active outbox row(s)"}})
}

func errsSummary(err error) string {
	if err == nil {
		return ""
	}
	if len(err.Error()) > 300 {
		return err.Error()[:300]
	}
	return err.Error()
}
