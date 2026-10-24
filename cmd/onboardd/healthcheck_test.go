package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeReadinessRequiresReadyAndAuth(t *testing.T) {
	const token = "test-admin-token-value-123456"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("path = %s, want /readyz", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if err := probeReadiness(context.Background(), ts.URL+"/readyz", token); err != nil {
		t.Fatalf("ready probe rejected a valid token: %v", err)
	}
	if err := probeReadiness(context.Background(), ts.URL+"/readyz", "wrong-token"); err == nil {
		t.Fatal("ready probe accepted an invalid token")
	}
}

func TestProbeReadinessFailsWhenDegraded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // e.g. missing revocation table
	}))
	defer ts.Close()
	err := probeReadiness(context.Background(), ts.URL+"/readyz", "token")
	if err == nil {
		t.Fatal("ready probe accepted a 503")
	}
	if strings.Contains(err.Error(), "token") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestProbeReadinessHandlesUnreachableServer(t *testing.T) {
	if err := probeReadiness(context.Background(), "http://127.0.0.1:1/readyz", "token"); err == nil {
		t.Fatal("unreachable server reported ready")
	}
}
