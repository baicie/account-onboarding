package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The run detail must show what the run created (for example the mailbox address) while never
// serialising a secret reference: the JWT that reads the mailbox stays in the secret store.
func TestRunDetailExposesResourcesWithoutSecrets(t *testing.T) {
	e := newEnv(t, nil)
	status, body := e.request(t, http.MethodPost, "/v1/runs", sandboxRequest(), adminToken, map[string]string{
		"Idempotency-Key": "r22-resource-exposure",
	})
	if status != http.StatusAccepted && status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create run = %d %s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var detail []byte
	for time.Now().Before(deadline) {
		st, d := e.request(t, http.MethodGet, "/v1/runs/"+created.ID, nil, adminToken, nil)
		if st != http.StatusOK {
			t.Fatalf("run detail = %d %s", st, d)
		}
		detail = d
		if strings.Contains(string(d), "display_alias") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !strings.Contains(string(detail), "display_alias") {
		t.Fatalf("run detail never exposed a resource: %s", detail)
	}
	if strings.Contains(string(detail), "secret://") {
		t.Fatalf("run detail leaked a secret reference: %s", detail)
	}
}
