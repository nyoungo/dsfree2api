package anthropic

import (
	"encoding/json"
)

// Request is the body of POST /v1/messages (Anthropic Messages API).
type Request struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []Message       `json:"messages"`
	Tools         []ToolDef       `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Metadata      *Metadata       `json:"metadata,omitempty"`
}

type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}

type Message struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// Content is either a plain string or an array of content blocks.
type Content struct {
	Text   string
	Blocks []Block
}

func (c *Content) UnmarshalJSON(data []byte) error {
	data = trimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		c.Text, c.Blocks = "", nil
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		c.Text, c.Blocks = s, nil
		return nil
	}
	var blocks []Block
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	c.Blocks, c.Text = blocks, ""
	return nil
}

func (c Content) MarshalJSON() ([]byte, error) {
	if c.Blocks != nil {
		return json.Marshal(c.Blocks)
	}
	return json.Marshal(c.Text)
}

// Block is one content block of a message, request or response side.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type ToolChoice struct {
	Type string `json:"type"` // "auto" | "any" | "tool" | "none"
	Name string `json:"name,omitempty"`
}

// Response is the body of a completed non-streaming /v1/messages call.
type Response struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []Block `json:"content"`
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        Usage   `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
