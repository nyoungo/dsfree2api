package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ContinuationContext returns a two-sided excerpt of partial: its first head
// runes keep the surrounding {"tool_calls": …} structure visible so the model
// never forgets it is mid-JSON, its last tail runes carry the resume point.
// Continuation turns embed only this excerpt: full replies grow past 20KB
// across rounds and the site degrades on huge prompts.
func ContinuationContext(partial string, head, tail int) string {
	runes := []rune(partial)
	if len(runes) <= head+tail {
		return partial
	}
	mid := len(runes) - head - tail
	return fmt.Sprintf("%s\n…（中间省略 %d 字符：以上是被截断回复的开头，以下是最新的末尾）…\n%s",
		string(runes[:head]), mid, string(runes[len(runes)-tail:]))
}

// ContinueInstruction builds the follow-up user turn that asks the model to
// resume a reply the channel cut off at its per-reply output cap. The site
// caps each generation (~1k tokens), so large tool-call JSON arrives in
// pieces; the model must continue byte-for-byte instead of restarting.
func ContinueInstruction(partial string, round, maxRounds int) string {
	if preferDSMLContinue(partial) {
		return dsmlContinueInstruction(partial, round, maxRounds)
	}
	tail := partial
	if n := len(tail); n > 80 {
		tail = tail[n-80:]
	}
	deadline := fmt.Sprintf("这是第 %d/%d 次续写。", round, maxRounds)
	if round >= maxRounds {
		deadline += "这是最后一次续写机会：本轮必须先收束剩余内容（补齐未完成的必要结构），随后立即写收尾 8 字符序列闭合并结束回复，禁止再展开任何新段落。"
	} else {
		deadline += "每轮只能输出约 2300 字符；若主体已完成或接近完成，请立即闭合 JSON 收尾，不要无限扩展。"
	}
	return fmt.Sprintf(
		"上一条 assistant 回复因通道输出上限被截断（上面保留了它的开头与最新末尾，中间已省略），%s目前已输出约 %d 个字符，最后 80 个字符是：\n%s\n\n"+
			"请只输出【续写片段】：从上面末尾片段的最后一个字符之后开始逐字符接着写，直到整个 JSON 完整闭合。"+
			"严格禁止：重复任何已输出内容、以 { 开头、重新生成 tool_calls 结构、任何解释或前后缀。"+
			"JSON 字符串内的换行必须写成两个字符的 \\n 转义序列、制表符写成 \\t，禁止输出真实的换行、制表或控制字符。"+
			"完成文件内容后必须立即闭合 JSON：依次写入 content 字符串的收尾引号、内层对象的 }、arguments 字符串的收尾引号、以及外层全部 } 与 ]，"+
			"文件写完后的收尾 8 个字符必须依次是：反斜杠、双引号、右花括号、双引号、右花括号、右花括号、右方括号、右花括号（即 \\\"}\"}}]}），一个都不能少，"+
			"然后立刻结束回复；禁止输出任何解释、Markdown 标记、使用说明、动画特点介绍或后记文字。"+
			"注意合理收尾：文件内容已经很长，若主体已经完成就立即闭合 JSON 结束，不要无限扩展新段落。"+
			"你的回复的第一个字符必须正好是被截断处的下一个字符。",
		deadline, utf8.RuneCountInString(partial), tail)
}

// dsmlContinueInstruction 是 DSML 方言的续写指令：要求模型按原标记格式
// 逐字符续写并补齐缺失的闭合标签。XML 内容里的换行是合法字符，因此不套用
// JSON 分支的“换行必须写成 \n 转义”规则。
func dsmlContinueInstruction(partial string, round, maxRounds int) string {
	tail := partial
	if n := len(tail); n > 80 {
		tail = tail[n-80:]
	}
	deadline := fmt.Sprintf("这是第 %d/%d 次续写。", round, maxRounds)
	if round >= maxRounds {
		deadline += "这是最后一次续写机会：本轮必须先收束剩余内容（补齐未完成的标记），随后立即闭合全部标签并结束回复，禁止再展开任何新段落。"
	} else {
		deadline += "每轮只能输出约 2300 字符；若主体已完成或接近完成，请立即补齐缺失的闭合标签收尾，不要无限扩展。"
	}
	return fmt.Sprintf(
		"上一条 assistant 回复因通道输出上限被截断（上面保留了它的开头与最新末尾，中间已省略），%s目前已输出约 %d 个字符，最后 80 个字符是：\n%s\n\n"+
			"请只输出【续写片段】：从上面末尾片段的最后一个字符之后开始逐字符接着写，直到整个工具调用标记完整闭合。"+
			"严格禁止：重复任何已输出内容、以 < 或 { 开头、重新生成整段工具调用结构、任何解释或前后缀。"+
			"参数内容写完后必须立即按原格式依次补齐缺失的闭合标签（parameter、invoke 及外层 calls），然后立刻结束回复；"+
			"参数内容中的换行、制表符按原样输出即可，无需转义。"+
			"禁止输出任何解释、Markdown 标记或后记文字。"+
			"注意合理收尾：内容已经很长，若主体已经完成就立即闭合标签结束，不要无限扩展新段落。"+
			"你的回复的第一个字符必须正好是被截断处的下一个字符。",
		deadline, utf8.RuneCountInString(partial), tail)
}

// ToolCallTruncated reports whether text looks like a forced tool-call JSON
// reply (see toolsPrompt) that the channel cut off before it could close.
// Fenced ```json blocks are unwrapped first, mirroring TryParseToolCalls.
// Text without a JSON payload falls back to the DSML dialect check.
func ToolCallTruncated(text string) bool {
	cand := strings.TrimSpace(text)
	if !strings.HasPrefix(cand, "{") {
		if strings.Contains(cand, "```json") {
			cand = strings.TrimSpace(firstBlock(strings.Split(cand, "```json")[1]))
		} else if strings.Contains(cand, "```") {
			cand = strings.TrimSpace(firstBlock(strings.Split(cand, "```")[1]))
		}
	}
	if !strings.HasPrefix(cand, `{"tool_calls"`) {
		// The model sometimes opens with a sentence before the tool JSON:
		// evaluate the payload itself, mirroring TryParseToolCalls.
		i := strings.Index(cand, `{"tool_calls"`)
		if i <= 0 {
			// 没有 JSON 载荷：再看是不是被截断的 DSML 方言标记。
			return dsmlTruncated(text)
		}
		cand = cand[i:]
	}
	return !json.Valid([]byte(cand))
}

// EscapeControlChars rewrites raw control characters that appear inside JSON
// string literals as \n / \r / \t escapes. Whitespace outside string literals
// (pretty-printed structure) is left untouched. It equals
// EscapeControlsAfter("", s) and suits complete documents.
func EscapeControlChars(s string) string {
	return EscapeControlsAfter("", s)
}

// EscapeControlsAfter escapes control characters in s while assuming s
// continues prefix inside the same JSON document: the string-literal state at
// the end of prefix carries over, which matters because continuation turns
// resume mid-string.
func EscapeControlsAfter(prefix, s string) string {
	if !needsEscape(s) {
		return s
	}
	st := escapeState{}
	if prefix != "" {
		var throw strings.Builder
		st.feed(prefix, &throw)
	}
	var out strings.Builder
	out.Grow(len(s) + 16)
	st.feed(s, &out)
	return out.String()
}

type escapeState struct {
	inString bool
	escaped  bool
}

func (st *escapeState) feed(s string, out *strings.Builder) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !st.inString {
			if c == '"' {
				st.inString = true
			}
			out.WriteByte(c)
			continue
		}
		if st.escaped {
			st.escaped = false
			out.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			if i+1 >= len(s) {
				// dangling escape: carried into the next feed so a
				// continuation piece can complete it
				st.escaped = true
				out.WriteByte('\\')
				continue
			}
			if j := escapeEndAt(s, i); j > i {
				out.WriteString(s[i : j+1])
				i = j
				continue
			}
			// invalid escape (…\d): double the backslash so the text
			// parses — matches escapeStringBody used by forceCloseJSON,
			// keeping streamed and repaired arguments identical
			out.WriteString(`\\`)
		case '"':
			st.inString = false
			out.WriteByte(c)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(out, `\u%04x`, c)
			} else {
				out.WriteByte(c)
			}
		}
	}
}

func needsEscape(s string) bool {
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 {
			return true
		}
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			if i+1 >= len(s) {
				escaped = true
				continue
			}
			j := escapeEndAt(s, i)
			if j == i {
				return true // invalid escape — feed doubles it
			}
			i = j
		case '"':
			inString = false
		}
	}
	return false
}

// TrimOverlap drops the longest prefix of head that duplicates the tail of
// acc: continuation turns sometimes re-emit the last few characters of the
// truncated reply before carrying on.
func TrimOverlap(acc, head string) string {
	max := len(acc)
	if len(head) < max {
		max = len(head)
	}
	if max > 64 {
		max = 64
	}
	for k := max; k > 0; k-- {
		if strings.HasSuffix(acc, head[:k]) {
			return head[k:]
		}
	}
	return head
}

// SendFunc runs one upstream conversation for prompt and streams its text
// through emit. Emit errors abort the conversation.
type SendFunc func(ctx context.Context, prompt string, emit func(text string) error) error

// ContinuationFunc builds the follow-up prompt for partial. round is 1-based
// and lets the instruction tell the model how many attempts it has left.
type ContinuationFunc func(partial string, round int) string

// RunContinued sends first through send, then — while the accumulated text is
// a truncated tool-call JSON — keeps sending cont(partial) turns to resume it
// until the JSON closes or rounds run out. Every stitched piece reaches emit
// in order, so callers see one continuous reply. Continuation failures only
// warn: the caller falls back to whatever text arrived, exactly as before.
func RunContinued(
	ctx context.Context,
	first string,
	cont ContinuationFunc,
	rounds int,
	send SendFunc,
	emit func(text string) error,
	logf func(msg string, args ...any),
	warn func(msg string, args ...any),
) error {
	var full strings.Builder
	err := send(ctx, first, func(text string) error {
		full.WriteString(text)
		return emit(text)
	})
	if err != nil {
		return err
	}
	if cont == nil || rounds <= 0 {
		return nil
	}
	for round := 0; round < rounds && ToolCallTruncated(full.String()); round++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		partial := full.String()
		next := cont(partial, round+1)
		var seg strings.Builder
		var sendErr error
		for attempt := 0; attempt < 3; attempt++ {
			seg.Reset()
			sendErr = send(ctx, next, func(text string) error {
				seg.WriteString(text)
				return nil
			})
			if sendErr == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			warn("tool-call continuation turn retrying", "round", round+1, "attempt", attempt+1, "error", sendErr)
		}
		if sendErr != nil {
			warn("tool-call continuation turn failed", "round", round+1, "prompt_chars", len(next), "error", sendErr)
			break
		}
		raw := seg.String()
		// DSML/XML 内容里的换行是合法字符，不能套用 JSON 字符串的转义规则。
		healed := raw
		if !preferDSMLContinue(partial) {
			healed = EscapeControlsAfter(partial, raw)
		}
		piece := TrimOverlap(partial, healed)
		if piece == "" {
			warn("tool-call continuation produced no new text", "round", round+1, "seg_chars", len(raw))
			break
		}
		full.WriteString(piece)
		logf("tool-call continuation stitched", "round", round+1, "added_chars", len(piece), "seg_chars", len(raw))
		if err := emit(piece); err != nil {
			return err
		}
	}
	if ToolCallTruncated(full.String()) {
		warn("tool-call continuation exhausted — returning truncated reply as-is")
	}
	return nil
}
