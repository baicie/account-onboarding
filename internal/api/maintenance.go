package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maintenanceDrainTimeout bounds how long entering maintenance waits for
// in-flight writes. A write that outlives it is reported, not killed.
const maintenanceDrainTimeout = 30 * time.Second

// maintenanceInfo is the observable maintenance state.
type maintenanceInfo struct {
	Active         bool       `json:"active"`
	Reason         string     `json:"reason,omitempty"`
	Since          *time.Time `json:"since,omitempty"`
	InFlight       int64      `json:"in_flight"`
	WritesInFlight int64      `json:"writes_in_flight"`
	WorkersPaused  bool       `json:"workers_paused"`
	Drained        bool       `json:"drained"`
}

// maintenanceState is the in-process maintenance flag. The window is process
// local on purpose: it ends when the operator restarts the service, so a crash
// in the middle of a restore can never come back "still in maintenance" with a
// half-replaced database.
type maintenanceState struct {
	mu     sync.RWMutex
	active bool
	reason string
	since  time.Time
}

// enter switches maintenance on and reports whether this call changed anything.
func (m *maintenanceState) enter(reason string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active {
		return false
	}
	m.active = true
	m.reason = reason
	m.since = now
	return true
}

// exit switches maintenance off and reports whether this call changed anything.
func (m *maintenanceState) exit() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return false
	}
	m.active = false
	m.reason = ""
	m.since = time.Time{}
	return true
}

func (m *maintenanceState) status() (bool, string, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active, m.reason, m.since
}

func (s *Server) maintenanceInfo() maintenanceInfo {
	active, reason, since := s.maint.status()
	info := maintenanceInfo{
		Active:         active,
		Reason:         reason,
		InFlight:       s.inFlight.Load(),
		WritesInFlight: s.writesInFlight.Load(),
	}
	if active {
		info.Since = &since
	}
	if s.Engine != nil {
		info.WorkersPaused = s.Engine.Paused()
	}
	info.Drained = info.WritesInFlight == 0
	return info
}

// maintenanceGate rejects new mutations while a maintenance window is open and
// tracks in-flight work so entering maintenance can drain instead of cutting a
// request in half.
func (s *Server) maintenanceGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/healthz" || strings.HasPrefix(path, "/v1/maintenance") {
			next.ServeHTTP(w, r)
			return
		}
		mutating := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if mutating {
			s.writesInFlight.Add(1)
			defer s.writesInFlight.Add(-1)
		}
		s.inFlight.Add(1)
		defer s.inFlight.Add(-1)
		if mutating {
			if active, reason, _ := s.maint.status(); active {
				w.Header().Set("Retry-After", "30")
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"error": map[string]any{
						"class":      "MAINTENANCE",
						"message":    "the service is in a maintenance window; writes are rejected until it exits",
						"reason":     reason,
						"request_id": requestID(r),
					},
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleMaintenanceState reports the current window.
func (s *Server) handleMaintenanceState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.maintenanceInfo())
}

// handleMaintenanceEnter opens a maintenance window: pause the workflow workers,
// reject new mutations, then wait (bounded) for in-flight writes to finish.
func (s *Server) handleMaintenanceEnter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) > 200 {
		reason = reason[:200]
	}
	if s.maint.enter(reason, time.Now().UTC()) {
		if s.Engine != nil {
			s.Engine.Pause()
		}
		s.auditMaintenance(r, "maintenance.enter", reason)
	}
	deadline := time.Now().Add(maintenanceDrainTimeout)
	waited := time.Duration(0)
	for s.writesInFlight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		waited += 20 * time.Millisecond
	}
	info := s.maintenanceInfo()
	writeJSON(w, http.StatusOK, map[string]any{
		"active":            info.Active,
		"drained":           info.Drained,
		"in_flight":         info.InFlight,
		"writes_in_flight":  info.WritesInFlight,
		"workers_paused":    info.WorkersPaused,
		"waited_ms":         waited.Milliseconds(),
		"drain_timeout_sec": int(maintenanceDrainTimeout.Seconds()),
		"next_step":         "stop the service, apply the restore, then start it again",
	})
}

// handleMaintenanceExit closes the window and resumes the workers.
func (s *Server) handleMaintenanceExit(w http.ResponseWriter, r *http.Request) {
	changed := s.maint.exit()
	if s.Engine != nil {
		s.Engine.Resume()
	}
	ready := true
	schemaVersion := 0
	wal := ""
	if s.Store != nil {
		if health, err := s.Store.Health(r.Context()); err == nil {
			schemaVersion = health.SchemaVersion
			wal = health.WALMode
			ready = health.Writable && health.SchemaVersion >= 6
		} else {
			ready = false
		}
	}
	if changed {
		s.auditMaintenance(r, "maintenance.exit", "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":         false,
		"resumed":        s.Engine != nil && !s.Engine.Paused(),
		"changed":        changed,
		"ready":          ready,
		"schema_version": schemaVersion,
		"wal":            wal,
	})
}

func (s *Server) auditMaintenance(r *http.Request, action, reason string) {
	if s.Store == nil {
		return
	}
	actor := "system"
	if info, ok := authInfoFrom(r); ok && info.Actor != "" {
		actor = info.Actor
	}
	detail := map[string]any{}
	if reason != "" {
		detail["reason"] = reason
	}
	_ = s.Store.Audit(r.Context(), actor, action, "", detail)
}
