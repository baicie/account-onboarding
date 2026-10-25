package api

import (
	"net/http"
)

// handleListExternalOperations exposes the audit/reconciliation view of remote
// side effects. Admin-only: it contains run/step identifiers and status but no
// tokens, JWTs, addresses or request bodies.
func (s *Server) handleListExternalOperations(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListExternalOperations(r.Context(), 200)
	if err != nil {
		writeError(w, r, err)
		return
	}
	counts := map[string]int{"total": len(items), "needs_reconciliation": 0}
	for _, item := range items {
		counts[item.Status]++
		if item.NeedsReview {
			counts["needs_reconciliation"]++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":               items,
		"counts":              counts,
		"reconciliation_note": "PREPARED/OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED need operator review; remote create success is not exactly-once with the local transaction",
	})
}
