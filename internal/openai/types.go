package openai

import (
	"encoding/json"
	"fmt"
)

type ChatMessage struct {
	Role       string         `json:"role"`
	Content    MessageContent `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// MessageContent accepts either a plain string or the OpenAI content-part
// array, unmarshalling into whichever shape arrives on the wire.
type MessageContent struct {
	Text  string
	Parts []map[string]any
	IsRaw bool
}

func (m *MessageContent) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		m.Text = s
		m.Parts = nil
		m.IsRaw = false
		return nil
	}
	if string(data) == "null" {
		m.Text = ""
		m.Parts = nil
		return nil
	}
	var parts []map[string]any
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	m.Parts = parts
	m.Text = ""
	m.IsRaw = true
	return nil
}

func (m MessageContent) MarshalJSON() ([]byte, error) {
	if m.IsRaw && m.Parts != nil {
		return json.Marshal(m.Parts)
	}
	return json.Marshal(m.Text)
}

func (m MessageContent) String() string {
	if m.IsRaw || (m.Text == "" && len(m.Parts) > 0) {
		return ContentToText(m.Parts)
	}
	return m.Text
}

type ResponseFormat struct {
	Type       string          `json:"type,omitempty"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// StopSequences 兼容 OpenAI 对 stop 的两种写法：单个字符串或字符串数组。
// 直接用 []string 会让 `"stop": "END"` 这种合法请求整个解码失败（400）。
type StopSequences []string

func (s *StopSequences) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		if one == "" {
			*s = nil
		} else {
			*s = StopSequences{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("stop must be a string or an array of strings")
	}
	*s = many
	return nil
}

type ChatCompletionRequest struct {
	Model            string          `json:"model"`
	Messages         []ChatMessage   `json:"messages"`
	Stream           bool            `json:"stream,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	MaxCompletionTok *int            `json:"max_completion_tokens,omitempty"`
	Tools            []ToolDef       `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	User             string          `json:"user,omitempty"`
	Stop             StopSequences   `json:"stop,omitempty"`
	ResponseFormat   *ResponseFormat `json:"response_format,omitempty"`
	StreamOptions    *StreamOptions  `json:"stream_options,omitempty"`
	N                *int            `json:"n,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
}

type ToolDef struct {
	Type     string   `json:"type,omitempty"`
	Function Function `json:"function"`
}

type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// ToolCallDelta is one entry of a streamed tool_calls delta. Index is always
// present; the other fields are emitted only when they carry content.
type ToolCallDelta struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function *FunctionDelta `json:"function,omitempty"`
}

type FunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Stream          bool            `json:"stream,omitempty"`
	Instructions    string          `json:"instructions,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Tools           []ResponsesTool `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	// Text 是 Responses API 的输出配置块，text.format 承载输出格式约束
	// （json_schema / json_object / text）。不解析它，客户端指定的
	// json_schema 会被静默丢弃，模型输出的 JSON 无从校验。
	Text *TextFormat `json:"text,omitempty"`
}

// TextFormat 是 Responses API 的 text 对象，这里只消费 format。
type TextFormat struct {
	Format *ResponseFormat `json:"format,omitempty"`
}

// ResponsesTool is a function tool in the flat Responses API shape; the nested
// Chat Completions shape is tolerated as well. Non-function tools (web search
// etc.) are dropped because the upstream cannot execute them.
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
	Function    *Function       `json:"function,omitempty"`
}

// ToToolDef normalizes the tool onto the Chat Completions function shape.
func (t ResponsesTool) ToToolDef() (ToolDef, bool) {
	if t.Function != nil && t.Function.Name != "" {
		return ToolDef{Type: "function", Function: *t.Function}, true
	}
	if t.Name == "" || (t.Type != "" && t.Type != "function") {
		return ToolDef{}, false
	}
	return ToolDef{Type: "function", Function: Function{
		Name: t.Name, Description: t.Description, Parameters: t.Parameters,
	}}, true
}

type ChoiceMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type Choice struct {
	Index        int           `json:"index"`
	Message      ChoiceMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type ChoiceDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        ChoiceDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type ChatCompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code,omitempty"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}
