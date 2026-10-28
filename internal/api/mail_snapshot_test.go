package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/config"
	"github.com/baicie/account-onboarding/internal/provider"
)

// With snapshots enabled the console can show the mail a run really received (sender, subject,
// link) without the body ever being stored. With snapshots disabled the endpoint reports that.
func TestRunMailsSnapshot(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config, _ *provider.Registry) { cfg.MailSnapshot = true })
	status, body := e.request(t, http.MethodPost, "/v1/runs", sandboxRequest(), adminToken, map[string]string{
		"Idempotency-Key": "r22-mail-snapshot",
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

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, raw := e.request(t, http.MethodGet, "/v1/runs/"+created.ID+"/mails", nil, adminToken, nil)
		if st != http.StatusOK {
			t.Fatalf("mails = %d %s", st, raw)
		}
		if strings.Contains(string(raw), `"sender"`) {
			if strings.Contains(string(raw), `"enabled":false`) {
				t.Fatalf("snapshots reported disabled: %s", raw)
			}
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no mail snapshot recorded within the deadline for run %s", created.ID)
}
