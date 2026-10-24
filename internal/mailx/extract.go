package mailx

import (
	"html"
	"regexp"
	"sort"
	"strings"
)

// Default extraction heuristics.
//
// Link patterns (first capture group is the URL; otherwise the whole match):
//  1. HTML href attributes, which cover the verification buttons used by IdPs.
//  2. Bare absolute http(s) URLs, which cover plain-text mails.
//
// OTP patterns, tried in order; the first pattern that produces candidates
// wins, because contextual matches ("your code is 123456") are more reliable
// than a bare digit run:
//  1. 4-8 digits in the 24 characters following a code-ish keyword.
//  2. 5-10 alphanumeric characters after a code-ish keyword (must contain a
//     digit, so ordinary words cannot match).
//  3. a standalone 6-8 digit run.
//  4. a standalone 6-8 character uppercase alphanumeric run containing a digit.
//
// Numeric scan text has HTML tags and URLs removed first, so digits inside a
// link, a CSS rule or a markup attribute are not mistaken for a code.
var defaultLinkPatterns = []string{
	`href\s*=\s*["']([^"'#][^"']*)["']`,
	`https?://[^\s"'<>]+`,
}

// otpPattern pairs a compiled expression with the extra digital requirement
// used by the alphanumeric heuristics.
type otpPattern struct {
	re           *regexp.Regexp
	requireDigit bool
}

var defaultOTPPatternExprs = []struct {
	expr         string
	requireDigit bool
}{
	{expr: `(?i)(?:verification|verify|security|one[-\s]?time|otp|passcode|code|pin|token)[^0-9]{0,24}([0-9]{4,8})\b`},
	{expr: `(?i)(?:verification|verify|security|one[-\s]?time|otp|passcode|code|pin|token)[^A-Za-z0-9]{0,24}([A-Z0-9]{5,10})\b`, requireDigit: true},
	{expr: `\b([0-9]{6,8})\b`},
	{expr: `\b([A-Z0-9]{6,8})\b`, requireDigit: true},
}

const (
	// maxCandidateLength bounds an extracted code or link length.
	maxCandidateLength = 2048
	// maxLinks and maxCodes bound how many transient candidates a Result keeps.
	maxLinks = 10
	maxCodes = 10
)

var (
	scriptRE   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleRE    = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	tagRE      = regexp.MustCompile(`(?is)<[^>]+>`)
	urlRE      = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	scoreURLRE = regexp.MustCompile(`(?i)(verify|verification|confirm|activate|activation|reset|signin|sign-in|signup|registration|magic|token|callback|auth)`)
	hostRE     = regexp.MustCompile(`^https?://[^/\s]+`)
)

func defaultOTPPatterns() []otpPattern {
	out := make([]otpPattern, 0, len(defaultOTPPatternExprs))
	for _, item := range defaultOTPPatternExprs {
		out = append(out, otpPattern{re: regexp.MustCompile(item.expr), requireDigit: item.requireDigit})
	}
	return out
}

// extractLinks collects absolute http(s) candidates from the transient body
// fields. HTML href values are unescaped so "&amp;" query separators survive.
func (m *Matcher) extractLinks(text, htmlBody string) []string {
	patterns := m.link
	if len(patterns) == 0 {
		return nil
	}
	var (
		raw   []string
		seen  = map[string]struct{}{}
		order = map[string]int{}
	)
	for _, source := range []string{text, htmlBody} {
		if source == "" {
			continue
		}
		for _, re := range patterns {
			for _, sm := range re.FindAllStringSubmatch(source, -1) {
				candidate := sm[0]
				if len(sm) > 1 && strings.TrimSpace(sm[1]) != "" {
					candidate = sm[1]
				}
				candidate = cleanURL(candidate)
				if candidate == "" {
					continue
				}
				if _, ok := seen[candidate]; ok {
					continue
				}
				seen[candidate] = struct{}{}
				order[candidate] = len(raw)
				raw = append(raw, candidate)
			}
		}
	}
	if len(raw) == 0 {
		return nil
	}

	// Prefer links whose URL carries verification-ish keywords; ties keep
	// document order. This keeps the primary ranking deterministic.
	type scored struct {
		url   string
		score int
		idx   int
	}
	ranked := make([]scored, 0, len(raw))
	for _, u := range raw {
		ranked = append(ranked, scored{url: u, score: linkScore(u), idx: order[u]})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].idx < ranked[j].idx
	})
	if len(ranked) > maxLinks {
		ranked = ranked[:maxLinks]
	}
	out := make([]string, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.url)
	}
	return out
}

// linkScore ranks a candidate by verification keywords in the URL.
func linkScore(u string) int {
	score := 0
	for _, match := range scoreURLRE.FindAllString(u, -1) {
		switch strings.ToLower(match) {
		case "verify", "verification", "confirm", "activate", "activation":
			score += 3
		case "reset", "signin", "sign-in", "signup", "registration", "magic", "callback":
			score += 2
		default:
			score++
		}
	}
	return score
}

// cleanURL unescapes HTML entities, trims surrounding whitespace/punctuation and
// rejects anything that is not an absolute http(s) URL.
func cleanURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = html.UnescapeString(s)
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'`")
	s = strings.TrimRight(s, ".,;:!?)]}")
	if s == "" || len(s) > maxCandidateLength {
		return ""
	}
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return ""
	}
	if !hostRE.MatchString(s) {
		return ""
	}
	return s
}

// extractCodes returns code candidates from the transient body fields.
func (m *Matcher) extractCodes(text, htmlBody string) []string {
	scan := codeScanText(text, htmlBody)
	for _, pattern := range m.otp {
		var codes []string
		seen := map[string]struct{}{}
		for _, sm := range pattern.re.FindAllStringSubmatch(scan, -1) {
			candidate := sm[0]
			if len(sm) > 1 && strings.TrimSpace(sm[1]) != "" {
				candidate = sm[1]
			}
			candidate = strings.TrimSpace(candidate)
			if candidate == "" || len(candidate) > 16 {
				continue
			}
			if pattern.requireDigit && !containsDigit(candidate) {
				continue
			}
			if !isPlausibleCode(candidate) {
				continue
			}
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			codes = append(codes, candidate)
			if len(codes) >= maxCodes {
				break
			}
		}
		if len(codes) > 0 {
			return codes
		}
	}
	return nil
}

// isPlausibleCode rejects candidates that are obviously not codes.
func isPlausibleCode(candidate string) bool {
	if len(candidate) < 4 || len(candidate) > 12 {
		return false
	}
	digits := 0
	for _, r := range candidate {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		default:
			return false
		}
	}
	return digits > 0
}

func containsDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// codeScanText builds the text searched for OTP codes: plain text plus the HTML
// body with scripts, styles, tags and URLs removed.
func codeScanText(text, htmlBody string) string {
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n")
	}
	if htmlBody != "" {
		stripped := scriptRE.ReplaceAllString(htmlBody, " ")
		stripped = styleRE.ReplaceAllString(stripped, " ")
		stripped = tagRE.ReplaceAllString(stripped, " ")
		stripped = html.UnescapeString(stripped)
		b.WriteString(stripped)
	}
	s := b.String()
	s = urlRE.ReplaceAllString(s, " ")
	return s
}
