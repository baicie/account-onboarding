package ops

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/baicie/account-onboarding/internal/security"
)

// Finding is one detected sensitive-data shape. Detail never contains the
// matched value; it only names the rule, the field and the size.
type Finding struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
	Detail string `json:"detail"`
}

// ScanOptions configures the sensitive-data scanner.
type ScanOptions struct {
	// KnownSecrets are exact secret values (tokens, passwords) that must not
	// appear verbatim in logs or event payloads. Values shorter than 4 bytes are
	// ignored. They are never echoed in findings.
	KnownSecrets []string
	// MaxFindings caps the report (default 200).
	MaxFindings int
}

// ScanReport is the scanner evidence.
type ScanReport struct {
	Sources        int            `json:"sources"`
	Lines          int            `json:"lines"`
	Bytes          int64          `json:"bytes"`
	Findings       []Finding      `json:"findings"`
	FindingsByKind map[string]int `json:"findings_by_kind"`
}

// Clean reports whether no sensitive shapes were found.
func (r ScanReport) Clean() bool { return len(r.Findings) == 0 }

// Merge folds another report into r.
func (r *ScanReport) Merge(o ScanReport) {
	if r.FindingsByKind == nil {
		r.FindingsByKind = map[string]int{}
	}
	r.Sources += o.Sources
	r.Lines += o.Lines
	r.Bytes += o.Bytes
	for k, v := range o.FindingsByKind {
		r.FindingsByKind[k] += v
	}
	r.Findings = append(r.Findings, o.Findings...)
}

var (
	scanJWT     = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\b`)
	scanAuthHdr = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+([A-Za-z0-9._~+/=-]{8,})`)
	scanAssign  = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])(password|passwd|pwd|secret|client_secret|admin_password|site_password|x-admin-auth|x-custom-auth|authorization|access_token|refresh_token|id_token|action_token|login_action_token|action-token|api_key|apikey|x-api-key|execution|session_code|tab_id|jwt|otp|token|state|code|key)["']?\s*[=:]\s*["']?([^\s"'&,;}]+)`)
	scanLong    = regexp.MustCompile(`[A-Za-z0-9+/_-]{48,}={0,2}`)
	scanPEM     = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	scanHex     = regexp.MustCompile(`^[0-9a-fA-F]{40,}$`)

	// Path-like strings are the one safe shape that can exceed the long-token
	// length threshold: absolute/relative filesystem paths, URL/API paths and
	// Windows paths. Only the heuristic long_token rule is suppressed for these
	// spans; high-confidence rules (JWT/Bearer/assignments/known secrets/PEM)
	// still see the original line.
	scanPathLike  = regexp.MustCompile(`(?:^|[\s"'=(:,])((?:~|\.{1,2})?/[A-Za-z0-9._@%+~/\-]{4,}|[A-Za-z]:[\\/][^\s"',}]+)`)
	scanPathField = regexp.MustCompile(`(?i)"(?:path|file|filepath|filename|dir|directory|log|log_path|db_path|work_dir|out_dir|data_dir|source|url|uri|endpoint)"\s*:\s*"([^"]{4,})"`)
	// Go import paths / URL paths without a leading slash (e.g.
	// github.com/baicie/account-onboarding/internal/provider/mailbox/cloudflare)
	// are path metadata, not credentials.
	scanImportPath = regexp.MustCompile(`[A-Za-z0-9._~+@-]+(?:/[A-Za-z0-9._~+@-]+){2,}`)
	// SCREAMING_SNAKE_CASE identifiers are environment variable names or docs
	// labels (e.g. ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE, R51_IMAGE/R51_DATA).
	scanEnvIdent = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+){2,}\b`)
	// Generated container/network/project identifiers are kebab-case
	// (onboarding-r51-r51-1792901234-11967-onboarding-1); mask multi-segment
	// lowercase identifiers for the heuristic long_token rule.
	scanKebabIdent = regexp.MustCompile(`\b[a-z0-9]+(?:-[a-z0-9]+){3,}\b`)
)

// pathLikeSpans returns the byte ranges of path-like substrings. Long-token
// detection skips matches that overlap one of these spans.
func pathLikeSpans(line string) []span {
	var out []span
	for _, m := range scanPathLike.FindAllStringSubmatchIndex(line, -1) {
		if m[2] >= 0 && m[3] > m[2] {
			out = append(out, span{m[2], m[3]})
		}
	}
	for _, m := range scanPathField.FindAllStringSubmatchIndex(line, -1) {
		if m[2] < 0 || m[3] <= m[2] {
			continue
		}
		value := line[m[2]:m[3]]
		// A "path" field whose value is pure token material (no separators) is
		// still suspicious; only mask values that actually look like paths.
		if strings.ContainsAny(value, `/\\`) {
			out = append(out, span{m[2], m[3]})
		}
	}
	for _, m := range scanImportPath.FindAllStringIndex(line, -1) {
		out = append(out, span{m[0], m[1]})
	}
	for _, m := range scanEnvIdent.FindAllStringIndex(line, -1) {
		out = append(out, span{m[0], m[1]})
	}
	for _, m := range scanKebabIdent.FindAllStringIndex(line, -1) {
		out = append(out, span{m[0], m[1]})
	}
	return out
}

type span struct{ start, end int }

func overlaps(spans []span, s, e int) bool {
	for _, sp := range spans {
		if s < sp.end && e > sp.start {
			return true
		}
	}
	return false
}

// ScanReader scans one text stream (a log file, captured output or a single
// event payload rendered as text) line by line.
func ScanReader(source string, rd io.Reader, opts ScanOptions) (ScanReport, error) {
	if opts.MaxFindings <= 0 {
		opts.MaxFindings = 200
	}
	rep := ScanReport{FindingsByKind: map[string]int{}, Sources: 1}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		rep.Lines++
		rep.Bytes += int64(len(text)) + 1
		scanLine(source, line, text, opts, &rep)
	}
	if err := sc.Err(); err != nil {
		return rep, err
	}
	return rep, nil
}

// ScanFile scans one local file.
func ScanFile(path string, opts ScanOptions) (ScanReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return ScanReport{FindingsByKind: map[string]int{}}, fmt.Errorf("scan file %s: %w", path, err)
	}
	defer f.Close()
	return ScanReader(path, f, opts)
}

func scanLine(source string, line int, text string, opts ScanOptions, rep *ScanReport) {
	if strings.TrimSpace(text) == "" {
		return
	}
	add := func(kind string, col int, detail string) {
		if len(rep.Findings) >= opts.MaxFindings {
			return
		}
		rep.Findings = append(rep.Findings, Finding{Kind: kind, Source: source, Line: line, Column: col, Detail: detail})
		rep.FindingsByKind[kind]++
	}

	paths := pathLikeSpans(text)
	var occupied []span
	for _, loc := range scanJWT.FindAllStringIndex(text, -1) {
		occupied = append(occupied, span{loc[0], loc[1]})
		add("jwt", loc[0], fmt.Sprintf("JWT-shaped value detected (%d bytes, redacted)", loc[1]-loc[0]))
	}
	for _, m := range scanAuthHdr.FindAllStringSubmatchIndex(text, -1) {
		kind := strings.ToLower(text[m[2]:m[3]])
		occupied = append(occupied, span{m[0], m[1]})
		add("authorization_header", m[0], fmt.Sprintf("%s credential detected (%d bytes, redacted)", kind, m[1]-m[0]))
	}
	for _, m := range scanAssign.FindAllStringSubmatchIndex(text, -1) {
		key := strings.ToLower(text[m[2]:m[3]])
		value := strings.Trim(text[m[4]:m[5]], `"'`)
		if !suspiciousAssignment(key, value) {
			continue
		}
		occupied = append(occupied, span{m[0], m[1]})
		add("credential_assignment", m[0], fmt.Sprintf("sensitive assignment %s=<redacted> (%d bytes)", key, len(value)))
	}
	for _, loc := range scanLong.FindAllStringIndex(text, -1) {
		raw := text[loc[0]:loc[1]]
		if scanHex.MatchString(raw) {
			// A hex digest (sha256, HMAC) is integrity metadata, not a bearer
			// credential. HMAC resource ids are explicitly designed to be stored.
			continue
		}
		if overlaps(occupied, loc[0], loc[1]) || overlaps(paths, loc[0], loc[1]) {
			continue
		}
		add("long_token", loc[0], fmt.Sprintf("long base64-like token shape detected (%d bytes, redacted)", len(raw)))
	}
	if loc := scanPEM.FindStringIndex(text); loc != nil {
		add("private_key", loc[0], "PEM private key block detected")
	}
	for _, secret := range opts.KnownSecrets {
		if len(secret) < 4 {
			continue
		}
		if idx := strings.Index(text, secret); idx >= 0 {
			add("known_secret", idx, fmt.Sprintf("registered secret value detected (%d bytes, redacted)", len(secret)))
		}
	}
}

var safeEnumValues = map[string]bool{
	"true": true, "false": true, "null": true, "none": true, "nil": true,
	"ok": true, "success": true, "error": true, "pending": true,
	"completed": true, "verified": true, "redacted": true, "unknown": true,
	"expired": true, "cancelled": true, "canceled": true, "waiting": true,
	"authorized": true, "denied": true, "valid": true, "invalid": true,
}

var enumShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,39}$`)

// suspiciousAssignment applies high-confidence heuristics so that normal
// workflow enums (`resume_state=OAUTH_PENDING`, `status="COMPLETED"`) are not
// reported while random-looking values are.
func suspiciousAssignment(key, value string) bool {
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	if lower == "redacted" || strings.HasPrefix(lower, "[redacted") || value == "***" {
		return false
	}
	switch key {
	case "password", "passwd", "pwd", "secret", "client_secret", "admin_password", "site_password", "x-admin-auth", "x-custom-auth":
		return len(value) >= 4
	case "authorization":
		// "Authorization: Bearer <token>" is handled by the dedicated detector;
		// a bare scheme word is not a leak.
		return len(value) >= 16 && !strings.EqualFold(value, "bearer") && !strings.EqualFold(value, "basic")
	default:
		// token / state / code / otp / jwt / *_token
		if len(value) < 8 {
			return false
		}
		if safeEnumValues[lower] || enumShape.MatchString(value) {
			return false
		}
		return true
	}
}

// ScanDatabase scans persisted artifacts for leaked secrets:
// events.public_payload_json, steps.error_summary/result_json,
// audit_log.detail_json, resources display_alias/extra_json and
// runs.provider_snapshot_json.
//
// The secrets table itself is deliberately not scanned: it stores AES-256-GCM
// ciphertext and nonces by design. Decryption proof is provided by Restore and
// RotateSecrets instead.
func ScanDatabase(ctx context.Context, dbPath string, opts ScanOptions) (ScanReport, error) {
	if opts.MaxFindings <= 0 {
		opts.MaxFindings = 200
	}
	rep := ScanReport{FindingsByKind: map[string]int{}}
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return rep, err
	}
	defer db.Close()

	scanRows := func(table, query string, scan func(*ScanReport, []string) error) error {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such table") {
				return nil
			}
			return fmt.Errorf("scan %s: %w", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := scanRowValues(rows)
			if err != nil {
				return fmt.Errorf("scan %s: %w", table, err)
			}
			if err := scan(&rep, vals); err != nil {
				return err
			}
		}
		return rows.Err()
	}

	if err := scanRows("events", `SELECT CAST(id AS TEXT), public_payload_json FROM events ORDER BY id`, func(rep *ScanReport, vals []string) error {
		rowID, payload := vals[0], vals[1]
		sub, err := ScanReader("events:"+rowID, strings.NewReader(payload), opts)
		if err != nil {
			return err
		}
		rep.Merge(sub)
		scanPayloadKeys("events:"+rowID, payload, rep)
		return nil
	}); err != nil {
		return rep, err
	}

	if err := scanRows("steps", `SELECT id, COALESCE(error_summary,''), COALESCE(result_json,'{}') FROM steps ORDER BY rowid`, func(rep *ScanReport, vals []string) error {
		rowID := vals[0]
		for i, text := range vals[1:] {
			if strings.TrimSpace(text) == "" {
				continue
			}
			sub, err := ScanReader("steps:"+rowID, strings.NewReader(text), opts)
			if err != nil {
				return err
			}
			rep.Merge(sub)
			if i == 1 {
				scanPayloadKeys("steps:"+rowID, text, rep)
			}
		}
		return nil
	}); err != nil {
		return rep, err
	}

	if err := scanRows("audit_log", `SELECT CAST(id AS TEXT), detail_json FROM audit_log ORDER BY id`, func(rep *ScanReport, vals []string) error {
		rowID, payload := vals[0], vals[1]
		sub, err := ScanReader("audit_log:"+rowID, strings.NewReader(payload), opts)
		if err != nil {
			return err
		}
		rep.Merge(sub)
		scanPayloadKeys("audit_log:"+rowID, payload, rep)
		return nil
	}); err != nil {
		return rep, err
	}

	if err := scanRows("resources", `SELECT id, COALESCE(display_alias,''), COALESCE(extra_json,'{}') FROM resources ORDER BY rowid`, func(rep *ScanReport, vals []string) error {
		rowID := vals[0]
		for i, text := range vals[1:] {
			if strings.TrimSpace(text) == "" {
				continue
			}
			sub, err := ScanReader(fmt.Sprintf("resources:%s:%d", rowID, i), strings.NewReader(text), opts)
			if err != nil {
				return err
			}
			rep.Merge(sub)
		}
		return nil
	}); err != nil {
		return rep, err
	}

	if err := scanRows("runs", `SELECT id, provider_snapshot_json FROM runs ORDER BY rowid`, func(rep *ScanReport, vals []string) error {
		sub, err := ScanReader("runs:"+vals[0], strings.NewReader(vals[1]), opts)
		if err != nil {
			return err
		}
		rep.Merge(sub)
		return nil
	}); err != nil {
		return rep, err
	}

	sort.SliceStable(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].Source != rep.Findings[j].Source {
			return rep.Findings[i].Source < rep.Findings[j].Source
		}
		if rep.Findings[i].Line != rep.Findings[j].Line {
			return rep.Findings[i].Line < rep.Findings[j].Line
		}
		return rep.Findings[i].Kind < rep.Findings[j].Kind
	})
	return rep, nil
}

// scanPayloadKeys enforces the event-payload allowlist (the same gate the
// service applies before persisting). A disallowed key is a leak even when its
// value looks harmless.
func scanPayloadKeys(source, raw string, rep *ScanReport) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return
	}
	for k := range payload {
		if security.IsEventPayloadKeyAllowed(k) {
			continue
		}
		if len(rep.Findings) >= 200 {
			return
		}
		rep.Findings = append(rep.Findings, Finding{
			Kind:   "event_key_not_allowlisted",
			Source: source,
			Detail: fmt.Sprintf("payload key %q is outside the event allowlist", k),
		})
		rep.FindingsByKind["event_key_not_allowlisted"]++
	}
}

// scanRowValues returns the row as strings. Columns are selected explicitly by
// the call sites, so a dynamic scan keeps this helper reusable.
func scanRowValues(rows *sql.Rows) ([]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}
	out := make([]string, len(raw))
	for i := range raw {
		out[i] = string(raw[i])
	}
	return out, nil
}
