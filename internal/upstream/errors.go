package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// UpstreamError is any failure talking to the deepseek.* sites.
type UpstreamError struct {
	Msg            string
	ContentStarted bool
	Cause          error
}

func (e *UpstreamError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "upstream failed"
}

func (e *UpstreamError) Unwrap() error { return e.Cause }

// QuotaExhaustedError means the site ran out of free quota for this session.
type QuotaExhaustedError struct{ UpstreamError }

// SessionVerificationError means Turnstile/nonce verification must redone.
type SessionVerificationError struct{ UpstreamError }

func errf(format string, a ...any) *UpstreamError {
	return &UpstreamError{Msg: fmt.Sprintf(format, a...)}
}

func newQuota(msg string) *QuotaExhaustedError {
	return &QuotaExhaustedError{UpstreamError: UpstreamError{Msg: msg}}
}

func newSession(msg string) *SessionVerificationError {
	return &SessionVerificationError{UpstreamError: UpstreamError{Msg: msg}}
}

// AsUpstream unwraps any error into an *UpstreamError.
func AsUpstream(err error) *UpstreamError {
	if err == nil {
		return nil
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue
	}
	return &UpstreamError{Msg: err.Error(), Cause: err}
}

func contentStarted(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.ContentStarted
	}
	return false
}

func isQuota(err error) bool {
	var q *QuotaExhaustedError
	return errors.As(err, &q)
}

func isSession(err error) bool {
	var s *SessionVerificationError
	return errors.As(err, &s)
}

// isSessionPayload classifies upstream JSON payloads that indicate the
// Turnstile cookie / nonce has to be re-established. It deliberately matches
// only explicit markers so a randomly embedded "nonce" string in echoed
// request data cannot trigger a costly re-solve.
func isSessionPayload(payload map[string]any) bool {
	var texts []string
	for _, k := range []string{"error", "message", "code"} {
		if v, ok := payload[k]; ok && v != nil {
			texts = append(texts, stringify(v))
		}
	}
	if data, ok := payload["data"].(map[string]any); ok {
		for _, k := range []string{"code", "message", "error"} {
			if v, ok := data[k]; ok && v != nil {
				texts = append(texts, stringify(v))
			}
		}
	}
	if notice, ok := payload["quota_notice"].(map[string]any); ok {
		if v, ok := notice["message"]; ok {
			texts = append(texts, stringify(v))
		}
	}
	text := strings.ToLower(strings.Join(texts, " "))
	for _, m := range []string{
		"deepseek_ts_required",
		"ts_required",
		"nonce_failure",
		"session_expired",
		"forbidden",
		"unauthorized",
		"csrf",
		"verification_required",
		"sicherheitspruefung",
	} {
		if strings.Contains(text, m) {
			return true
		}
	}
	if v, ok := payload["ts_required"]; ok {
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return strings.TrimSpace(t) != "" && t != "0" && !strings.EqualFold(t, "false")
		default:
			return true
		}
	}
	return false
}

// isConfigProblem reports errors that retrying cannot fix: the site stopped
// serving the AIPKit chat page (it now answers with a redirect or HTML), or
// the config points at something that does not exist.
func isConfigProblem(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "nonce fetch failed")
}

// isCacheEmptyProblem matches the site's cache-message rejection that a
// dropped config or a cookie re-solve never fixes on the same site: the
// mirrors carry independent sessions, so callers fail over instead of
// burning refresh cycles.
func isCacheEmptyProblem(err error) bool {
	return strings.Contains(err.Error(), "empty_data_to_cache")
}

// hasQuotaEnvelope reports whether the payload carries the upstream quota
// notice envelope, independent of event type and of the site language. The
// front end keys on the same markers (type "quota_notice" or a "notice"
// object), so a notice that travels without an "error" field is still caught.
func hasQuotaEnvelope(payload map[string]any) bool {
	if _, ok := payload["quota_notice"]; ok {
		return true
	}
	if t, ok := payload["type"].(string); ok && strings.EqualFold(strings.TrimSpace(t), "quota_notice") {
		return true
	}
	if n, ok := payload["notice"].(map[string]any); ok && len(n) > 0 {
		return true
	}
	return false
}

// quotaMarkers are the localized "allowance spent" wordings the mirrors send.
// The same server plugin runs on de/es/fr with a different language, so the
// text check must not key on the German phrasing alone.
var quotaMarkers = []string{
	// de
	"guthaben",
	"aufgebraucht",
	// es
	"agotado",
	"saldo insuficiente",
	// fr
	"épuisé",
	"epuis",
	"consomm",
	// en
	"quota",
	"exhausted",
	"out of credit",
}

// quotaTexts collects every human readable field a quota notice can travel in.
func quotaTexts(payload map[string]any) []string {
	var texts []string
	collect := func(m map[string]any, keys ...string) {
		for _, k := range keys {
			if v, ok := m[k]; ok && v != nil {
				texts = append(texts, stringify(v))
			}
		}
	}
	collect(payload, "error", "message", "code")
	for _, k := range []string{"data", "notice", "quota_notice"} {
		if m, ok := payload[k].(map[string]any); ok {
			collect(m, "error", "message", "code", "title", "text", "detail")
		}
	}
	return texts
}

// isQuotaPayload classifies a quota-exhausted payload. The envelope is
// language independent; the wording check covers the phrasing each mirror
// actually sends (de/es/fr) plus the English fallback.
func isQuotaPayload(payload map[string]any) bool {
	if hasQuotaEnvelope(payload) {
		return true
	}
	text := strings.ToLower(strings.Join(quotaTexts(payload), " "))
	if text == "" {
		return false
	}
	for _, m := range quotaMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// jsonPayload decodes a response body into a payload map for classification;
// a non-object body yields an empty map so callers can safely probe it.
func jsonPayload(body []byte) map[string]any {
	payload := map[string]any{}
	_ = json.Unmarshal(body, &payload)
	return payload
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
