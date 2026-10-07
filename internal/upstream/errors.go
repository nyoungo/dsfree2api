package upstream

import (
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

// isQuotaPayload mirrors the German/English quota notices used upstream.
func isQuotaPayload(payload map[string]any) bool {
	if _, ok := payload["quota_notice"]; ok {
		return true
	}
	var texts []string
	for _, k := range []string{"error", "message"} {
		if v, ok := payload[k]; ok && v != nil {
			texts = append(texts, stringify(v))
		}
	}
	if notice, ok := payload["quota_notice"].(map[string]any); ok {
		if v, ok := notice["message"]; ok {
			texts = append(texts, stringify(v))
		}
	}
	text := strings.ToLower(strings.Join(texts, " "))
	if strings.Contains(text, "guthaben") || strings.Contains(text, "quota") {
		return true
	}
	return strings.Contains(text, "token") && strings.Contains(text, "aufgebraucht")
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
