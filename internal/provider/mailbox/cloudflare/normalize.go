package cloudflare

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

// Normalizer scope
//
// The live worker's parsed_mails schema is frozen only by a live contract test
// (docs/08). Until then this normalizer documents and tolerates the variations
// seen in the upstream project and in local deployments:
//
//   - List envelope: {"results":[...], "count":N}, {"parsed_mails":[...]},
//     {"mails":[...]}, {"messages":[...]}, {"data":[...]}, {"items":[...]},
//     {"list":[...]}, a bare [...], or a null list.
//   - Mail item: a JSON object with direct fields, or with a "raw" field that is
//     either an RFC822-ish string ("Subject: ...\r\n\r\nbody") or an object
//     ({"subject","from","to","date","text","html","body","headers":{...}}).
//   - Field aliases are matched case-insensitively. "from"/"to" may be plain
//     strings, {address,name} objects or arrays of either.
//   - Timestamps accept RFC3339(Nano), "2006-01-02 15:04:05", RFC1123/822 and
//     Unix seconds/milliseconds. An unparsable timestamp becomes the zero time;
//     mailx then rejects the message instead of guessing.
//   - An item with no recognizable field is skipped (returns ok=false) so one
//     placeholder row cannot break the whole page.
//   - A missing message ID is derived deterministically from the normalized
//     content so deduplication stays stable across polls.

// parseMailList extracts the raw mail items from a parsed_mails response body.
func parseMailList(body []byte, op string) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errs.New(errs.ClassProviderBug, op, "cloudflare returned an empty parsed_mails body")
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, errs.New(errs.ClassProviderBug, op, "cloudflare returned malformed parsed_mails JSON")
		}
		return items, nil
	case '{':
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &wrapper); err != nil {
			return nil, errs.New(errs.ClassProviderBug, op, "cloudflare returned malformed parsed_mails JSON")
		}
		for _, key := range []string{"results", "parsed_mails", "mails", "messages", "data", "items", "list"} {
			raw, ok := lookupKey(wrapper, key)
			if !ok {
				continue
			}
			value := bytes.TrimSpace(raw)
			if len(value) == 0 || bytes.EqualFold(value, []byte("null")) {
				return nil, nil
			}
			if value[0] != '[' {
				continue // not a list under this key; keep looking
			}
			var items []json.RawMessage
			if err := json.Unmarshal(value, &items); err != nil {
				return nil, errs.New(errs.ClassProviderBug, op, "cloudflare returned malformed parsed_mails JSON")
			}
			return items, nil
		}
		return nil, errs.New(errs.ClassProviderBug, op, "cloudflare parsed_mails response has an unrecognized schema")
	default:
		return nil, errs.New(errs.ClassProviderBug, op, "cloudflare parsed_mails response is not JSON")
	}
}

// normalizeListItem converts one list item into a provider.Message.
func normalizeListItem(raw json.RawMessage) (provider.Message, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.EqualFold(trimmed, []byte("null")) {
		return provider.Message{}, false, nil
	}
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return provider.Message{}, false, errs.New(errs.ClassProviderBug, opListMessages, "cloudflare returned a malformed mail item")
		}
		return normalizeMailObject(obj)
	case '"':
		var rawMail string
		if err := json.Unmarshal(trimmed, &rawMail); err != nil {
			return provider.Message{}, false, errs.New(errs.ClassProviderBug, opListMessages, "cloudflare returned a malformed raw mail item")
		}
		if strings.TrimSpace(rawMail) == "" {
			return provider.Message{}, false, nil
		}
		fields := parseRFC822(rawMail)
		return fields.toMessage(opListMessages)
	default:
		return provider.Message{}, false, nil
	}
}

// normalizeDetail normalizes a single-message detail response. The deployment
// may return the mail object directly or wrap it in results/mail/data/result.
func normalizeDetail(body []byte) (provider.Message, bool, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return provider.Message{}, false, nil
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return provider.Message{}, false, errs.New(errs.ClassProviderBug, opGetMessage, "cloudflare returned malformed detail JSON")
		}
		for _, item := range items {
			if msg, ok, err := normalizeListItem(item); err != nil || ok {
				return msg, ok, err
			}
		}
		return provider.Message{}, false, nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return provider.Message{}, false, errs.New(errs.ClassProviderBug, opGetMessage, "cloudflare returned malformed detail JSON")
		}
		if msg, ok, err := normalizeMailObject(obj); err != nil || ok {
			return msg, ok, err
		}
		for _, key := range []string{"results", "mail", "data", "result", "item", "parsed_mail"} {
			raw, found := lookupKey(obj, key)
			if !found {
				continue
			}
			if msg, ok, err := normalizeOneOrFirst(raw); err != nil || ok {
				return msg, ok, err
			}
		}
		return provider.Message{}, false, nil
	default:
		return provider.Message{}, false, nil
	}
}

// normalizeOneOrFirst normalizes a mail object, or the first usable element of a
// mail array.
func normalizeOneOrFirst(raw json.RawMessage) (provider.Message, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return provider.Message{}, false, nil
	}
	if trimmed[0] != '[' {
		return normalizeListItem(trimmed)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return provider.Message{}, false, errs.New(errs.ClassProviderBug, opGetMessage, "cloudflare returned malformed detail JSON")
	}
	for _, item := range items {
		if msg, ok, err := normalizeListItem(item); err != nil || ok {
			return msg, ok, err
		}
	}
	return provider.Message{}, false, nil
}

// normalizeMailObject maps one JSON object to a provider.Message.
func normalizeMailObject(obj map[string]json.RawMessage) (provider.Message, bool, error) {
	fields := mailFields{}

	// 1. Raw payload first: direct fields override it afterwards.
	if raw, ok := lookupKey(obj, "raw"); ok {
		fields.absorbRaw(raw)
	}
	// 2. Direct (parsed) fields.
	fields.absorbMap(obj)
	fields.absorbHeaders(obj)

	return fields.toMessage(opListMessages)
}

// mailFields is the intermediate normalized projection.
type mailFields struct {
	id          string
	subject     string
	sender      string
	recipients  []string
	text        string
	html        string
	received    time.Time
	hasID       bool
	hasSubject  bool
	hasSender   bool
	hasTo       bool
	hasText     bool
	hasHTML     bool
	hasReceived bool
	hasRaw      bool
}

func (f *mailFields) absorbMap(obj map[string]json.RawMessage) {
	if raw, ok := lookupKey(obj, "id", "mail_id", "message_id", "msg_id"); ok {
		if value, ok := scalarToString(raw); ok {
			f.id, f.hasID = value, true
		}
	}
	if raw, ok := lookupKey(obj, "subject"); ok {
		if value, ok := scalarToString(raw); ok {
			f.subject, f.hasSubject = value, true
		}
	}
	if raw, ok := lookupKey(obj, "from", "sender"); ok {
		if value, ok := addressField(raw); ok {
			f.sender, f.hasSender = value, true
		}
	}
	if raw, ok := lookupKey(obj, "to", "recipient", "recipients", "delivered_to"); ok {
		if values := addressListField(raw); len(values) > 0 {
			f.recipients, f.hasTo = values, true
		}
	}
	if raw, ok := lookupKey(obj, "received_at", "receivedAt", "created_at", "createdAt", "created", "date", "time", "timestamp"); ok {
		if value, ok := parseTimeValue(raw); ok {
			f.received, f.hasReceived = value, true
		}
	}
	if raw, ok := lookupKey(obj, "text", "body", "content", "raw_text", "text_body", "plain"); ok {
		if value, ok := scalarToString(raw); ok {
			f.text, f.hasText = value, true
		}
	}
	if raw, ok := lookupKey(obj, "html", "html_body", "content_html"); ok {
		if value, ok := scalarToString(raw); ok {
			f.html, f.hasHTML = value, true
		}
	}
}

// absorbHeaders maps an explicit {"headers": {...}} sub-object, using the same
// aliases as RFC822 header names.
func (f *mailFields) absorbHeaders(obj map[string]json.RawMessage) {
	raw, ok := lookupKey(obj, "headers", "header")
	if !ok {
		return
	}
	var headers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &headers); err != nil {
		return
	}
	if raw, ok := lookupKey(headers, "subject"); ok {
		if value, ok := scalarToString(raw); ok && value != "" {
			f.subject, f.hasSubject = value, true
		}
	}
	if raw, ok := lookupKey(headers, "from"); ok {
		if value, ok := addressField(raw); ok && value != "" {
			f.sender, f.hasSender = value, true
		}
	}
	if raw, ok := lookupKey(headers, "to"); ok {
		if values := addressListField(raw); len(values) > 0 {
			f.recipients, f.hasTo = values, true
		}
	}
	if raw, ok := lookupKey(headers, "date"); ok {
		if value, ok := parseTimeValue(raw); ok {
			f.received, f.hasReceived = value, true
		}
	}
	if raw, ok := lookupKey(headers, "message-id", "message_id", "messageid"); ok {
		if value, ok := scalarToString(raw); ok && value != "" {
			f.id, f.hasID = value, true
		}
	}
}

// absorbRaw handles the "raw" field, which is either an RFC822-ish string or a
// nested object with the same aliases.
func (f *mailFields) absorbRaw(raw json.RawMessage) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.EqualFold(trimmed, []byte("null")) {
		return
	}
	switch trimmed[0] {
	case '"':
		var rawMail string
		if err := json.Unmarshal(trimmed, &rawMail); err != nil {
			return
		}
		if strings.TrimSpace(rawMail) == "" {
			return
		}
		parsed := parseRFC822(rawMail)
		f.mergeLowerPriority(parsed)
		f.hasRaw = true
	case '{':
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &nested); err != nil {
			return
		}
		parsed := mailFields{}
		parsed.absorbMap(nested)
		parsed.absorbHeaders(nested)
		f.mergeLowerPriority(parsed)
		f.hasRaw = true
	}
}

// mergeLowerPriority copies fields that are not set yet.
func (f *mailFields) mergeLowerPriority(other mailFields) {
	if !f.hasID && other.hasID {
		f.id, f.hasID = other.id, true
	}
	if !f.hasSubject && other.hasSubject {
		f.subject, f.hasSubject = other.subject, true
	}
	if !f.hasSender && other.hasSender {
		f.sender, f.hasSender = other.sender, true
	}
	if !f.hasTo && other.hasTo {
		f.recipients, f.hasTo = other.recipients, true
	}
	if !f.hasText && other.hasText {
		f.text, f.hasText = other.text, true
	}
	if !f.hasHTML && other.hasHTML {
		f.html, f.hasHTML = other.html, true
	}
	if !f.hasReceived && other.hasReceived {
		f.received, f.hasReceived = other.received, true
	}
}

// toMessage projects non-empty fields into provider.Message. ok=false when the
// object carried nothing recognizable.
func (f *mailFields) toMessage(op string) (provider.Message, bool, error) {
	recognized := f.hasID || f.hasSubject || f.hasSender || f.hasTo || f.hasText || f.hasHTML || f.hasReceived
	if !recognized && !f.hasRaw {
		return provider.Message{}, false, nil
	}

	msg := provider.Message{
		ID:         strings.TrimSpace(f.id),
		Sender:     strings.TrimSpace(f.sender),
		Subject:    strings.TrimSpace(f.subject),
		Text:       f.text,
		HTML:       f.html,
		ReceivedAt: f.received.UTC(),
	}
	if len(f.recipients) > 0 {
		msg.Recipient = strings.TrimSpace(f.recipients[0])
	}
	if msg.ID == "" {
		msg.ID = derivedMessageID(msg)
	}
	return msg, true, nil
}

// derivedMessageID builds a stable identifier for deployments that omit "id".
// It hashes only non-secret routing metadata plus the body, so the same message
// keeps the same ID across polls.
func derivedMessageID(msg provider.Message) string {
	h := sha256.New()
	for _, part := range []string{
		msg.Subject,
		msg.Sender,
		msg.Recipient,
		msg.ReceivedAt.UTC().Format(time.RFC3339Nano),
		msg.Text,
		msg.HTML,
	} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return "cf-auto-" + hex.EncodeToString(h.Sum(nil)[:8])
}

// parseRFC822 extracts the header fields and the body from an RFC822-ish
// message string.
func parseRFC822(rawMail string) mailFields {
	rawMail = strings.ReplaceAll(rawMail, "\r\n", "\n")
	head, body := rawMail, ""
	if idx := strings.Index(rawMail, "\n\n"); idx >= 0 {
		head, body = rawMail[:idx], rawMail[idx+2:]
	}

	headers := map[string]string{}
	var current string
	for _, line := range strings.Split(head, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if current != "" {
				headers[current] += " " + strings.TrimSpace(line)
			}
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			current = ""
			continue
		}
		current = strings.ToLower(strings.TrimSpace(name))
		headers[current] = strings.TrimSpace(value)
	}

	fields := mailFields{}
	fields.hasRaw = true
	if value := headers["subject"]; value != "" {
		fields.subject, fields.hasSubject = value, true
	}
	if value := headers["from"]; value != "" {
		fields.sender, fields.hasSender = value, true
	}
	if value := headers["to"]; value != "" {
		fields.recipients, fields.hasTo = splitAddressHeader(value), true
	}
	if value := headers["date"]; value != "" {
		if parsed, ok := parseTimeString(value); ok {
			fields.received, fields.hasReceived = parsed, true
		}
	}
	if value := headers["message-id"]; value != "" {
		fields.id, fields.hasID = strings.Trim(value, "<>"), true
	}
	if body != "" {
		fields.text, fields.hasText = body, true
		if looksLikeHTML(body) {
			fields.html, fields.hasHTML = body, true
		}
	}
	return fields
}

// splitAddressHeader splits a To/Cc header value on commas that are outside
// angle brackets and returns the address parts.
func splitAddressHeader(value string) []string {
	var out []string
	var current strings.Builder
	inAngle := false
	for _, r := range value {
		switch r {
		case '<':
			inAngle = true
			current.WriteRune(r)
		case '>':
			inAngle = false
			current.WriteRune(r)
		case ',':
			if inAngle {
				current.WriteRune(r)
				continue
			}
			if part := strings.TrimSpace(current.String()); part != "" {
				out = append(out, part)
			}
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if part := strings.TrimSpace(current.String()); part != "" {
		out = append(out, part)
	}
	return out
}

// addressField normalizes a From-ish value: string, {name,address,email} object,
// or the first element of an array.
func addressField(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.EqualFold(trimmed, []byte("null")) {
		return "", false
	}
	switch trimmed[0] {
	case '"':
		return scalarToString(trimmed)
	case '{':
		var obj struct {
			Address string `json:"Address"`
			Email   string `json:"Email"`
			Name    string `json:"Name"`
			Value   string `json:"value"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return "", false
		}
		address := firstNonEmpty(obj.Address, obj.Email, obj.Value)
		name := strings.TrimSpace(obj.Name)
		switch {
		case address != "" && name != "":
			return name + " <" + address + ">", true
		case address != "":
			return address, true
		case name != "":
			return name, true
		}
		return "", false
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return "", false
		}
		for _, item := range items {
			if value, ok := addressField(item); ok {
				return value, true
			}
		}
		return "", false
	default:
		return scalarToString(trimmed)
	}
}

// addressListField normalizes a To-ish value into a list of addresses.
func addressListField(raw json.RawMessage) []string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.EqualFold(trimmed, []byte("null")) {
		return nil
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil
		}
		var out []string
		for _, item := range items {
			if value, ok := addressField(item); ok {
				out = append(out, value)
			}
		}
		return out
	case '"':
		value, ok := scalarToString(trimmed)
		if !ok {
			return nil
		}
		return splitAddressHeader(value)
	default:
		if value, ok := addressField(trimmed); ok {
			return []string{value}
		}
		return nil
	}
}

// scalarToString decodes a JSON string, number or bool into a string.
func scalarToString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.EqualFold(trimmed, []byte("null")) {
		return "", false
	}
	switch trimmed[0] {
	case '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", false
		}
		return value, true
	case '{', '[':
		return "", false
	default:
		return string(trimmed), true
	}
}

// parseTimeValue decodes a JSON timestamp: string formats or a Unix epoch number.
func parseTimeValue(raw json.RawMessage) (time.Time, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.EqualFold(trimmed, []byte("null")) {
		return time.Time{}, false
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return time.Time{}, false
		}
		return parseTimeString(value)
	}
	return parseEpochString(string(trimmed))
}

// timeLayouts are the timestamp formats accepted by the normalizer.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05",
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	time.ANSIC,
	time.UnixDate,
	time.RFC850,
}

func parseTimeString(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	if epoch, ok := parseEpochString(value); ok {
		return epoch, true
	}
	return time.Time{}, false
}

// parseEpochString parses Unix seconds or milliseconds from a numeric string.
func parseEpochString(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	switch {
	case number > 1e15: // microseconds
		return time.Unix(0, number*1e3).UTC(), true
	case number > 1e12: // milliseconds
		return time.Unix(0, number*1e6).UTC(), true
	case number > 1e9: // seconds
		return time.Unix(number, 0).UTC(), true
	default:
		return time.Time{}, false
	}
}

// decodeAddressID normalizes new_address address_id, which may be a JSON number
// or string, into a bounded identifier.
func decodeAddressID(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.EqualFold(trimmed, []byte("null")) {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return ""
		}
		return strings.TrimSpace(value)
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return ""
		}
		for _, key := range []string{"id", "address_id", "value"} {
			if nested, ok := lookupKey(obj, key); ok {
				if value := decodeAddressID(nested); value != "" {
					return value
				}
			}
		}
		return ""
	default:
		return string(trimmed)
	}
}

// isDuplicateResponse reports whether a create response means "this address
// already exists" and can be recovered by retrying with enablePrefix=true.
func isDuplicateResponse(res *httpResult) bool {
	if res.status == 409 {
		return true
	}
	if res.status != 400 && res.status != 422 {
		return false
	}
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(res.body, &payload); err != nil {
		return false
	}
	joined := strings.ToLower(strings.Join([]string{payload.Error, payload.Message, payload.Detail, payload.Msg}, " "))
	for _, marker := range []string{
		"already exist", "alreadyexist", "already exists", "duplicate", "address exists", "already registered", "已存在", "已被占用", "重复",
	} {
		if strings.Contains(joined, marker) {
			return true
		}
	}
	return false
}

// lookupKey performs a case-insensitive multi-alias lookup on a JSON object.
func lookupKey(obj map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if raw, ok := obj[key]; ok {
			return raw, true
		}
	}
	for name, raw := range obj {
		for _, key := range keys {
			if strings.EqualFold(name, key) {
				return raw, true
			}
		}
	}
	return nil, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func looksLikeHTML(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "<html") || strings.Contains(lower, "<a ") || strings.Contains(lower, "<br")
}
