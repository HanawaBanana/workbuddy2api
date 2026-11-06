package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// responsesToChat 的消息重组：user → assistant(tool_calls) → tool，
// 覆盖 Codex 每轮重放完整 input 时的典型形态。
func TestResponsesToChatMessages(t *testing.T) {
	raw := []byte(`{
	  "model":"cn:auto","stream":true,
	  "instructions":"you are codex",
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	    {"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"},
	    {"type":"function_call_output","call_id":"call_1","output":"file.txt"}
	  ]
	}`)
	body, stream, model, _, err := responsesToChat(raw)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if !stream || model != "cn:auto" {
		t.Fatalf("stream=%v model=%q", stream, model)
	}
	var got struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("want 4 messages, got %d: %s", len(got.Messages), body)
	}
	if got.Messages[0].Role != "system" {
		t.Errorf("msg0 role=%q", got.Messages[0].Role)
	}
	if got.Messages[1].Role != "user" {
		t.Errorf("msg1 role=%q", got.Messages[1].Role)
	}
	a := got.Messages[2]
	if a.Role != "assistant" || len(a.ToolCalls) != 1 {
		t.Fatalf("msg2 = %+v", a)
	}
	if a.ToolCalls[0].ID != "call_1" || a.ToolCalls[0].Function.Name != "shell" {
		t.Errorf("tool_call = %+v", a.ToolCalls[0])
	}
	if a.ToolCalls[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("arguments = %q", a.ToolCalls[0].Function.Arguments)
	}
	tool := got.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" {
		t.Errorf("msg3 = %+v", tool)
	}
	// tool_call 必须紧跟 tool 消息（chat 协议硬约束）。
	if s, _ := tool.Content.(string); s != "file.txt" {
		t.Errorf("tool content = %v", tool.Content)
	}
}

// 扁平工具描述 → chat 的 {type,function} 嵌套；custom 工具应被跳过。
func TestResponsesToChatTools(t *testing.T) {
	raw := []byte(`{
	  "model":"cn:auto",
	  "input":"ping",
	  "tools":[
	    {"type":"function","name":"shell","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
	    {"type":"custom","name":"apply_patch","description":"freeform"}
	  ],
	  "tool_choice":"auto"
	}`)
	body, _, _, _, err := responsesToChat(raw)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice any `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("want 1 tool (custom skipped), got %d", len(got.Tools))
	}
	if got.Tools[0].Function.Name != "shell" {
		t.Errorf("tool name = %q", got.Tools[0].Function.Name)
	}
	if !strings.Contains(string(got.Tools[0].Function.Parameters), `"cmd"`) {
		t.Errorf("parameters = %s", got.Tools[0].Function.Parameters)
	}
	if got.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v", got.ToolChoice)
	}
}

// 流式文本：delta 增量 → output_text.delta，finalize 补 completed。
func TestResponsesStreamText(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := newRespTranslator(rec, "cn:auto", true, nil)
	tr.onChunk([]byte(`{"id":"c1","choices":[{"delta":{"content":"Hel"}}]}`))
	tr.onChunk([]byte(`{"id":"c1","choices":[{"delta":{"content":"lo"}}]}`))
	tr.onChunk([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	tr.finalize()

	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing event %q in stream", want)
		}
	}
	// completed 事件里必须带完整文本与 usage。
	last := lastEventPayload(t, out, "response.completed")
	resp := last["response"].(map[string]any)
	items := resp["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output len = %d", len(items))
	}
	msg := items[0].(map[string]any)
	content := msg["content"].([]any)[0].(map[string]any)
	if content["text"] != "Hello" {
		t.Errorf("text = %v", content["text"])
	}
	if _, ok := resp["usage"]; !ok {
		t.Error("completed response missing usage")
	}
}

// 流式工具调用：arguments 分片累积，call_id/name 落在 function_call item 上。
func TestResponsesStreamToolCall(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := newRespTranslator(rec, "cn:auto", true, nil)
	tr.onChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"shell","arguments":"{\"cmd\""}}]}}]}`))
	tr.onChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"ls\"}"}}]}}]}`))
	tr.onChunk([]byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`))
	tr.finalize()

	out := rec.Body.String()
	if !strings.Contains(out, "event: response.function_call_arguments.delta") {
		t.Error("missing function_call_arguments.delta")
	}
	if !strings.Contains(out, "event: response.function_call_arguments.done") {
		t.Error("missing function_call_arguments.done")
	}
	last := lastEventPayload(t, out, "response.completed")
	resp := last["response"].(map[string]any)
	items := resp["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output len = %d", len(items))
	}
	fc := items[0].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_x" || fc["name"] != "shell" {
		t.Errorf("item = %+v", fc)
	}
	if fc["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments = %v", fc["arguments"])
	}
}

// 上游错误帧 → response.failed，且不再发 completed。
func TestResponsesStreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := newRespTranslator(rec, "cn:auto", true, nil)
	tr.onChunk([]byte(`{"error":{"message":"all accounts cooling","code":"rate_limit_exceeded"}}`))
	tr.finalize()

	out := rec.Body.String()
	if !strings.Contains(out, "event: response.failed") {
		t.Errorf("missing response.failed: %s", out)
	}
	if strings.Contains(out, "event: response.completed") {
		t.Error("must not emit completed after failure")
	}
}

// 非流式 chat.completion → response 对象。
func TestChatCompletionToResponse(t *testing.T) {
	body := []byte(`{
	  "id":"chatcmpl-1","model":"cn:auto","created":1700000000,
	  "choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
	  "usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
	}`)
	resp, err := chatCompletionToResponse(body, "cn:auto", nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("resp = %+v", resp)
	}
	out := resp["output"].([]map[string]any)
	if len(out) != 1 || out[0]["type"] != "message" {
		t.Fatalf("output = %+v", out)
	}
	u := resp["usage"].(*respUsage)
	if u.InputTokens != 5 || u.OutputTokens != 3 {
		t.Errorf("usage = %+v", u)
	}
}

// 上游注释心跳与 data 帧同块（无空行分隔）时，首帧不得被丢弃。
// 回归用例：曾因整块 HasPrefix("data:") 判断，把带 name 与首个 arguments
// 分片的首帧吞掉，导致 function_call 的 name 为空、arguments 缺开头。
func TestRespSinkHeartbeatPrefix(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newRespSink(rec, "cn:auto", true, nil)
	s.hdr.Set("Content-Type", "text/event-stream")
	// 首块：注释行 + data 行，中间无空行
	s.Write([]byte(`: heartbeat
data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":"{\""}}]}}]}

`))
	s.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"cmd\":\"ls\"}"}}]}}]}

`))
	s.finish()

	out := rec.Body.String()
	last := lastEventPayload(t, out, "response.completed")
	items := last["response"].(map[string]any)["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output len = %d, out=%s", len(items), out)
	}
	fc := items[0].(map[string]any)
	if fc["name"] != "shell" {
		t.Errorf("name = %v (首帧被丢)", fc["name"])
	}
	if fc["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments = %v", fc["arguments"])
	}
}

// lastEventPayload 取出指定类型的最后一个 SSE 事件的 data JSON。
func lastEventPayload(t *testing.T, stream, typ string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, block := range strings.Split(stream, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
				continue
			}
			if m["type"] == typ {
				found = m
			}
		}
	}
	if found == nil {
		t.Fatalf("event %q not found", typ)
	}
	return found
}

// ---------------------------------------------------------------------------
// reasoning_content → reasoning summary 事件链
// ---------------------------------------------------------------------------

// 思考增量必须先于正文建项，且 completed 里 reasoning item 排在 message 之前。
func TestResponsesStreamReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := newRespTranslator(rec, "cn:deepseek-v4.1-flash", true, nil)
	tr.onChunk([]byte(`{"choices":[{"delta":{"reasoning_content":"先想"}}]}`))
	tr.onChunk([]byte(`{"choices":[{"delta":{"reasoning_content":"一下"}}]}`))
	tr.onChunk([]byte(`{"choices":[{"delta":{"content":"答案"}}]}`))
	tr.finalize()

	out := rec.Body.String()
	for _, want := range []string{
		"event: response.output_item.added",
		"event: response.reasoning_summary_part.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
		"event: response.reasoning_summary_part.done",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing event %q", want)
		}
	}
	// 增量帧必须逐字给出，客户端靠它流式渲染。
	if d := lastEventPayload(t, out, "response.reasoning_summary_text.delta"); d["delta"] != "一下" {
		t.Errorf("last delta = %v", d["delta"])
	}
	if d := lastEventPayload(t, out, "response.reasoning_summary_text.done"); d["text"] != "先想一下" {
		t.Errorf("summary text = %v", d["text"])
	}

	comp := lastEventPayload(t, out, "response.completed")
	resp := comp["response"].(map[string]any)
	items := resp["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("want 2 output items, got %d", len(items))
	}
	r0 := items[0].(map[string]any)
	if r0["type"] != "reasoning" {
		t.Fatalf("item0 type = %v (reasoning 必须排在 message 之前)", r0["type"])
	}
	sum := r0["summary"].([]any)[0].(map[string]any)
	if sum["type"] != "summary_text" || sum["text"] != "先想一下" {
		t.Errorf("summary = %v", sum)
	}
	if items[1].(map[string]any)["type"] != "message" {
		t.Errorf("item1 type = %v", items[1].(map[string]any)["type"])
	}
}

// reasoning.effort 必须透传给 chat 的 reasoning_effort（上游会按模型档位降级）。
func TestResponsesForwardsReasoningEffort(t *testing.T) {
	raw := []byte(`{"model":"m","input":"hi","reasoning":{"effort":"high","summary":"auto"}}`)
	body, _, _, _, err := responsesToChat(raw)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", got["reasoning_effort"])
	}
}

// ---------------------------------------------------------------------------
// namespace 工具（Codex computer use / browser / multi_agent 的载体）
// ---------------------------------------------------------------------------

// namespace 组内的子工具必须铺平成独立 function，并记进扁平名 → namespace 映射。
func TestConvertToolsNamespace(t *testing.T) {
	raw := []byte(`{
	  "model":"m","input":"hi",
	  "tools":[
	    {"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{}}},
	    {"type":"namespace","name":"mcp__cua_repl","description":"UI automation",
	     "tools":[
	       {"type":"function","name":"js","description":"control UI","parameters":{"type":"object","properties":{"code":{"type":"string"}}}},
	       {"type":"function","name":"reset","description":"reset","parameters":{"type":"object","properties":{}}}
	     ]},
	    {"type":"web_search","external_web_access":false}
	  ]
	}`)
	body, _, _, ns, err := responsesToChat(raw)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	names := make([]string, 0, len(got.Tools))
	for _, x := range got.Tools {
		names = append(names, x.Function.Name)
	}
	// exec_command + cua 的 2 个子工具；web_search 无 chat 对等物应被跳过。
	want := []string{"exec_command", "mcp__cua_repl__js", "mcp__cua_repl__reset"}
	if len(names) != len(want) {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("tool[%d] = %q, want %q", i, names[i], want[i])
		}
	}
	if !strings.Contains(string(got.Tools[1].Function.Parameters), `"code"`) {
		t.Errorf("子工具 parameters 丢失: %s", got.Tools[1].Function.Parameters)
	}
	if r, ok := ns["mcp__cua_repl__js"]; !ok || r.Namespace != "mcp__cua_repl" || r.Name != "js" {
		t.Errorf("ns 映射错误: %+v", ns)
	}
}

// 模型调用扁平名时，回传的 function_call 必须带 namespace，Codex 才知道路由到哪个工具组。
func TestResponsesNamespaceCallRoundTrip(t *testing.T) {
	ns := map[string]nsRef{"mcp__cua_repl__js": {Namespace: "mcp__cua_repl", Name: "js"}}

	rec := httptest.NewRecorder()
	tr := newRespTranslator(rec, "m", true, ns)
	tr.onChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"mcp__cua_repl__js","arguments":"{\"code\":\"1\"}"}}]}}]}`))
	tr.finalize()

	comp := lastEventPayload(t, rec.Body.String(), "response.completed")
	items := comp["response"].(map[string]any)["output"].([]any)
	fc := items[0].(map[string]any)
	if fc["namespace"] != "mcp__cua_repl" {
		t.Errorf("namespace = %v, want mcp__cua_repl", fc["namespace"])
	}
	if fc["name"] != "js" {
		t.Errorf("name = %v, want js（应去掉扁平前缀）", fc["name"])
	}

	// 反向：历史回放时 namespace+name 要还原成扁平名，否则与工具表对不上。
	raw := []byte(`{"model":"m","input":[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
	  {"type":"function_call","namespace":"mcp__cua_repl","name":"js","arguments":"{}","call_id":"call_1"}
	]}`)
	body, _, _, _, err := responsesToChat(raw)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 2 || len(got.Messages[1].ToolCalls) != 1 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if n := got.Messages[1].ToolCalls[0].Function.Name; n != "mcp__cua_repl__js" {
		t.Errorf("replay name = %q, want mcp__cua_repl__js", n)
	}
}
