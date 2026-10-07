package openai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ToolArgDelta is one incremental tool_calls delta produced by ArgStreamer.
// Empty fields are omitted from the wire chunk.
type ToolArgDelta struct {
	Index int
	ID    string
	Name  string
	Args  string
}

// ArgStreamer turns raw tool-call JSON that grows over time into OpenAI-style
// incremental tool_calls deltas so tool requests stream instead of buffering.
//
// The raw document is re-scanned on every Feed; the decoded arguments text is
// a prefix-monotonic function of the document, so each delta is exactly the
// new suffix. Finalize reconciles the stream with the repaired parse result
// (balanceJSON / EscapeControlChars / forceCloseJSON) and reports any call
// that could not be streamed.
type ArgStreamer struct {
	sent     map[int]string // args text already written downstream
	derived  map[int]string // latest full decode (incl. hold-back tail)
	idSent   map[int]string
	nameSent map[int]string
	seen     map[int]bool
	nostream map[int]bool // arguments delivered as object/number — resend whole
	diverge  string       // first divergence context, set by Finalize
}

// NewArgStreamer creates a streamer for one request.
func NewArgStreamer() *ArgStreamer {
	return &ArgStreamer{
		sent:     map[int]string{},
		derived:  map[int]string{},
		idSent:   map[int]string{},
		nameSent: map[int]string{},
		seen:     map[int]bool{},
		nostream: map[int]bool{},
	}
}

// Sent reports whether any delta was written downstream.
func (a *ArgStreamer) Sent() bool { return len(a.seen) > 0 }

// Diverge returns a short diagnostic of where Finalize found the repaired
// arguments diverging from the streamed text (empty when clean).
func (a *ArgStreamer) Diverge() string { return a.diverge }

func (a *ArgStreamer) noteDiverge(idx int, target, emit, sent string) {
	if a.diverge != "" {
		return
	}
	at := firstDiffIndex(target, emit)
	if d := firstDiffIndex(target, sent); d < at {
		at = d
	}
	a.diverge = fmt.Sprintf("idx=%d tLen=%d eLen=%d sLen=%d diff@%d target=%q emit=%q",
		idx, len(target), len(emit), len(sent), at, ctxWindow(target, at), ctxWindow(emit, at))
}

func firstDiffIndex(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func ctxWindow(s string, at int) string {
	const win = 60
	start := at - win
	if start < 0 {
		start = 0
	}
	end := at + win
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

type argCallState struct {
	derived  string
	id       string
	name     string
	nostream bool
}

// Feed scans the full accumulated document and returns the deltas to send.
func (a *ArgStreamer) Feed(text string) []ToolArgDelta {
	states := a.scan(text)
	var out []ToolArgDelta
	for i, st := range states {
		if st.nostream {
			a.nostream[i] = true
			continue
		}
		if a.nostream[i] {
			continue
		}
		emit := trimHold(st.derived)
		prev := a.sent[i]
		if !strings.HasPrefix(emit, prev) {
			continue // defensive: decode regressed (should be monotonic)
		}
		d := ToolArgDelta{Index: i}
		send := false
		if a.idSent[i] == "" && st.id != "" {
			d.ID = st.id
			a.idSent[i] = st.id
			send = true
		}
		if st.name != "" && a.nameSent[i] != st.name {
			d.Name = st.name
			a.nameSent[i] = st.name
			send = true
		}
		if args := emit[len(prev):]; args != "" {
			d.Args = args
			send = true
		}
		a.sent[i] = emit
		a.derived[i] = st.derived
		if send {
			a.seen[i] = true
			out = append(out, d)
		}
	}
	return out
}

// Finalize reconciles the stream against the final parsed tool calls.
// deltas must be sent before the finish chunk; resend holds calls that were
// never streamed (send them in full, with their index). diverged signals that
// the repaired arguments are not an extension of what was already streamed —
// the client keeps the streamed text in that case.
func (a *ArgStreamer) Finalize(calls []ToolCall) (deltas []ToolArgDelta, resend []ToolCall, diverged bool) {
	for i, c := range calls {
		if a.nostream[i] || !a.seen[i] {
			idx := i
			c.Index = &idx
			resend = append(resend, c)
			continue
		}
		target := c.Function.Arguments
		emit := trimHold(a.derived[i])
		sent := a.sent[i]
		d := ToolArgDelta{Index: i}
		flush := ""
		switch {
		case strings.HasPrefix(target, emit):
			// append-only repair (closers etc.): send the rest incl. held tail
			flush = target[len(sent):]
		case strings.HasPrefix(emit, target):
			// repair dropped the held tail (dangling comma/whitespace). If the
			// client has not been sent beyond the new end, flush up to it;
			// otherwise it keeps extra text and we can only report it.
			if strings.HasPrefix(target, sent) {
				flush = target[len(sent):]
			} else {
				diverged = true
				a.noteDiverge(i, target, emit, sent)
			}
		case strings.HasPrefix(target, sent):
			// repair only rewrote the unsent held region: catch the client up
			flush = target[len(sent):]
		default:
			// mid-document cut (forceCloseJSON anchor rebuild): already sent
			// text can differ from the repair — report and keep the stream.
			diverged = true
			a.noteDiverge(i, target, emit, sent)
		}
		if flush != "" {
			d.Args = flush
		}
		if a.idSent[i] == "" && c.ID != "" {
			d.ID = c.ID
		}
		if c.Function.Name != "" && a.nameSent[i] != c.Function.Name {
			d.Name = c.Function.Name
		}
		if d.Args != "" || d.ID != "" || d.Name != "" {
			deltas = append(deltas, d)
		}
	}
	return deltas, resend, diverged
}

var (
	argsKeyWord = []byte(`"arguments"`)
	fenceRe     = regexp.MustCompile("```")
	tcNameRe    = regexp.MustCompile(`"name"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	tcIDRe      = regexp.MustCompile(`"id"\s*:\s*"((?:[^"\\]|\\.)*)"`)
)

func (a *ArgStreamer) scan(text string) []argCallState {
	var states []argCallState
	if !extractableToolJSON(text) {
		return states
	}
	// Only scan the payload: with prose in front the arguments keys before
	// it must not count as tool calls. A fenced payload starts after its
	// fence; fences later than the payload live inside a string value and
	// must not move the base.
	base := 0
	if !strings.HasPrefix(strings.TrimSpace(text), "{") {
		if tc := strings.Index(text, `{"tool_calls"`); tc > 0 {
			base = tc
		} else if f := fenceRe.FindStringIndex(text); f != nil {
			base = f[1]
		}
	}
	prevEnd := base
	off := base
	for {
		j := strings.Index(text[off:], string(argsKeyWord))
		if j < 0 {
			break
		}
		pos := off + j
		off = pos + len(argsKeyWord)
		winStart := prevEnd
		if pos-winStart > 8192 {
			winStart = pos - 8192
		}
		if winStart > pos {
			winStart = pos
		}
		id, name := lookbackIDName(text[winStart:pos])
		k := skipSpace(text, pos+len(argsKeyWord))
		if k >= len(text) || text[k] != ':' {
			continue // the word without key syntax
		}
		st := argCallState{id: id, name: name}
		v := skipSpace(text, k+1)
		if v >= len(text) {
			prevEnd = len(text)
		} else {
			switch text[v] {
			case '"':
				decoded, end := decodeArgsValue(text, v+1)
				st.derived = innerEscape(decoded)
				prevEnd = end
			case '{', '[':
				st.nostream = true
				prevEnd = v + 1
			default:
				st.nostream = true
				prevEnd = v + 1
			}
		}
		states = append(states, st)
	}
	return states
}

// extractableToolJSON mirrors the candidate gate in TryParseToolCalls: the
// document must look like it could become a tool_calls payload — starting
// with the JSON, fenced, or with prose in front of the payload.
func extractableToolJSON(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "{") {
		return strings.Contains(t, `"tool_calls"`)
	}
	return strings.Contains(t, `{"tool_calls"`)
}

func skipSpace(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// decodeArgsValue decodes the outer-JSON string value starting after the
// opening quote at start. It returns the decoded inner text and the index
// after the closing quote (or len(text) when the value is still open). An
// incomplete escape pair at the end of text is withheld for the next scan.
func decodeArgsValue(text string, start int) (string, int) {
	var b strings.Builder
	i := start
	for i < len(text) {
		c := text[i]
		if c == '\\' {
			if i+1 >= len(text) {
				break
			}
			c2 := text[i+1]
			switch c2 {
			case '"', '\\', '/':
				b.WriteByte(c2)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, n := decodeUnicodeEscape(text, i)
				if n == 0 {
					if i+6 > len(text) {
						// incomplete at end: withhold for the next scan
						return b.String(), i
					}
					// malformed but complete (\uZZZZ): emit raw — the
					// inner heal doubles the backslash, like the parse path
					b.WriteString(`\u`)
					i += 2
					continue
				}
				b.WriteRune(r)
				i += n
				continue
			default:
				// invalid outer escape (…\d): keep the backslash as inner
				// text so the inner heal matches the repaired parse
				b.WriteByte('\\')
				b.WriteByte(c2)
			}
			i += 2
			continue
		}
		if c == '"' {
			return b.String(), i + 1
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), len(text)
}

// decodeUnicodeEscape decodes \uXXXX (and surrogate pairs) at text[i].
// It returns the rune and total sequence length, or (0, 0) when incomplete.
func decodeUnicodeEscape(text string, i int) (rune, int) {
	if i+6 > len(text) || text[i+1] != 'u' || !isHex4(text[i+2:i+6]) {
		return 0, 0
	}
	var r rune
	if _, err := fmt.Sscanf(text[i+2:i+6], "%x", &r); err != nil {
		return 0, 0
	}
	if r >= 0xD800 && r <= 0xDBFF {
		// high surrogate — needs a low surrogate to follow
		if i+12 <= len(text) && text[i+6] == '\\' && text[i+7] == 'u' && isHex4(text[i+8:i+12]) {
			var lo rune
			if _, err := fmt.Sscanf(text[i+8:i+12], "%x", &lo); err == nil && lo >= 0xDC00 && lo <= 0xDFFF {
				return 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00), 12
			}
		}
		return r, 6 // lone surrogate — emit as-is, clients tolerate
	}
	return r, 6
}

// innerEscape applies EscapeControlChars semantics to decoded inner text.
func innerEscape(decoded string) string {
	if !needsEscape(decoded) {
		return decoded
	}
	var st escapeState
	var out strings.Builder
	out.Grow(len(decoded) + 16)
	st.feed(decoded, &out)
	return out.String()
}

// trimHold strips the suffix that balanceJSON would trim or drop when the
// channel cap stops the reply there: whitespace, a dangling comma/colon, or a
// lone backslash. Held text is released by a later Feed or Finalize.
func trimHold(s string) string {
	i := len(s)
	for i > 0 {
		switch s[i-1] {
		case ' ', '\t', '\r', '\n', ',', ':':
			i--
		default:
			goto backslash
		}
	}
backslash:
	if i > 0 && s[i-1] == '\\' {
		run := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			run++
		}
		if run%2 == 1 {
			i--
		}
	}
	return s[:i]
}

// lookbackIDName finds the id / name of the tool call that owns the upcoming
// arguments key: the last occurrence before it within the local window.
func lookbackIDName(window string) (id, name string) {
	if m := tcIDRe.FindAllStringSubmatch(window, -1); len(m) > 0 {
		id = decodeJSONString(m[len(m)-1][1])
	}
	if m := tcNameRe.FindAllStringSubmatch(window, -1); len(m) > 0 {
		name = decodeJSONString(m[len(m)-1][1])
	}
	return id, name
}

func decodeJSONString(s string) string {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err == nil {
		return out
	}
	return s
}
