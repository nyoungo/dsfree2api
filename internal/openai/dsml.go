package openai

import (
	"regexp"
	"strings"
)

// DeepSeek 模型偶尔会退回自己的 DSML 标记方言输出工具调用，形如：
//
//	<｜｜DSML｜｜ calls>
//	<｜｜DSML｜｜ invoke name="write">
//	<｜｜DSML｜｜ parameter name="filePath" string="true">a.html</｜｜DSML｜｜ parameter>
//	</｜｜DSML｜｜ invoke>
//	</｜｜DSML｜｜ calls>
//
// 分隔竖线可能全角(U+FF5C)也可能半角('|')、个数不固定（还有
// <|DSML|> calls> 这种写法），且截断时闭标签整段缺失。解析策略是先把
// DSML 标签改写成普通 XML 标签，再复用既有的 tagged 路径。
//
// 组1=开/闭前缀（< 或 </），组2=标签名；标记之后的属性与 '>' 不消费，
// 保证 invoke name="…" 的属性原样保留。
var dsmlTagRe = regexp.MustCompile(`(?i)(</?)\s*[｜|]{1,4}DSML[｜|]{1,4}>?\s*([a-z_]\w*)`)

// normalizeDSML 把 DSML 方言标签改写成普通 XML 标签：竖线标记整体去掉，
// 标签名统一小写，calls 归并为已知起点标签 tool_calls。返回 (文本, 是否
// 确有标签被改写)；不含 DSML 标记时原样返回，避免每轮解析都跑正则。
func normalizeDSML(text string) (string, bool) {
	if !strings.Contains(strings.ToLower(text), "dsml") {
		return text, false
	}
	out := dsmlTagRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := dsmlTagRe.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		name := strings.ToLower(sub[2])
		if name == "calls" {
			name = "tool_calls"
		}
		return sub[1] + name
	})
	return out, out != text
}

// ensureTaggedStart 给归一化后缺少已知起点标签的裸 invoke/function 块补一个
// <tool_calls> 起点：findStartTag 只认内置/自定义起点标签，DSML 若没写
// calls 外层，不补起点就进不了 tagged 解析路径。
func ensureTaggedStart(text string, opts ParseOptions) string {
	lower := strings.ToLower(text)
	for _, tag := range []string{"<invoke", "<function"} {
		i := strings.Index(lower, tag)
		if i < 0 {
			continue
		}
		if _, _, _, ok := findStartTag(text[:i], opts); ok {
			return text // 起点已在该块之前
		}
		return text[:i] + "<tool_calls>" + text[i:]
	}
	return text
}

// dsmlTruncated 报告 DSML 方言的工具调用块是否被通道输出上限截断：开标签
// 多于闭标签即视为未闭合。只认 DSML 方言本身，避免普通散文里的 XML 示例
// 误触发续写轮次。
func dsmlTruncated(text string) bool {
	norm, ok := normalizeDSML(text)
	if !ok {
		return false
	}
	// 代码围栏里的标记是文档示例，解析层会跳过，同样不触发续写轮次。
	start := -1
	for _, tag := range []string{"<tool_calls>", "<invoke"} {
		if i := strings.Index(norm, tag); i >= 0 && (start < 0 || i < start) {
			start = i
		}
	}
	if start >= 0 && isInsideCodeFence(norm, start) {
		return false
	}
	if strings.Count(norm, "<invoke") > strings.Count(norm, "</invoke>") {
		return true
	}
	return strings.Count(norm, "<tool_calls>") > strings.Count(norm, "</tool_calls>")
}

// preferDSMLContinue 报告续写片段应按 DSML/XML 方言组织而非 JSON：文本里
// 有 DSML 标记且没有 JSON 工具载荷（有载荷时截断的是 JSON，仍走 JSON 指令）。
func preferDSMLContinue(partial string) bool {
	if strings.Contains(partial, `{"tool_calls"`) {
		return false
	}
	_, ok := normalizeDSML(partial)
	return ok
}
