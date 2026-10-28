package api_test

import (
	"io"
	"net/http"
	"regexp"
	"testing"
)

// The bundle is mounted at /ui/app, so index.html must reference its assets from that same
// prefix and those URLs must resolve. A mismatch (for example a vite base of /ui/) ships an
// index.html whose scripts 404, which leaves a blank console.
func TestSPAAssetsResolveFromTheMountPoint(t *testing.T) {
	e := newEnv(t, nil)
	resp, err := e.client.Get(e.ts.URL + "/ui/app")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index = %d", resp.StatusCode)
	}
	m := regexp.MustCompile(`/ui/app/assets/[A-Za-z0-9._-]+\.js`).FindString(string(body))
	if m == "" {
		t.Fatalf("index.html does not reference an /ui/app asset: %s", body)
	}
	asset, err := e.client.Get(e.ts.URL + m)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(asset.Body)
	asset.Body.Close()
	if asset.StatusCode != http.StatusOK || len(payload) == 0 {
		t.Fatalf("asset %s = %d (%d bytes)", m, asset.StatusCode, len(payload))
	}
	if ct := asset.Header.Get("Content-Type"); ct == "" {
		t.Fatalf("asset %s has no content type", m)
	}
}
