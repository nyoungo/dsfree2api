# OpenAI 兼容性审计（Chat Completions / Responses / Messages）

> 审计基准：`openai-openapi` 的 `CreateChatCompletionRequest` /
> `CreateChatCompletionStreamResponse` / `CreateResponse` / `ResponseStreamEvent`
> schema，以及 `openai-python` 的运行时解析逻辑。
> 审计对象：本仓库（`github.com/nyoungo/dsfree2api`）。
>
> 本文档记录**已核对**的差异与修复结论。每条结论标注规范来源，便于回归对照。

---

## 1. 工具调用修复（tool-call repair lineage）

模型在受限通道下会产出各种畸形工具调用。网关在 `internal/openai` 内做
宽容解析，避免把原始文本透传给客户端。

**规范**：Chat Completions 的 `tool_calls` 条目必须含 `type="function"` 与
`function.name`（`function.arguments` 为 JSON 字符串）；Responses 的
`function_call` 条目必须含 `call_id` / `name` / `arguments`。

**已实现的修复**（`internal/openai/toolcalls.go`）：

- JSON 外壳：`{"tool_calls":[...]}`、裸数组、裸对象、```json / ``` 围栏；
- 未加引号的键（`{name: "f"}` → `{"name": "f"}`）；
- 非法反斜杠（`C:\Users` → `C:\\Users`）；
- 字段别名：`function`/`params`/`parameters`/`args`/`input`；
- `name` 与 `arguments` 互换（`name` 为对象、`arguments` 为字符串）；
- 参数外溢到顶层（`{"name":"f","city":"北京"}`）；
- XML 方言：`<tool_call>` / `<tool_calls>` / `<invoke name>` + `<parameter>`，
  以及 `<function><name>…<arguments>…`；
- 特殊标记 `<|tool▁calls▁begin|>` / `<|tool▁calls▁end|>`，并对 `｜`(U+FF5C)、
  `▁`(U+2581) 做模糊匹配；
- 代码围栏内的 XML 示例会被跳过（`isInsideCodeFence`），但围栏内的 JSON
  仍按既有行为解析（由 `TestTryParseToolCallsMarkdownJSONBlock` 固定）。

**约束**：`function.name` 缺失的条目会被整条候选丢弃，绝不返回 `name` 为空的
调用（否则错误会被推迟到执行阶段才暴露）。

**已知差距**：流式路径（`internal/openai/argstream.go`）只对流式化的
`{"tool_calls":…}` 形状增量下发；上面这些新形状会在 `Finalize` 时作为完整
`tool_calls` 重发（`resend`），行为正确但不增量。可接受。

**回归**：`internal/openai/repair_test.go` 覆盖 10 个场景主题
（01–04 XML、05 字段别名、06 arguments 字符串、07/08 括号、09 互换、
10 参数外溢）。

---

## 2. 服务`param`/`code`字段缺失（OpenAI Error schema）

**规范**：OpenAI `Error` 的 `required` 为 `["type", "message", "param", "code"]`；
SDK 常按 `code` 区分 `model_not_found` 等语义。

**修复**：`internal/api/server.go` 的 `writeError` 现在总是输出
`message` / `type` / `param` / `code` 四个字段（不适用时为 `null`），并按状态码
推导：

- `401` → `code="invalid_api_key"`
- `404` 且 message 以 `model not found` 开头 → `param="model"`, `code="model_not_found"`
- `429` → `code="rate_limit_exceeded"`
- `413` → `code="request_too_large"`
- `502`/`504` → `code="upstream_error"` / `upstream_timeout"`
- `idempotency_error` → `code="idempotency_error"`

Anthropic 信封（`writeAnthropicError`）保持 `{"type":"error","error":{type,message}}`
不变。

---

## 3. Responses API 状态（`previous_response_id` / `store`）

**规范**：`CreateResponse` 支持 `store`（默认 `true`）与 `previous_response_id`；
`GET /v1/responses/{id}` 返回响应快照。

**修复**：

- `openai.ResponsesRequest` 新增 `store *bool` 与 `previous_response_id string`；
- 进程内、有界、带 TTL 的缓存（`internal/api/responsestore.go`），
  容量默认 256、TTL 默认 1h，可由 `[responses]` 配置；
- 非流式与流式完成路径都会写入（`store:false` 跳过）；
- `previous_response_id` 命中时用 `openai.HistoryMessages` 重建上下文，
  拼接到本轮 input 之前（system/developer 指令保持在最前）；
- 过期 / 未知 `previous_response_id` → `400 invalid_request_error`；
  `GET` 未知或过期 → `404`。

**取舍**：绝不落盘（响应可能含用户内容），重启即失效，与上游无关的
无状态网关定位一致。

---

## 4. `Idempotency-Key` 语义

**规范**：Stripe / OpenAI 约定。

实现（`internal/api/idempotency.go`），作用于
`POST /v1/chat/completions`、`/v1/responses`、`/v1/messages`：

- 作用域 = `(API key, method+path, Idempotency-Key)`；
- 指纹 = `sha256(scope|key|body)`；
- 同键 + 不同请求体 → `400 idempotency_error`；
- 首次执行期间再次到达 → `409 idempotency_error`；
- 已完成 → 逐字节回放（状态码 / `Content-Type` / 响应体），
  并带 `idempotent-replayed: true`；
- 单条响应体上限 1 MiB、总量 64 MiB、容量 1024、TTL 24h；
  超限标记为不可回放（重试 `409`）；写入失败或处理中途 panic 则撤销占位，
  让重试可以真正重跑；
- 超过 255 字符的 key → `400`。

---

## 5. 模型别名

`config.ModelAliases` / `DefaultModel` + `Config.ResolveModel`：请求模型名按
`精确 id → 大小写不敏感 id → model_aliases（大小写不敏感）→ default_model`
解析，供 Codex CLI / Claude Code 发送的任意 `gpt-*` / `claude-*` 名字使用。
`/v1/models` 也会列出别名。见 `internal/config/alias_test.go`、
`internal/api/alias_test.go`。

---

## 6. 工具类型过滤与推理字段

Responses 请求里的非 function 工具（`web_search_preview` 等）会被
`ResponsesTool.ToToolDef` 丢弃（上游无法执行）。已由
`internal/openai/prompt_test.go:144` 固定。

`reasoning` / `reasoning_effort` 字段同样不会被转发（`ResponsesRequest` 不解析，
`encoding/json` 忽略未知字段），即对上游强制关闭，避免请求体被拒绝。

---

## 7. 待观察 / 风险

- **`hif-leim` 风险标记**：仅记录，暂不改变行为。若上游开始返回该标记，
  需要单独评估对工具调用解析的影响。
- **流式工具增量覆盖**：见 §1「已知差距」。
- **状态缓存进程内**：多实例部署时 `previous_response_id` 与
  `Idempotency-Key` 不共享；如需跨实例，需要外部存储（Redis）后续评估。