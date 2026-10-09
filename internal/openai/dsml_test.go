package openai

import (
	"context"
	"strings"
	"testing"
)

// dsmlBlock 是用户原始样本的完整形态：双全角竖线标记、calls/invoke/
// parameter 三层全部闭合。
func dsmlBlock() string {
	return `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="write">` +
		`<｜｜DSML｜｜ parameter name="filePath" string="true">tihu.html</｜｜DSML｜｜ parameter>` +
		`<｜｜DSML｜｜ parameter name="content" string="true"><html><body>hi</body></html></｜｜DSML｜｜ parameter>` +
		`</｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`
}

func TestParseDSMLInvokeCalls(t *testing.T) {
	remaining, calls, ok := TryParseToolCallsForTools(dsmlBlock(), toolsFor("write"))
	if !ok || len(calls) != 1 {
		t.Fatalf("ok=%v calls=%+v", ok, calls)
	}
	if remaining != "" {
		t.Fatalf("remaining = %q, want empty", remaining)
	}
	if calls[0].Function.Name != "write" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if got := argsField(t, calls[0], "filePath"); got != "tihu.html" {
		t.Fatalf("filePath = %q", got)
	}
	if got := argsField(t, calls[0], "content"); got != "<html><body>hi</body></html>" {
		t.Fatalf("content = %q", got)
	}
}

// 用户原始样本：截断场景 —— 最后一个 parameter 断在半截，没有任何闭标签。
func TestParseDSMLTruncatedInvoke(t *testing.T) {
	raw := `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="write">` +
		`<｜｜DSML｜｜ parameter name="filePath" string="true">tihu.html</｜｜DSML｜｜ parameter>` +
		`<｜｜DSML｜｜ parameter name="content" string="true"><html><body><canvas id="scene">stopped mid`
	_, calls, ok := TryParseToolCallsForTools(raw, toolsFor("write"))
	if !ok || len(calls) != 1 {
		t.Fatalf("ok=%v calls=%+v", ok, calls)
	}
	if calls[0].Function.Name != "write" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if got := argsField(t, calls[0], "filePath"); got != "tihu.html" {
		t.Fatalf("filePath = %q", got)
	}
	want := `<html><body><canvas id="scene">stopped mid`
	if got := argsField(t, calls[0], "content"); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// 半角单竖线写法（<|DSML|> calls>）同样要能解析。
func TestParseDSMLPipeVariant(t *testing.T) {
	raw := `<|DSML|> calls><|DSML|> invoke name="get_weather">` +
		`<|DSML|> parameter name="city">北京</|DSML|> parameter>` +
		`</|DSML|> invoke></|DSML|> calls>`
	_, calls, ok := TryParseToolCalls(raw)
	if !ok || len(calls) != 1 {
		t.Fatalf("ok=%v calls=%+v", ok, calls)
	}
	if calls[0].Function.Name != "get_weather" || argsField(t, calls[0], "city") != "北京" {
		t.Fatalf("call = %+v", calls[0])
	}
}

// 没写 calls 外层的裸 invoke 块：归一化后补 <tool_calls> 起点进 tagged 路径。
func TestParseDSMLStandaloneInvoke(t *testing.T) {
	raw := `<｜｜DSML｜｜ invoke name="read">` +
		`<｜｜DSML｜｜ parameter name="path">/a.txt</｜｜DSML｜｜ parameter>` +
		`</｜｜DSML｜｜ invoke>`
	_, calls, ok := TryParseToolCalls(raw)
	if !ok || len(calls) != 1 {
		t.Fatalf("ok=%v calls=%+v", ok, calls)
	}
	if calls[0].Function.Name != "read" || argsField(t, calls[0], "path") != "/a.txt" {
		t.Fatalf("call = %+v", calls[0])
	}
}

// 工具调用块两侧的散文要原样保留在 remaining 里。
func TestParseDSMLKeepsProse(t *testing.T) {
	raw := "好的，这就写。\n" + dsmlBlock() + "\n写完了。"
	remaining, calls, ok := TryParseToolCalls(raw)
	if !ok || len(calls) != 1 {
		t.Fatalf("ok=%v calls=%+v", ok, calls)
	}
	if !strings.Contains(remaining, "好的") || !strings.Contains(remaining, "写完了") {
		t.Fatalf("remaining = %q", remaining)
	}
}

// 参数内容里形如 [{"name": …}] 的片段是被写入的文件数据，不能被 JSON
// 候选路径抢成假调用。
func TestParseDSMLContentJSONNotHijacked(t *testing.T) {
	raw := `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="write">` +
		`<｜｜DSML｜｜ parameter name="content" string="true">[{"name": "inner"}]</｜｜DSML｜｜ parameter>` +
		`</｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`
	_, calls, ok := TryParseToolCallsForTools(raw, toolsFor("write"))
	if !ok || len(calls) != 1 {
		t.Fatalf("ok=%v calls=%+v", ok, calls)
	}
	if calls[0].Function.Name != "write" {
		t.Fatalf("content 里的 JSON 抢走了调用名: %+v", calls[0])
	}
	if !strings.Contains(calls[0].Function.Arguments, "inner") {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
}

// 调用名不在已提供的工具清单里时，DSML 块按普通文本返回，不产出假调用。
func TestParseDSMLRejectsUnknownTool(t *testing.T) {
	raw := `<｜｜DSML｜｜ invoke name="not_a_tool">` +
		`<｜｜DSML｜｜ parameter name="x">1</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>`
	if _, calls, ok := TryParseToolCallsForTools(raw, toolsFor("write")); ok {
		t.Fatalf("未知工具名被当成了真实调用: %+v", calls)
	}
}

// 代码围栏里的 DSML 示例是文档，不是调用。
func TestParseDSMLInsideCodeFenceSkipped(t *testing.T) {
	raw := "示例：\n```text\n" + dsmlBlock() + "\n```"
	if _, calls, ok := TryParseToolCalls(raw); ok {
		t.Fatalf("围栏示例被当成了调用: %+v", calls)
	}
}

func TestNormalizeDSML(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<｜｜DSML｜｜ calls>`, `<tool_calls>`},
		{`</｜｜DSML｜｜ calls>`, `</tool_calls>`},
		{`<｜｜DSML｜｜ invoke name="write">`, `<invoke name="write">`},
		{"</" + "｜｜DSML｜｜ parameter>", "</" + "parameter>"},
		{`<|DSML|> invoke name="x">`, `<invoke name="x">`},
		{"</" + "|DSML| parameter>", "</" + "parameter>"},
		{`<｜DSML｜ parameter name="a">`, `<parameter name="a">`},
		{"没有 DSML 标记的普通文本", "没有 DSML 标记的普通文本"},
	}
	for _, c := range cases {
		got, changed := normalizeDSML(c.in)
		if got != c.want {
			t.Errorf("normalizeDSML(%q) = %q, want %q", c.in, got, c.want)
		}
		if wantChanged := c.in != c.want; changed != wantChanged {
			t.Errorf("normalizeDSML(%q) changed = %v, want %v", c.in, changed, wantChanged)
		}
	}
}

func TestToolCallTruncatedDSML(t *testing.T) {
	open := `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="write">` +
		`<｜｜DSML｜｜ parameter name="content">abc`
	closed := open + `</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`
	fenced := "```text\n" + open + "\n```"
	prose := "不要用 DSML 标记输出工具调用。"
	cases := []struct {
		in   string
		want bool
	}{
		{open, true},
		{closed, false},
		{fenced, false},
		{prose, false},
		{"", false},
	}
	for _, c := range cases {
		if got := ToolCallTruncated(c.in); got != c.want {
			t.Errorf("ToolCallTruncated(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestContinueInstructionDSML(t *testing.T) {
	partial := strings.Repeat("x", 120) + `<｜｜DSML｜｜ invoke name="write">` + "TAIL80CHARS"
	got := ContinueInstruction(partial, 2, 5)
	for _, want := range []string{"2/5", "TAIL80CHARS", "闭合标签", "parameter"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSML 续写指令缺少 %q:\n%s", want, got)
		}
	}
	// JSON 分支的收尾 8 字符规则不能混进来。
	if strings.Contains(got, "右花括号") {
		t.Errorf("DSML 续写指令混入了 JSON 收尾规则:\n%s", got)
	}
	// 含 JSON 载荷时仍走 JSON 指令。
	jsonPartial := strings.Repeat("x", 120) + `{"tool_calls": [{"trunc`
	if j := ContinueInstruction(jsonPartial, 2, 5); !strings.Contains(j, "右花括号") {
		t.Errorf("JSON 部分未走 JSON 指令:\n%s", j)
	}
}

// DSML 截断回复跨轮续写拼接：内容里的换行按 XML 语义原样保留，
// 不套用 JSON 字符串的 \n 转义。
func TestRunContinuedStitchesDSML(t *testing.T) {
	turns := []string{
		`<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="write">` +
			`<｜｜DSML｜｜ parameter name="content" string="true">alpha "beta`,
		" gamma\ndelta" +
			`</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`,
	}
	prompts := 0
	var got strings.Builder
	err := RunContinued(context.Background(),
		"first-prompt",
		func(string, int) string { return "continue-prompt" },
		3,
		func(_ context.Context, prompt string, emit func(string) error) error {
			prompts++
			if prompts > len(turns) {
				t.Fatalf("unexpected extra send #%d", prompts)
			}
			return emit(turns[prompts-1])
		},
		func(text string) error { got.WriteString(text); return nil },
		func(string, ...any) {},
		func(string, ...any) {},
	)
	if err != nil {
		t.Fatalf("RunContinued: %v", err)
	}
	if prompts != 2 {
		t.Fatalf("prompts = %d, want 2", prompts)
	}
	if ToolCallTruncated(got.String()) {
		t.Fatal("stitched DSML reply still truncated")
	}
	_, calls, ok := TryParseToolCalls(got.String())
	if !ok || len(calls) != 1 {
		t.Fatalf("TryParseToolCalls failed on stitched reply: ok=%v calls=%+v", ok, calls)
	}
	want := "alpha \"beta gamma\ndelta"
	if content := argsField(t, calls[0], "content"); content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
}
