// Package ops implements operator tooling for the account-onboarding service:
// consistent SQLite snapshots, restore verification, AES-256-GCM key rotation,
// sensitive-data scanning, an offline load smoke and HTTP hardening audits.
//
// Everything in this package operates on local files and loopback-only
// in-process HTTP servers (net/http/httptest). It never contacts a third-party
// service, never writes to a live database except the explicit rotation
// transaction, and never prints secret material.
//
// The command-line entry points live under internal/ops/cmd/* and are wrapped by
// scripts/*.sh. See docs/security/R4-audit.md and docs/acceptance/R4.md.
package ops

const (
	// ToolName identifies this tooling in backup manifests.
	ToolName = "account-onboarding-ops"
	// ToolVersion is the ops tooling version recorded in manifests.
	ToolVersion = "0.1.0-r4"
	// ManifestFormatVersion is the JSON manifest schema version.
	ManifestFormatVersion = 1
)
