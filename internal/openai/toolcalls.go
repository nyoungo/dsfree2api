package openai

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Special begin/end markers DeepSeek emits around a tool-call block. The
// separators use U+2581 / U+FF5C lookalikes, so tag matching normalizes them.
const (
	toolCallStart = "<|tool▁calls▁begin|>"
	toolCallEnd   = "<|tool▁calls▁end|>"
)

// builtinStartTags / builtinEndTags mirror the reference defaults: the special
// tokens plus the <tool_call>/<tool_calls> XML wrappers models hallucinate.
var (
	builtinStartTags = []string{toolCallStart, "<|tool_call_begin|>", "<tool_calls>", "<tool_call>"}
	builtinEndTags   = []string{toolCallEnd, "<|tool_call_end|>", "</tool_calls>", "</tool_call>"}
)

// ParseOptions tunes tool-call extraction. ExtraStarts/ExtraEnds add tag
// wrappers beyond the built-ins; ToolNames lets the loose bare/spilled-object
// repair require a name that actually matches an offered tool.
type ParseOptions struct {
	ExtraStarts []string
	ExtraEnds   []string
	ToolNames   []string
}

func optionsForTools(tools []ToolDef) ParseOptions {
	opts := ParseOptions{}
	for _, t := range tools {
		if n := strings.TrimSpace(t.Function.Name); n != "" {
			opts.ToolNames = append(opts.ToolNames, n)
		}
	}
	return opts
}

// TryParseToolCalls extracts an OpenAI-style tool_calls object from raw model
// output. It mirrors the Python/Rust implementations: plain JSON, ```json
// fences, generic ``` fences, XML/<tool_call>/<invoke> wrappers and the usual
// model repairs (unquoted keys, invalid backslashes, field aliases,
// swapped/ spilled arguments) are all accepted, plus DeepSeek's DSML dialect
// (see dsml.go) including truncated blocks with missing closing tags.
//
// Returns the remaining prose plus the parsed tool calls (ok=false when the
// text is ordinary chat).
func TryParseToolCalls(text string) (string, []ToolCall, bool) {
	return TryParseToolCallsWith(text, ParseOptions{})
}

// TryParseToolCallsForTools is TryParseToolCalls with the offered tool names
// used to validate the looser repairs (bare object / spilled parameters).
func TryParseToolCallsForTools(text string, tools []ToolDef) (string, []ToolCall, bool) {
	return TryParseToolCallsWith(text, optionsForTools(tools))
}

// TryParseToolCallsWith is TryParseToolCalls with explicit options.
func TryParseToolCallsWith(text string, opts ParseOptions) (string, []ToolCall, bool) {
	norm, isDSML := normalizeDSML(text)
	// DSML 方言优先按 tagged 路径解析：参数内容里形如 [{"name":…}] 的片段是
	// 正被写入的文件数据，若先跑 JSON 候选路径会被抢成假调用；解析失败时
	// 仍回落到下面的常规两段路径。
	if isDSML {
		if remaining, calls, ok := parseTaggedToolCalls(ensureTaggedStart(norm, opts), opts); ok && matchesOfferedTools(calls, opts) {
			return remaining, calls, true
		}
	}
	if remaining, calls, ok := parseJSONToolCalls(text, opts); ok {
		return remaining, calls, true
	}
	return parseTaggedToolCalls(text, opts)
}

// matchesOfferedTools 报告解析结果与工具清单是否相容：DSML 只是兜底方言，
// 调用名不在已提供的工具里时多半是散文引用了标记格式，按普通文本返回。
func matchesOfferedTools(calls []ToolCall, opts ParseOptions) bool {
	if len(opts.ToolNames) == 0 {
		return true
	}
	for _, c := range calls {
		if !containsString(opts.ToolNames, c.Function.Name) {
			return false
		}
	}
	return len(calls) > 0
}

// ---------------------------------------------------------------------------
// JSON payload path
// ---------------------------------------------------------------------------

func parseJSONToolCalls(text string, opts ParseOptions) (string, []ToolCall, bool) {
	for _, cand := range jsonCandidates(text) {
		var v any
		if !decodeJSONValue(cand, &v) {
			continue
		}
		calls := callsFromValue(v, opts, "")
		if len(calls) == 0 {
			continue
		}
		// A prose-prefix candidate runs to the end of the text; keep only the
		// payload itself so prose after the JSON survives in remaining.
		if full, ok := jsonPrefix(cand); ok && len(full) < len(cand) {
			cand = full
		}
		remaining := strings.ReplaceAll(text, cand, "")
		remaining = strings.ReplaceAll(remaining, "```json", "")
		remaining = strings.ReplaceAll(remaining, "```", "")
		return strings.TrimSpace(remaining), calls, true
	}
	return text, nil, false
}

// jsonCandidates collects the substrings that could hold a tool-call payload:
// the trimmed text itself, the suffix after a prose prefix, and fenced blocks.
func jsonCandidates(text string) []string {
	trimmed := strings.TrimSpace(text)
	var out []string
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		out = append(out, trimmed)
	} else if i := strings.Index(trimmed, `{"tool_calls"`); i > 0 {
		out = append(out, strings.TrimSpace(trimmed[i:]))
	} else if i := firstJSONStart(trimmed); i > 0 {
		out = append(out, strings.TrimSpace(trimmed[i:]))
	}
	if strings.Contains(trimmed, "```json") {
		for _, part := range strings.Split(trimmed, "```json")[1:] {
			out = appendCandidate(out, part)
		}
	} else if strings.Contains(trimmed, "```") {
		for _, part := range strings.Split(trimmed, "```")[1:] {
			out = appendCandidate(out, part)
		}
	}
	return out
}

func appendCandidate(out []string, part string) []string {
	code := strings.TrimSpace(firstBlock(part))
	if strings.HasPrefix(code, "{") || strings.HasPrefix(code, "[") {
		return append(out, code)
	}
	return out
}

func firstJSONStart(s string) int { return strings.IndexAny(s, "{[") }

func lastJSONEnd(s string) int {
	i := strings.LastIndexByte(s, '}')
	if j := strings.LastIndexByte(s, ']'); j > i {
		i = j
	}
	return i
}

// decodeJSONValue unmarshals raw, falling back through the repair ladder:
// unquoted keys / invalid backslashes, suffix balancing, the first complete
// JSON value, then the force-close rebuild.
func decodeJSONValue(raw string, out *any) bool {
	try := func(s string) bool {
		if s == "" {
			return false
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			*out = v
			return true
		}
		return false
	}
	if try(raw) {
		return true
	}
	if fixed, ok := repairJSON(raw); ok && try(fixed) {
		return true
	}
	// The candidate may carry prose or tag debris around the payload: retry
	// the repairs on the bracketed JSON extent only.
	if i := firstJSONStart(raw); i >= 0 {
		if j := lastJSONEnd(raw); j > i {
			sub := raw[i : j+1]
			if fixed, ok := repairJSON(sub); ok && try(fixed) {
				return true
			}
			if balanced := balanceJSON(sub); balanced != sub && try(balanced) {
				return true
			}
		}
	}
	if balanced := balanceJSON(raw); balanced != raw && try(balanced) {
		return true
	}
	if prefix, ok := jsonPrefix(raw); ok && try(prefix) {
		return true
	}
	if closed, ok := forceCloseJSON(raw); ok && try(closed) {
		return true
	}
	return false
}

// callsFromValue maps a decoded JSON value onto tool calls. A {"tool_calls":
// …} shell, a bare array or a bare object are all accepted.
func callsFromValue(v any, opts ParseOptions, fallbackName string) []ToolCall {
	switch t := v.(type) {
	case map[string]any:
		if shell, ok := t["tool_calls"]; ok {
			return callsFromCollection(shell, opts, fallbackName)
		}
		return normalizeAndMap([]any{t}, opts, fallbackName)
	case []any:
		return normalizeAndMap(t, opts, fallbackName)
	}
	return nil
}

func callsFromCollection(v any, opts ParseOptions, fallbackName string) []ToolCall {
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			return nil
		}
		return normalizeAndMap(t, opts, fallbackName)
	case map[string]any:
		return normalizeAndMap([]any{t}, opts, fallbackName)
	}
	return nil
}

// normalizeAndMap requires every item to map onto a valid call: a single bad
// entry rejects the whole collection (a name-less call must never reach the
// client, where it only fails at execution time).
func normalizeAndMap(items []any, opts ParseOptions, fallbackName string) []ToolCall {
	calls := make([]ToolCall, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && fallbackName != "" && !hasNameish(m) {
			m["name"] = fallbackName
		}
		c, ok := callFromItem(it, opts)
		if !ok {
			return nil
		}
		calls = append(calls, c)
	}
	return calls
}

func hasNameish(m map[string]any) bool {
	if _, ok := m["function"]; ok {
		return true
	}
	for _, k := range nameKeys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

var (
	nameKeys = []string{"name", "tool", "tool_name", "function_name", "fn"}
	argKeys  = []string{"arguments", "params", "parameters", "args", "input", "function_input"}
	metaKeys = map[string]bool{
		"id": true, "type": true, "index": true, "function": true,
		"name": true, "tool": true, "tool_name": true, "function_name": true, "fn": true,
		"arguments": true, "params": true, "parameters": true, "args": true,
		"input": true, "function_input": true, "call_id": true,
	}
)

// callFromItem maps one tool-call object onto a ToolCall, applying the field
// alias, name/arguments swap and spilled-parameter repairs.
func callFromItem(item any, opts ParseOptions) (ToolCall, bool) {
	obj, ok := item.(map[string]any)
	if !ok {
		return ToolCall{}, false
	}
	name, args, ok := extractNameArgs(obj, opts)
	if !ok || name == "" {
		return ToolCall{}, false
	}
	argText, ok := encodeArguments(args)
	if !ok {
		return ToolCall{}, false
	}
	id := stringValue(obj["id"])
	if id == "" {
		id = "call_" + randomHex(8)
	}
	typ := stringValue(obj["type"])
	if typ == "" {
		typ = "function"
	}
	return ToolCall{
		ID:       id,
		Type:     typ,
		Function: FunctionCall{Name: name, Arguments: argText},
	}, true
}

func extractNameArgs(obj map[string]any, opts ParseOptions) (string, any, bool) {
	// Nested function object: {"function":{"name":…,"arguments":…}}.
	if fn, ok := obj["function"].(map[string]any); ok {
		name := firstString(fn, "name")
		if name == "" {
			name = firstString(obj, nameKeys...)
		}
		if name == "" {
			return "", nil, false
		}
		if args, ok := lookupArg(fn); ok {
			return name, args, true
		}
		if args, ok := lookupArg(obj); ok {
			return name, args, true
		}
		if spilled := spilledArgs(obj); len(spilled) > 0 {
			return name, spilled, true
		}
		return name, map[string]any{}, true
	}
	// function as a plain string alias: {"function":"get_weather","params":{…}}.
	if fn, ok := obj["function"].(string); ok && fn != "" {
		if args, ok := lookupArg(obj); ok {
			return fn, args, true
		}
		return fn, spilledArgs(obj), true
	}
	// Swapped: name holds the parameter object, arguments holds the name.
	if nameObj, ok := obj["name"].(map[string]any); ok {
		if s, ok := obj["arguments"].(string); ok && s != "" {
			return s, nameObj, true
		}
	}
	name := firstString(obj, nameKeys...)
	if name == "" {
		return "", nil, false
	}
	if args, ok := lookupArg(obj); ok {
		return name, args, true
	}
	// Parameters spilled to the top level: every non-meta key is an argument.
	if spilled := spilledArgs(obj); len(spilled) > 0 {
		if len(opts.ToolNames) == 0 || containsString(opts.ToolNames, name) {
			return name, spilled, true
		}
		// The offered tool list is known and the name is not one of them:
		// this is ordinary JSON, not a tool call.
		return "", nil, false
	}
	return name, map[string]any{}, true
}

func lookupArg(obj map[string]any) (any, bool) {
	for _, k := range argKeys {
		if v, ok := obj[k]; ok {
			return v, true
		}
	}
	return nil, false
}

func spilledArgs(obj map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range obj {
		if metaKeys[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// encodeArguments renders the arguments value: a string is healed into valid
// JSON, anything else is marshalled as-is.
func encodeArguments(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "{}", true
	case string:
		return healArgumentString(t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return "", false
		}
		return string(raw), true
	}
}

func healArgumentString(s string) (string, bool) {
	s = EscapeControlChars(s)
	if json.Valid([]byte(s)) {
		return s, true
	}
	if fixed, ok := repairJSON(s); ok {
		return fixed, true
	}
	if balanced := balanceJSON(s); json.Valid([]byte(balanced)) {
		return balanced, true
	}
	if prefix, ok := jsonPrefix(s); ok {
		return prefix, true
	}
	if closed, ok := forceCloseJSON(s); ok {
		return closed, true
	}
	return "", false
}

func firstString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Tagged / XML path
// ---------------------------------------------------------------------------

func parseTaggedToolCalls(text string, opts ParseOptions) (string, []ToolCall, bool) {
	var calls []ToolCall
	var remaining strings.Builder
	pos, last := 0, 0
	for pos < len(text) {
		start, startLen, startTag, ok := findStartTag(text[pos:], opts)
		if !ok {
			break
		}
		abs := pos + start
		if isInsideCodeFence(text, abs) {
			break
		}
		after := abs + startLen
		innerEnd, end := len(text), len(text)
		if p, l, found := findEndTag(text, after, opts, startTag); found && p >= after {
			innerEnd, end = p, p+l
		}
		got := extractTaggedCalls(text[after:innerEnd], opts)
		if len(got) > 0 {
			remaining.WriteString(text[last:abs])
			calls = append(calls, got...)
			last, pos = end, end
			continue
		}
		pos = after
		if pos <= abs {
			pos = abs + 1
		}
	}
	if len(calls) == 0 {
		return text, nil, false
	}
	remaining.WriteString(text[last:])
	return strings.TrimSpace(remaining.String()), calls, true
}

// extractTaggedCalls parses the body between a tag pair: a wrapped JSON array
// or object first, then the XML dialects.
func extractTaggedCalls(inner string, opts ParseOptions) []ToolCall {
	fallbackName := extractXMLName(inner)
	// 以 <invoke 开头的块是 XML 方言：先按标签解析，否则参数内容里恰好
	// 形如 [{"name": …}] 的片段（例如正被写入的 JSON 文件）会被 JSON
	// 路径抢走，产出一个假调用、丢掉真实调用。
	if strings.HasPrefix(strings.ToLower(strings.TrimLeft(inner, " \t\r\n")), "<invoke") {
		if calls := parseXMLInvokeCalls(inner, opts); len(calls) > 0 {
			return calls
		}
	}
	if calls := parseJSONInText(inner, opts, fallbackName); len(calls) > 0 {
		return calls
	}
	if calls := parseXMLInvokeCalls(inner, opts); len(calls) > 0 {
		return calls
	}
	return parseXMLFunctionCalls(inner, opts)
}

// parseJSONInText pulls the first JSON array/object out of inner and maps it,
// injecting fallbackName into items that carry arguments but no name (the
// mixed <tool_calls><function><name>…</name></function>[{"arguments":…}]
// shape).
func parseJSONInText(inner string, opts ParseOptions, fallbackName string) []ToolCall {
	if i := strings.IndexByte(inner, '['); i >= 0 {
		if j := strings.LastIndexByte(inner, ']'); j > i {
			raw := inner[i : j+1]
			var v any
			if decodeJSONValue(raw, &v) {
				return callsFromCollection(v, opts, fallbackName)
			}
		}
	}
	if i := strings.IndexByte(inner, '{'); i >= 0 {
		if j := strings.LastIndexByte(inner, '}'); j > i {
			raw := inner[i : j+1]
			var v any
			if decodeJSONValue(raw, &v) {
				return callsFromValue(v, opts, fallbackName)
			}
		}
	}
	return nil
}

func parseXMLInvokeCalls(inner string, opts ParseOptions) []ToolCall {
	var calls []ToolCall
	lower := strings.ToLower(inner)
	pos := 0
	for {
		i := strings.Index(lower[pos:], "<invoke")
		if i < 0 {
			return calls
		}
		abs := pos + i
		gt := strings.IndexByte(inner[abs:], '>')
		if gt < 0 {
			return calls
		}
		head := inner[abs : abs+gt+1]
		bodyStart := abs + gt + 1
		ci := strings.Index(lower[bodyStart:], "</invoke>")
		// 截断兜底：闭标签缺失时把剩余文本整体当作 body，最后一个
		// parameter 的值由 parseXMLParams 取到末尾。
		bodyEnd := len(inner)
		if ci >= 0 {
			bodyEnd = bodyStart + ci
		}
		body := inner[abs:bodyEnd]
		name := xmlAttr(head, "name")
		if name == "" {
			name = xmlTagValue(body, "name")
		}
		if name != "" {
			calls = append(calls, makeXMLCall(name, parseXMLParams(body)))
		}
		if ci < 0 {
			return calls
		}
		pos = bodyStart + ci + len("</invoke>")
	}
}

func parseXMLFunctionCalls(inner string, opts ParseOptions) []ToolCall {
	var calls []ToolCall
	lower := strings.ToLower(inner)
	pos := 0
	for {
		i := strings.Index(lower[pos:], "<function")
		if i < 0 {
			return calls
		}
		abs := pos + i
		gt := strings.IndexByte(inner[abs:], '>')
		if gt < 0 {
			return calls
		}
		head := inner[abs : abs+gt+1]
		selfClose := strings.HasSuffix(strings.TrimSpace(head), "/>")
		bodyStart := abs + gt + 1
		bodyEnd, next := len(inner), len(inner)
		if !selfClose {
			if ci := strings.Index(lower[bodyStart:], "</function>"); ci >= 0 {
				bodyEnd = bodyStart + ci
				next = bodyEnd + len("</function>")
			}
		}
		body := inner[bodyStart:bodyEnd]
		name := xmlAttr(head, "name")
		if name == "" {
			name = xmlTagValue(body, "name")
		}
		if name != "" {
			calls = append(calls, makeXMLCall(name, parseXMLParams(body)))
		}
		if next >= len(inner) {
			return calls
		}
		pos = next
	}
}

func makeXMLCall(name string, params map[string]any) ToolCall {
	raw, err := json.Marshal(params)
	if err != nil || len(raw) == 0 {
		raw = []byte("{}")
	}
	return ToolCall{
		ID:       "call_" + randomHex(8),
		Type:     "function",
		Function: FunctionCall{Name: name, Arguments: string(raw)},
	}
}

// parseXMLParams collects <parameter name="k">v</parameter> / <param name="k">
// values, falling back to the JSON inside <arguments>.
func parseXMLParams(body string) map[string]any {
	params := map[string]any{}
	lower := strings.ToLower(body)
	for _, tag := range []string{"parameter", "param"} {
		open := "<" + tag
		closeTag := "</" + tag + ">"
		pos := 0
		for {
			i := strings.Index(lower[pos:], open)
			if i < 0 {
				break
			}
			abs := pos + i
			// `<param` 是 `<parameter` 的前缀：命中更长标签时跳过，交给
			// 另一轮处理，否则会把 parameter 的值当成本轮截断尾部覆盖。
			if abs+len(open) < len(lower) && isTagNameChar(lower[abs+len(open)]) {
				pos = abs + len(open)
				continue
			}
			gt := strings.IndexByte(body[abs:], '>')
			if gt < 0 {
				break
			}
			head := body[abs : abs+gt+1]
			bodyStart := abs + gt + 1
			if strings.HasSuffix(strings.TrimSpace(head), "/>") {
				pos = bodyStart
				continue
			}
			name := xmlAttr(head, "name")
			ci := strings.Index(lower[bodyStart:], closeTag)
			// 截断兜底：闭标签缺失时，值取到下一个同名开标签或 body 末尾。
			end := len(body)
			if ci >= 0 {
				end = bodyStart + ci
			} else if j := strings.Index(lower[bodyStart:], open); j >= 0 {
				end = bodyStart + j
			}
			if name != "" {
				params[name] = coerceXMLValue(body[bodyStart:end])
			}
			if ci < 0 {
				pos = end
				continue
			}
			pos = end + len(closeTag)
		}
	}
	if len(params) == 0 {
		if args := xmlTagValue(body, "arguments"); args != "" {
			var v any
			if decodeJSONValue(strings.TrimSpace(args), &v) {
				if m, ok := v.(map[string]any); ok {
					return m
				}
			}
		}
	}
	return params
}

func isTagNameChar(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func coerceXMLValue(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	var v any
	if json.Unmarshal([]byte(t), &v) == nil {
		return v
	}
	return s
}

func extractXMLName(s string) string {
	if v := xmlTagValue(s, "name"); v != "" {
		return v
	}
	if i := strings.Index(strings.ToLower(s), "<function"); i >= 0 {
		if v := xmlAttr(s[i:], "name"); v != "" {
			return v
		}
	}
	if i := strings.Index(strings.ToLower(s), "<invoke"); i >= 0 {
		if v := xmlAttr(s[i:], "name"); v != "" {
			return v
		}
	}
	return ""
}

// xmlTagValue returns the trimmed body of the first <tag>…</tag> pair.
func xmlTagValue(s, tag string) string {
	lower := strings.ToLower(s)
	open := "<" + strings.ToLower(tag)
	i := strings.Index(lower, open)
	if i < 0 {
		return ""
	}
	gt := strings.IndexByte(s[i:], '>')
	if gt < 0 {
		return ""
	}
	bodyStart := i + gt + 1
	closeTag := "</" + strings.ToLower(tag) + ">"
	ci := strings.Index(lower[bodyStart:], closeTag)
	if ci < 0 {
		return ""
	}
	return strings.TrimSpace(s[bodyStart : bodyStart+ci])
}

// xmlAttr returns the value of attr="…" / attr='…' inside the first tag of s.
func xmlAttr(s, attr string) string {
	lower := strings.ToLower(s)
	key := strings.ToLower(attr)
	i := strings.Index(lower, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(attr):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if rest == "" || rest[0] != '=' {
		return ""
	}
	rest = strings.TrimLeft(rest[1:], " \t\r\n")
	if rest == "" {
		return ""
	}
	quote := rest[0]
	if quote != '"' && quote != '\'' {
		return ""
	}
	end := strings.IndexByte(rest[1:], quote)
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
}

func findStartTag(text string, opts ParseOptions) (int, int, string, bool) {
	for _, tag := range builtinStartTags {
		if p, l, ok := findTagFuzzy(text, tag); ok {
			return p, l, tag, true
		}
	}
	for _, tag := range opts.ExtraStarts {
		if strings.TrimSpace(tag) == "" {
			continue
		}
		if p, l, ok := findTagFuzzy(text, tag); ok {
			return p, l, tag, true
		}
	}
	return 0, 0, "", false
}

func findEndTag(s string, from int, opts ParseOptions, startTag string) (int, int, bool) {
	if from > len(s) {
		from = len(s)
	}
	search := s[from:]
	if open := strings.TrimRight(startTag, ">"); strings.HasPrefix(open, "<") && !strings.HasPrefix(open, "</") {
		if p, l, ok := findTagFuzzy(search, "</"+open[1:]+">"); ok {
			return from + p, l, true
		}
	}
	for _, tag := range builtinEndTags {
		if p, l, ok := findTagFuzzy(search, tag); ok {
			return from + p, l, true
		}
	}
	for _, tag := range opts.ExtraEnds {
		if strings.TrimSpace(tag) == "" {
			continue
		}
		if p, l, ok := findTagFuzzy(search, tag); ok {
			return from + p, l, true
		}
	}
	// A following start tag also terminates the block when the model forgot
	// the end marker between two calls.
	if p, l, ok := findTagFuzzy(search, startTag); ok {
		return from + p, l, true
	}
	for _, tag := range builtinStartTags {
		if p, l, ok := findTagFuzzy(search, tag); ok {
			return from + p, l, true
		}
	}
	return 0, 0, false
}

// findTagFuzzy locates tag (with or without the trailing '>') in s, first by
// exact match then by the fullwidth/underscore-normalized fuzzy match.
func findTagFuzzy(s, tag string) (int, int, bool) {
	partial := strings.TrimRight(tag, ">")
	if partial == "" {
		return 0, 0, false
	}
	if i := strings.Index(s, partial); i >= 0 {
		l := len(partial)
		if i+l < len(s) && s[i+l] == '>' {
			l++
		}
		return i, l, true
	}
	if p, l, ok := fuzzyMatchTag(s, partial); ok {
		if p+l < len(s) && s[p+l] == '>' {
			l++
		}
		return p, l, true
	}
	return 0, 0, false
}

// normTagChar normalizes the lookalike separators: '｜'(U+FF5C) → '|',
// '▁'(U+2581) → '_'.
func normTagChar(r rune) rune {
	switch r {
	case '\uFF5C':
		return '|'
	case '\u2581':
		return '_'
	}
	return r
}

func eqTagChar(a, b rune) bool {
	return a == b || normTagChar(a) == normTagChar(b)
}

func fuzzyMatchTag(haystack, partial string) (int, int, bool) {
	n := []rune(partial)
	h := []rune(haystack)
	if len(n) == 0 || len(h) < len(n) {
		return 0, 0, false
	}
	for start := 0; start+len(n) <= len(h); start++ {
		matched := true
		for j := 0; j < len(n); j++ {
			if !eqTagChar(n[j], h[start+j]) {
				matched = false
				break
			}
		}
		if matched {
			bpos, blen := 0, 0
			for _, r := range h[:start] {
				bpos += utf8.RuneLen(r)
			}
			for _, r := range h[start : start+len(n)] {
				blen += utf8.RuneLen(r)
			}
			return bpos, blen, true
		}
	}
	return 0, 0, false
}

func isInsideCodeFence(s string, pos int) bool {
	if pos < 0 {
		return false
	}
	if pos > len(s) {
		pos = len(s)
	}
	return strings.Count(s[:pos], "```")%2 == 1
}

// repairJSON applies the two reference repairs — invalid backslashes then
// unquoted keys — and reports whether the result is valid JSON.
func repairJSON(s string) (string, bool) {
	step1 := repairInvalidBackslashes(s)
	if json.Valid([]byte(step1)) {
		return step1, true
	}
	step2 := repairUnquotedKeys(step1)
	if json.Valid([]byte(step2)) {
		return step2, true
	}
	return "", false
}

func repairInvalidBackslashes(s string) string {
	runes := []rune(s)
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c != '\\' {
			out.WriteRune(c)
			continue
		}
		if i+1 < len(runes) {
			n := runes[i+1]
			if strings.ContainsRune(`"\/bfnrtu`, n) {
				out.WriteRune('\\')
				out.WriteRune(n)
			} else {
				out.WriteString(`\\`)
				out.WriteRune(n)
			}
			i++
			continue
		}
		out.WriteRune('\\')
	}
	return out.String()
}

func repairUnquotedKeys(s string) string {
	runes := []rune(s)
	var out strings.Builder
	out.Grow(len(s) + 32)
	i := 0
	for i < len(runes) {
		out.WriteRune(runes[i])
		if (runes[i] == '{' || runes[i] == ',') && i+1 < len(runes) {
			i++
			for i < len(runes) && unicode.IsSpace(runes[i]) {
				out.WriteRune(runes[i])
				i++
			}
			if i < len(runes) && (unicode.IsLetter(runes[i]) || runes[i] == '_') {
				keyStart := i
				for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) || runes[i] == '_') {
					i++
				}
				if i < len(runes) && runes[i] == ':' {
					out.WriteRune('"')
					out.WriteString(string(runes[keyStart:i]))
					out.WriteRune('"')
				} else {
					out.WriteString(string(runes[keyStart:i]))
					continue
				}
			}
			continue
		}
		i++
	}
	return out.String()
}

// jsonPrefix returns the first complete JSON value in s, ignoring any
// trailing garbage after it.
func jsonPrefix(s string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

func firstBlock(s string) string {
	if i := strings.Index(s, "```"); i >= 0 {
		return s[:i]
	}
	return s
}

// balanceJSON repairs an arguments string the model closed prematurely: a
// trailing unterminated string, a dangling comma/colon or unclosed braces
// (the channel cap sometimes lets a reply stop right before its inner JSON
// closes). It returns the original text untouched when the damage is not a
// simple missing-suffix problem.
func balanceJSON(s string) string {
	if json.Valid([]byte(s)) {
		return s
	}
	var stack []byte
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			switch c {
			case '"':
				inString = true
			case '{', '[':
				stack = append(stack, c)
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '"':
			inString = false
		}
	}

	out := s
	// A dangling escape (…\) is a partial escape sequence: drop it.
	if inString && escaped {
		out = out[:len(out)-1]
		escaped = false
	}
	if !inString {
		out = strings.TrimRight(out, " \t\r\n")
		addedValue := false
		for len(out) > 0 {
			last := out[len(out)-1]
			if last == ',' || last == ':' {
				if last == ':' {
					addedValue = true
				}
				out = strings.TrimRight(out[:len(out)-1], " \t\r\n")
				continue
			}
			break
		}
		var trimmed strings.Builder
		trimmed.WriteString(out)
		if addedValue {
			trimmed.WriteString(": \"\"")
		}
		out = trimmed.String()
	}
	var b strings.Builder
	b.Grow(len(out) + len(stack) + 4)
	b.WriteString(out)
	if inString {
		b.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
	if json.Valid([]byte(b.String())) {
		return b.String()
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// n*2 hex chars, same length as the happy path — and unlike
		// "00000000"[:n] this cannot slice past the end for n > 8.
		return strings.Repeat("0", n*2)
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}
