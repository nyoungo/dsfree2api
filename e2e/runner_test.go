//go:build e2e

// Package e2e is a scenario harness that exercises a running dsfree2api
// instance over HTTP. It is ported from the reference py-e2e-tests runner.
//
// Run it against a live gateway:
//
//	E2E_BASE_URL=http://127.0.0.1:8000 \
//	E2E_API_KEY=sk-... \
//	go test -tags e2e ./e2e/... -v
//
// Optional env:
//
//	E2E_MODEL         override the model sent (default: first in models[])
//	E2E_SCENARIOS     scenario directory (default: scenarios)
//	E2E_TIMEOUT_SEC   per-request timeout (default: 120)
//
// Without E2E_BASE_URL every test is skipped, so `go test ./...` stays green.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type scenario struct {
	Name       string          `json:"name"`
	Endpoint   string          `json:"endpoint"`
	Category   string          `json:"category"`
	Models     []string        `json:"models"`
	System     string          `json:"system"`
	Messages   []message       `json:"messages"`
	Tools      json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Request    json.RawMessage `json:"request"`
	Checks     checks          `json:"checks"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type checks struct {
	HasToolCalls bool     `json:"has_tool_calls"`
	ToolNames    []string `json:"tool_names"`
	FinishReason string   `json:"finish_reason"`
	NoError      bool     `json:"no_error"`
}

type outcome struct {
	content      string
	toolCalls    []toolCall
	finishReason string
}

type toolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func TestScenarioFilesParse(t *testing.T) {
	files, err := scenarioFiles(env("E2E_SCENARIOS", "scenarios"))
	if err != nil {
		t.Fatalf("scan scenarios: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no scenario files found")
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var sc scenario
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if sc.Name == "" {
			t.Errorf("%s: missing name", file)
		}
		if sc.Endpoint != "" && sc.Endpoint != "openai" && sc.Endpoint != "anthropic" {
			t.Errorf("%s: unknown endpoint %q", file, sc.Endpoint)
		}
	}
}

func TestScenarios(t *testing.T) {
	base := env("E2E_BASE_URL", "")
	if base == "" {
		t.Skip("E2E_BASE_URL is not set; skipping live e2e scenarios")
	}
	base = strings.TrimRight(base, "/")
	apiKey := strings.TrimSpace(os.Getenv("E2E_API_KEY"))
	dir := env("E2E_SCENARIOS", "scenarios")
	timeout, _ := strconv.Atoi(env("E2E_TIMEOUT_SEC", "120"))
	if timeout <= 0 {
		timeout = 120
	}
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}

	files, err := scenarioFiles(dir)
	if err != nil {
		t.Fatalf("scan scenarios: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no scenarios found under %s", dir)
	}

	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var sc scenario
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		t.Run(sc.Name, func(t *testing.T) {
			if sc.Endpoint != "" && sc.Endpoint != "openai" {
				t.Skipf("endpoint %q not supported by the Go harness yet", sc.Endpoint)
			}
			runOpenAIScenario(t, client, base, apiKey, &sc)
		})
	}
}

// scenarioFiles walks dir recursively and returns every *.json path, sorted.
func scenarioFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".json") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func runOpenAIScenario(t *testing.T, client *http.Client, base, apiKey string, sc *scenario) {
	model := env("E2E_MODEL", "")
	if model == "" && len(sc.Models) > 0 {
		model = sc.Models[0]
	}

	messages := make([]map[string]any, 0, len(sc.Messages)+1)
	if sc.System != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sc.System})
	}
	for _, m := range sc.Messages {
		content := any("")
		if len(m.Content) > 0 {
			_ = json.Unmarshal(m.Content, &content)
		}
		messages = append(messages, map[string]any{"role": m.Role, "content": content})
	}

	payload := map[string]any{"model": model, "messages": messages}
	if len(sc.Tools) > 0 {
		var tools any
		if json.Unmarshal(sc.Tools, &tools) == nil {
			payload["tools"] = tools
		}
	}
	if len(sc.ToolChoice) > 0 {
		var tc any
		if json.Unmarshal(sc.ToolChoice, &tc) == nil {
			payload["tool_choice"] = tc
		}
	}
	stream := false
	if len(sc.Request) > 0 {
		var req map[string]any
		if json.Unmarshal(sc.Request, &req) == nil {
			if v, ok := req["stream"].(bool); ok {
				stream = v
			}
			for k, v := range req {
				if k != "stream" && k != "messages" {
					payload[k] = v
				}
			}
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	url := base + "/v1/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if sc.Checks.NoError && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("HTTP %d from %s: %s", resp.StatusCode, url, body)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}

	var out outcome
	if stream {
		out, err = collectStream(resp.Body)
	} else {
		out, err = parseCompletion(resp.Body)
	}
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if sc.Checks.HasToolCalls && len(out.toolCalls) == 0 {
		t.Fatalf("expected tool calls, got none (content=%q finish=%q)", out.content, out.finishReason)
	}
	for _, want := range sc.Checks.ToolNames {
		if !hasToolName(out.toolCalls, want) {
			t.Fatalf("expected tool %q among %+v", want, out.toolCalls)
		}
	}
	if sc.Checks.FinishReason != "" && out.finishReason != sc.Checks.FinishReason {
		t.Fatalf("finish_reason = %q, want %q", out.finishReason, sc.Checks.FinishReason)
	}
	for _, tc := range out.toolCalls {
		if !json.Valid([]byte(tc.Arguments)) {
			t.Errorf("tool %q arguments are not valid JSON: %q", tc.Name, tc.Arguments)
		}
	}
}

func hasToolName(calls []toolCall, name string) bool {
	for _, c := range calls {
		if c.Name == name {
			return true
		}
	}
	return false
}

func parseCompletion(r io.Reader) (outcome, error) {
	var body struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function toolCall `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return outcome{}, err
	}
	var out outcome
	if len(body.Choices) == 0 {
		return out, nil
	}
	ch := body.Choices[0]
	out.content = ch.Message.Content
	out.finishReason = ch.FinishReason
	for _, tc := range ch.Message.ToolCalls {
		out.toolCalls = append(out.toolCalls, tc.Function)
	}
	return out, nil
}

// collectStream assembles SSE chat.completion.chunk deltas into one outcome.
func collectStream(r io.Reader) (outcome, error) {
	var out outcome
	type acc struct {
		name string
		args strings.Builder
	}
	index := map[int]*acc{}
	order := []int{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				out.finishReason = c.FinishReason
			}
			out.content += c.Delta.Content
			for _, tc := range c.Delta.ToolCalls {
				a, ok := index[tc.Index]
				if !ok {
					a = &acc{}
					index[tc.Index] = a
					order = append(order, tc.Index)
				}
				if tc.Function.Name != "" {
					a.name += tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					a.args.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("read stream: %w", err)
	}
	for _, i := range order {
		a := index[i]
		out.toolCalls = append(out.toolCalls, toolCall{Name: a.name, Arguments: a.args.String()})
	}
	return out, nil
}
