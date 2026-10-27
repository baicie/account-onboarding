package cloudflare

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestParsedMailsContractFixtures keeps the Go normalizer and the shell
// preflight on the same provider-compatible envelope set. It does not prove the
// live Cloudflare Worker schema; that still requires an authorized live test.
func TestParsedMailsContractFixtures(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "contracts", "cloudflare", "parsed_mails_envelopes.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name string          `json:"name"`
			Body json.RawMessage `json:"body"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 10 {
		t.Fatalf("fixture count = %d, want 10", len(fixture.Cases))
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			items, err := parseMailList(tc.Body, opListMessages)
			if err != nil {
				t.Fatalf("parseMailList: %v", err)
			}
			if tc.Name == "null_list" || tc.Name == "empty_list" {
				if len(items) != 0 {
					t.Fatalf("items = %d, want 0", len(items))
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %d, want 1", len(items))
			}
			msg, ok, err := normalizeListItem(items[0])
			if err != nil || !ok {
				t.Fatalf("normalizeListItem: ok=%v err=%v", ok, err)
			}
			if msg.ID != "m1" || msg.Sender != "sender@example.test" {
				t.Fatalf("normalized message = %+v", msg)
			}
		})
	}
}
