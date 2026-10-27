package cloudflare

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParsedMailsInvalidContractFixtures(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "contracts", "cloudflare", "parsed_mails_contract.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Invalid []struct {
			Name string `json:"name"`
			Body string `json:"body"`
		} `json:"invalid_cases"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Invalid) == 0 {
		t.Fatal("no invalid contract cases")
	}
	for _, tc := range manifest.Invalid {
		t.Run(tc.Name, func(t *testing.T) {
			if _, err := parseMailList([]byte(tc.Body), opListMessages); err == nil {
				t.Fatalf("invalid body accepted: %q", tc.Body)
			}
		})
	}
}
