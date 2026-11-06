package server

// OpenAI Responses API 兼容层（/v1/responses）。
//
// 背景：Codex CLI 0.80+ 起只支持 `wire_api = "responses"`，而本网关的上游
// （CodeBuddy）只提供 Chat Completions 语义。本文件在**不改动任何既有逻辑**的
// 前提下补一层协议适配：
//
//	Responses 请求 → (responsesToChat) → 内部 chat body
//	              → h.chatCompletions(sink, 内部请求)   ← 完整复用账号池/轮转/熔断/计费
//	              → (respSink + respTranslator) → Responses 事件流
//
// 之所以用「内部再调一次 chatCompletions」而不是重写一套上游调用：账号池轮转、
// 换号重试、粘性、积分预算、错误分类这些都在那 500 行里，复制一份必然漂移。
// sink 拦在它前面，只做「Chat chunk → Responses event」的翻译。
//
// 有状态语义（previous_response_id / store=true）不支持：Codex 每轮都重放完整
// input 且 store=false，无状态实现即可满足。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 请求结构
// ---------------------------------------------------------------------------

type respReq struct {
	Model             string            `json:"model"`
	Instructions      string            `json:"instructions"`
	Input             json.RawMessage   `json:"input"`
	Tools             []json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	MaxOutputTokens   int               `json:"max_output_tokens"`
	Temperature       *float64          `json:"temperature"`
	TopP              *float64          `json:"top_p"`
	Stream            bool              `json:"stream"`
	// Reasoning 形如 {"effort":"high","summary":"auto"}。effort 透传给上游（那里有
	// PrepareBodyOptWithEfforts 按模型 supportedEfforts 做降级），summary 无法满足
	// （Chat 协议没有 reasoning summary，只能把 reasoning_content 原文当摘要回传）。
	Reasoning json.RawMessage `json:"reasoning"`
}

// nsRef 记录「扁平化后的 chat 函数名」到「Responses namespace + 原始工具名」的映射。
//
// Responses 协议用 type="namespace" 把一组工具打包下发（Codex 的 computer use
// /mcp__cua_repl、browser/mcp__node_repl、multi_agent_v1、各类插件都是这个形态），
// 而 Chat Completions 只有平铺的 function 列表。故下发时拼成
// "<namespace>__<tool>"，模型调用后回传时再按本表拆回 namespace 字段，
// 否则 Codex 只看到裸工具名，无法路由到命名空间内的实现。
type nsRef struct {
	Namespace string
	Name      string
}

const nsSep = "__"

func flattenName(ns, name string) string { return ns + nsSep + name }

// responses 处理入口。鉴权由 withAuth 完成（context 里已注入密钥条目），
// 因此可以直接复用 chatCompletions 内部对 keyGroups 的读取。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chatBody, stream, model, nsMap, err := responsesToChat(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid responses payload: "+err.Error())
		return
	}

	// 构造内部请求：只换 body，其余（context / 会话头 / 追踪头）原样保留，
	// 使粘性、会话聚合键、keyGroups 与直接调用 /v1/chat/completions 完全一致。
	ir := r.Clone(r.Context())
	ir.Body = io.NopCloser(bytes.NewReader(chatBody))
	ir.ContentLength = int64(len(chatBody))
	ir.Header.Set("Content-Type", "application/json")

	sink := newRespSink(w, model, stream, nsMap)
	h.chatCompletions(sink, ir)
	sink.finish()
}

// ---------------------------------------------------------------------------
// 请求转换：Responses input/tools → Chat messages/tools
// ---------------------------------------------------------------------------

func responsesToChat(raw []byte) (chatBody []byte, stream bool, model string, ns map[string]nsRef, err error) {
	var req respReq
	if err = json.Unmarshal(raw, &req); err != nil {
		return nil, false, "", nil, err
	}

	msgs := make([]map[string]any, 0, 8)
	if s := strings.TrimSpace(req.Instructions); s != "" {
		// instructions 等价于 chat 的 system；放在最前，后续 system/developer 依次追加。
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}

	// lastAssistant 指向「最近一条可作为 tool_calls 载体的 assistant 消息」下标。
	// chat 协议要求 tool_calls 挂在 assistant 消息内、其后紧跟 role=tool，
	// 而 Responses 把它们拆成平级的 item，故需按序重组。-1 表示不可挂载。
	lastAssistant := -1

	if len(req.Input) > 0 {
		var items []json.RawMessage
		var asText string
		if json.Unmarshal(req.Input, &items) != nil {
			// input 允许直接是字符串（简写形态）。
			if json.Unmarshal(req.Input, &asText) == nil && asText != "" {
				msgs = append(msgs, map[string]any{"role": "user", "content": asText})
			}
		} else {
			for _, it := range items {
				var item struct {
					Type      string          `json:"type"`
					Role      string          `json:"role"`
					Content   json.RawMessage `json:"content"`
					Name      string          `json:"name"`
					Namespace string          `json:"namespace"`
					Arguments string          `json:"arguments"`
					CallID    string          `json:"call_id"`
					Output    json.RawMessage `json:"output"`
				}
				if json.Unmarshal(it, &item) != nil {
					continue
				}
				switch item.Type {
				case "function_call":
					// 命名空间工具（computer use / browser / multi_agent 等）在历史回放里
					// 必须还原成扁平名，否则模型看到的名字与工具表对不上。
					callName := item.Name
					if item.Namespace != "" {
						callName = flattenName(item.Namespace, item.Name)
					}
					call := map[string]any{
						"id":   item.CallID,
						"type": "function",
						"function": map[string]any{
							"name":      callName,
							"arguments": item.Arguments,
						},
					}
					if item.CallID == "" {
						call["id"] = newCallID()
					}
					if lastAssistant >= 0 {
						if tc, ok := msgs[lastAssistant]["tool_calls"].([]map[string]any); ok {
							msgs[lastAssistant]["tool_calls"] = append(tc, call)
						} else {
							msgs[lastAssistant]["tool_calls"] = []map[string]any{call}
							// tool_calls 与 content 并存时 content 必须是 null 或字符串；
							// 这里保留原文本（模型可能先说一句再调工具）。
						}
					} else {
						msgs = append(msgs, map[string]any{
							"role":       "assistant",
							"content":    nil,
							"tool_calls": []map[string]any{call},
						})
						lastAssistant = len(msgs) - 1
					}
				case "function_call_output":
					out := rawToString(item.Output)
					msgs = append(msgs, map[string]any{
						"role":         "tool",
						"tool_call_id": item.CallID,
						"content":      out,
					})
					// tool 消息打断了 assistant 的 tool_calls 块，后续 function_call 需新建 message。
					lastAssistant = -1
				case "reasoning", "item_reference", "custom_tool_call", "custom_tool_call_output":
					// reasoning item、item 引用、自定义工具（apply_patch 等自由文本工具）
					// 在 Chat 协议里都没有对等物：直接丢弃。
					// Codex 只用 reasoning 展示思考摘要，缺失不影响工具循环。
					continue
				default:
					// message（type 通常为 "message" 或缺失）以及自定义 item。
					role := item.Role
					if role == "" {
						continue
					}
					if role == "developer" {
						role = "system"
					}
					msgs = append(msgs, map[string]any{
						"role":    role,
						"content": chatContent(item.Content),
					})
					if role == "assistant" {
						lastAssistant = len(msgs) - 1
					} else {
						lastAssistant = -1
					}
				}
			}
		}
	}
	if len(msgs) == 0 {
		return nil, false, req.Model, nil, fmt.Errorf("input is empty")
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	tools, nsMap := convertTools(req.Tools)
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := convertToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
		if req.ParallelToolCalls != nil {
			out["parallel_tool_calls"] = *req.ParallelToolCalls
		}
	}
	// reasoning.effort → chat 的 reasoning_effort。上游 PrepareBodyOptWithEfforts
	// 会按模型 supportedEfforts 自动降级，故这里原样透传即可。
	if e := reasoningEffort(req.Reasoning); e != "" {
		out["reasoning_effort"] = e
	}
	// max_output_tokens 语义对齐 chat 的 max_tokens。
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}

	b, err := json.Marshal(out)
	return b, req.Stream, req.Model, nsMap, err
}

// reasoningEffort 从 Responses 的 reasoning 对象里取 effort。
// 兼容 {"effort":"high"} 与少数客户端直接给字符串的形态。
func reasoningEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Effort
	}
	return ""
}

// chatContent 把 Responses 的内容数组压成 chat 的 content：
// 纯文本 → string；含图片 → 多模态 part 数组。
func chatContent(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL any    `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	texts := make([]string, 0, len(parts))
	multi := make([]map[string]any, 0, len(parts))
	hasImage := false
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "summary_text":
			texts = append(texts, p.Text)
			multi = append(multi, map[string]any{"type": "text", "text": p.Text})
		case "input_image":
			u := imageURLOf(p.ImageURL)
			if u == "" {
				continue
			}
			hasImage = true
			multi = append(multi, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": u},
			})
		}
	}
	if hasImage {
		return multi
	}
	return strings.Join(texts, "\n")
}

func imageURLOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if s, ok := x["url"].(string); ok {
			return s
		}
	}
	return ""
}

func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// convertTools 把 Responses 的扁平工具描述转成 chat 的 {type,function} 嵌套形态。
func convertTools(raw []json.RawMessage) ([]map[string]any, map[string]nsRef) {
	out := make([]map[string]any, 0, len(raw))
	ns := make(map[string]nsRef)

	// buildFn 生成一条 chat function 定义；name 为最终对外暴露的名字。
	buildFn := func(name, desc string, params json.RawMessage) (map[string]any, bool) {
		if name == "" {
			return nil, false
		}
		fn := map[string]any{"name": name}
		if desc != "" {
			fn["description"] = desc
		}
		if len(params) > 0 {
			fn["parameters"] = params
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		return map[string]any{"type": "function", "function": fn}, true
	}

	for _, r := range raw {
		var t struct {
			Type        string            `json:"type"`
			Name        string            `json:"name"`
			Description string            `json:"description"`
			Parameters  json.RawMessage   `json:"parameters"`
			Tools       []json.RawMessage `json:"tools"`
			Function    *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(r, &t) != nil {
			continue
		}

		// namespace：把组内每个子工具铺平成独立 function，名字加前缀，
		// 并把映射记进 ns，供回传时还原 namespace 字段。
		if t.Type == "namespace" {
			if t.Name == "" {
				continue
			}
			for _, sub := range t.Tools {
				var st struct {
					Type        string          `json:"type"`
					Name        string          `json:"name"`
					Description string          `json:"description"`
					Parameters  json.RawMessage `json:"parameters"`
					InputSchema json.RawMessage `json:"inputSchema"`
				}
				if json.Unmarshal(sub, &st) != nil {
					continue
				}
				// 子工具既可能是 type="function"（Responses 原生形态），
				// 也可能是 MCP 的 name/description/inputSchema（DynamicToolNamespaceTool）。
				if st.Name == "" || (st.Type != "" && st.Type != "function" && st.Type != "custom") {
					continue
				}
				params := st.Parameters
				if len(params) == 0 {
					params = st.InputSchema
				}
				// 组描述拼上子工具描述，避免模型只看到孤立函数名而不知归属。
				desc := st.Description
				if t.Description != "" {
					desc = "[" + t.Name + "] " + desc
				}
				name := flattenName(t.Name, st.Name)
				if fn, ok := buildFn(name, desc, params); ok {
					out = append(out, fn)
					ns[name] = nsRef{Namespace: t.Name, Name: st.Name}
				}
			}
			continue
		}

		// custom（Codex 的 apply_patch 等自由文本工具）没有 chat 对等物：跳过，
		// 模型仍可通过 shell 完成同类操作。web_search 等托管工具同样跳过。
		if t.Type != "function" && t.Function == nil {
			continue
		}
		name, desc, params := t.Name, t.Description, t.Parameters
		if t.Function != nil {
			if name == "" {
				name = t.Function.Name
			}
			if desc == "" {
				desc = t.Function.Description
			}
			if len(params) == 0 {
				params = t.Function.Parameters
			}
		}
		if fn, ok := buildFn(name, desc, params); ok {
			out = append(out, fn)
		}
	}
	return out, ns
}

func convertToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &o) == nil && o.Type == "function" && o.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": o.Name}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 输出拦截：chatCompletions 的 ResponseWriter
// ---------------------------------------------------------------------------

type sinkMode int

const (
	modeUnknown sinkMode = iota
	modeStream
	modeBuffered
)

type respSink struct {
	w     http.ResponseWriter
	tr    *respTranslator
	hdr   http.Header
	code  int
	mode  sinkMode
	buf   []byte
	wrote bool
}

func newRespSink(w http.ResponseWriter, model string, stream bool, ns map[string]nsRef) *respSink {
	return &respSink{
		w:    w,
		hdr:  make(http.Header),
		code: http.StatusOK,
		tr:   newRespTranslator(w, model, stream, ns),
	}
}

func (s *respSink) Header() http.Header { return s.hdr }

func (s *respSink) WriteHeader(code int) { s.code = code }

func (s *respSink) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	if s.mode == modeUnknown {
		// StreamHint 先 Set Content-Type 再写首帧，故首次 Write 时即可判定形态。
		if strings.HasPrefix(s.hdr.Get("Content-Type"), "text/event-stream") {
			s.mode = modeStream
			s.startStream()
		} else {
			s.mode = modeBuffered
		}
	}
	if s.mode == modeStream {
		s.drain()
	}
	return len(p), nil
}

// Flush 由 StreamHint 逐帧调用；帧的写出时机由 drain 掌控，此处空实现。
func (s *respSink) Flush() {}

func (s *respSink) startStream() {
	if s.wrote {
		return
	}
	s.wrote = true
	h := s.w.Header()
	// X-Wb-Account 由 chatCompletions 写在 sink.hdr 上，转写到真实响应。
	if v := s.hdr.Get("X-Wb-Account"); v != "" {
		h.Set("X-Wb-Account", v)
	}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.tr.bindWriter(s.w)
}

// drain 按 SSE 帧边界（空行）切分并逐帧翻译。
//
// ★ 必须按「块内逐行」找 data:，不能对整块做 HasPrefix 判断 ★
// 上游会在流首插入注释心跳（`: heartbeat`），而 wb2api 的 StreamHint 对注释行是
// 原样透传（只写一行、不补空行），于是首块形如 ": heartbeat\ndata: {...}"。
// 若整块判前缀，带 name/首个 arguments 分片的那一帧会被整体丢弃 —— 表现为
// function_call 的 name 为空、arguments 缺开头，Codex 侧报 unsupported call。
func (s *respSink) drain() {
	for {
		i := bytes.Index(s.buf, []byte("\n\n"))
		if i < 0 {
			return
		}
		block := string(s.buf[:i])
		s.buf = s.buf[i+2:]
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			s.tr.onChunk([]byte(payload))
		}
	}
}

// finish 在所有上游逻辑结束后收尾：流式补 completed/failed 事件，
// 缓冲态则把 chat 的 JSON（成功或错误）整体翻译成 Responses 形态。
func (s *respSink) finish() {
	switch s.mode {
	case modeStream:
		s.tr.finalize()
	case modeBuffered:
		s.flushBuffered()
	default:
		// 既没开流也没写 body（chatCompletions 早期 return 的极端分支）：
		// 按状态码给出标准错误信封。
		if s.code >= 400 {
			writeOpenAIError(s.w, s.code, "upstream_error", http.StatusText(s.code))
			return
		}
		writeOpenAIError(s.w, http.StatusBadGateway, "upstream_parse", "empty upstream response")
	}
}

func (s *respSink) flushBuffered() {
	body := s.buf
	// 错误路径（非 200 或带 error 信封）：原样透传，Codex 认识 OpenAI 错误格式。
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(body, &probe)
	if s.code >= 400 || len(probe.Error) > 0 {
		if v := s.hdr.Get("X-Wb-Account"); v != "" {
			s.w.Header().Set("X-Wb-Account", v)
		}
		s.w.Header().Set("Content-Type", "application/json")
		s.w.WriteHeader(s.code)
		_, _ = s.w.Write(body)
		return
	}

	resp, err := chatCompletionToResponse(body, s.tr.model, s.tr.ns)
	if err != nil {
		writeOpenAIError(s.w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	if v := s.hdr.Get("X-Wb-Account"); v != "" {
		s.w.Header().Set("X-Wb-Account", v)
	}
	writeJSON(s.w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Chat chunk → Responses 事件
// ---------------------------------------------------------------------------

type outItem struct {
	index  int
	kind   string // "message" | "function_call" | "reasoning"
	id     string
	callID string
	name   string
	args   strings.Builder
	text   strings.Builder
	added  bool
}

type respTranslator struct {
	w         http.ResponseWriter
	fl        http.Flusher
	model     string
	stream    bool
	respID    string
	created   int64
	seq       int
	started   bool
	items     []*outItem
	msgItem   *outItem
	reasoning *outItem
	ns        map[string]nsRef // 扁平函数名 → namespace/原名
	tools     map[int]*outItem // chat tool_calls[].index → item
	usage     *respUsage
	finish    string
	failed    bool
	failCode  string
	failMsg   string
	done      bool
}

type respUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func newRespTranslator(w http.ResponseWriter, model string, stream bool, ns map[string]nsRef) *respTranslator {
	fl, _ := w.(http.Flusher)
	return &respTranslator{
		w:       w,
		fl:      fl,
		model:   model,
		stream:  stream,
		respID:  "resp_" + randHex(12),
		created: time.Now().Unix(),
		ns:      ns,
		tools:   map[int]*outItem{},
	}
}

// splitName 把扁平函数名还原成 (namespace, 原名)。命中不到映射的保持原样。
func (t *respTranslator) splitName(name string) (string, string) {
	return splitFlatName(t.ns, name)
}

func splitFlatName(ns map[string]nsRef, name string) (string, string) {
	if r, ok := ns[name]; ok {
		return r.Namespace, r.Name
	}
	return "", name
}

func (t *respTranslator) bindWriter(w http.ResponseWriter) {
	t.w = w
	t.fl, _ = w.(http.Flusher)
}

// emit 写出一个 Responses SSE 事件。同时给 event: 行（规范形态，事件解析器
// 既认 data.type 也认 event 名）。
func (t *respTranslator) emit(typ string, payload map[string]any) {
	if t.done {
		return
	}
	payload["type"] = typ
	payload["sequence_number"] = t.seq
	t.seq++
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var sb strings.Builder
	sb.WriteString("event: ")
	sb.WriteString(typ)
	sb.WriteString("\ndata: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	if _, err := io.WriteString(t.w, sb.String()); err != nil {
		return
	}
	if t.fl != nil {
		t.fl.Flush()
	}
}

// itemPayload 生成 output 数组中的 item 实体。
// function_call 若来自命名空间工具，必须带 namespace 字段 —— Codex 靠它把调用
// 路由到 mcp__cua_repl / mcp__node_repl 等工具组，缺了就是 unsupported call。
func (t *respTranslator) itemPayload(it *outItem, status string) map[string]any {
	switch it.kind {
	case "reasoning":
		return map[string]any{
			"id":      it.id,
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": it.text.String()}},
		}
	case "function_call":
		ns, short := t.splitName(it.name)
		m := map[string]any{
			"id":        it.id,
			"type":      "function_call",
			"call_id":   it.callID,
			"name":      short,
			"arguments": it.args.String(),
			"status":    status,
		}
		if ns != "" {
			m["namespace"] = ns
		}
		return m
	default: // message
		return map[string]any{
			"id": it.id, "type": "message", "role": "assistant", "status": status,
			"content": []any{map[string]any{
				"type": "output_text", "text": it.text.String(), "annotations": []any{},
			}},
		}
	}
}

func (t *respTranslator) baseResponse(status string) map[string]any {
	out := make([]map[string]any, 0, len(t.items))
	for _, it := range t.items {
		out = append(out, t.itemPayload(it, "in_progress"))
	}
	m := map[string]any{
		"id":                 t.respID,
		"object":             "response",
		"created_at":         t.created,
		"status":             status,
		"model":              t.model,
		"output":             out,
		"error":              nil,
		"incomplete_details": nil,
	}
	if t.usage != nil {
		m["usage"] = t.usage
	}
	return m
}

func (t *respTranslator) start() {
	if t.started {
		return
	}
	t.started = true
	t.emit("response.created", map[string]any{"response": t.baseResponse("in_progress")})
	t.emit("response.in_progress", map[string]any{"response": t.baseResponse("in_progress")})
}

func (t *respTranslator) onChunk(raw []byte) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	if e, ok := obj["error"]; ok && len(e) > 0 {
		t.failWith(string(e))
		return
	}
	t.start()

	if u, ok := obj["usage"]; ok && len(u) > 0 && string(u) != "null" {
		t.absorbUsage(u)
	}

	choicesRaw, ok := obj["choices"]
	if !ok {
		return
	}
	var choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	if json.Unmarshal(choicesRaw, &choices) != nil || len(choices) == 0 {
		return
	}
	c := choices[0]
	if c.FinishReason != "" {
		t.finish = c.FinishReason
	}
	// 思考增量先于正文增量处理：reasoning item 在 output 里必须排在 message 之前，
	// 按时序首次出现即建项可保证顺序正确。
	if c.Delta.ReasoningContent != "" {
		t.appendReasoning(c.Delta.ReasoningContent)
	} else if c.Message.ReasoningContent != "" {
		t.appendReasoning(c.Message.ReasoningContent)
	}
	// 个别上游在流里给 message 而非 delta（非标准兜底）。
	if c.Delta.Content == "" && c.Message.Content != "" {
		t.appendText(c.Message.Content)
	} else {
		t.appendText(c.Delta.Content)
	}
	for _, tc := range c.Delta.ToolCalls {
		t.appendToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
	}
	for _, tc := range c.Message.ToolCalls {
		t.appendToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
	}
}

func (t *respTranslator) absorbUsage(raw json.RawMessage) {
	var u struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	t.usage = &respUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	t.usage.InputTokensDetails.CachedTokens = u.PromptTokensDetails.CachedTokens
	t.usage.OutputTokensDetails.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
}

// appendReasoning 把上游的 reasoning_content 增量翻译成 Responses 的
// reasoning summary 事件链。
//
// 为什么用 summary 而不是 reasoning_text：Codex 只订阅 reasoning summary
// （请求里 reasoning.summary="auto"，include=["reasoning.encrypted_content"]），
// 它从 response.reasoning_summary_text.delta 取流式增量。Chat 协议没有「摘要」
// 这一层，只能把思考原文直接当摘要下发 —— 好处是内容完整，代价是可能比官方
// 摘要啰嗦（官方那条是二次生成的短摘要）。
func (t *respTranslator) appendReasoning(delta string) {
	if delta == "" {
		return
	}
	it := t.reasoning
	if it == nil {
		it = &outItem{index: len(t.items), kind: "reasoning", id: "rs_" + randHex(12)}
		t.items = append(t.items, it)
		t.reasoning = it
	}
	if !it.added {
		it.added = true
		t.emit("response.output_item.added", map[string]any{
			"output_index": it.index,
			"item": map[string]any{
				"id":      it.id,
				"type":    "reasoning",
				"summary": []any{},
			},
		})
		t.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id":       it.id,
			"output_index":  it.index,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		})
	}
	it.text.WriteString(delta)
	t.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id":       it.id,
		"output_index":  it.index,
		"summary_index": 0,
		"delta":         delta,
	})
}

func (t *respTranslator) appendText(delta string) {
	if delta == "" {
		return
	}
	if t.msgItem == nil {
		it := &outItem{
			index: len(t.items),
			kind:  "message",
			id:    "msg_" + randHex(12),
		}
		t.items = append(t.items, it)
		t.msgItem = it
	}
	it := t.msgItem
	if !it.added {
		it.added = true
		t.emit("response.output_item.added", map[string]any{
			"output_index": it.index,
			"item": map[string]any{
				"id":      it.id,
				"type":    "message",
				"role":    "assistant",
				"status":  "in_progress",
				"content": []any{},
			},
		})
		t.emit("response.content_part.added", map[string]any{
			"item_id":       it.id,
			"output_index":  it.index,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	it.text.WriteString(delta)
	t.emit("response.output_text.delta", map[string]any{
		"item_id":       it.id,
		"output_index":  it.index,
		"content_index": 0,
		"delta":         delta,
	})
}

func (t *respTranslator) appendToolCall(idx int, id, name, args string) {
	it, ok := t.tools[idx]
	if !ok {
		it = &outItem{
			index: len(t.items),
			kind:  "function_call",
			id:    "fc_" + randHex(12),
			name:  name,
		}
		if id != "" {
			it.callID = id
		} else {
			it.callID = newCallID()
		}
		t.items = append(t.items, it)
		t.tools[idx] = it
	}
	if it.name == "" && name != "" {
		it.name = name
	}
	if !it.added {
		it.added = true
		t.emit("response.output_item.added", map[string]any{
			"output_index": it.index,
			"item": map[string]any{
				"id":        it.id,
				"type":      "function_call",
				"call_id":   it.callID,
				"name":      it.name,
				"arguments": "",
				"status":    "in_progress",
			},
		})
	}
	if args == "" {
		return
	}
	it.args.WriteString(args)
	t.emit("response.function_call_arguments.delta", map[string]any{
		"item_id":      it.id,
		"output_index": it.index,
		"delta":        args,
	})
}

func (t *respTranslator) failWith(raw string) {
	t.failed = true
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(raw), &e)
	t.failMsg = e.Error.Message
	if t.failMsg == "" {
		t.failMsg = strings.TrimSpace(raw)
	}
	if c, ok := e.Error.Code.(string); ok {
		t.failCode = c
	} else {
		t.failCode = e.Error.Type
	}
	if t.failCode == "" {
		t.failCode = "upstream_error"
	}
}

// finalize 在 chatCompletions 返回后调用：补齐每个 item 的 done 事件与整体 completed。
func (t *respTranslator) finalize() {
	if t.done {
		return
	}
	if !t.started {
		// 一帧有效数据都没收到：通常是上游 200 空流，转成 failed 事件。
		if !t.failed {
			t.failed = true
			t.failCode = "upstream_parse"
			t.failMsg = "empty upstream stream"
		}
		t.start()
	}
	t.done = true

	for _, it := range t.items {
		switch it.kind {
		case "message":
			if !it.added {
				// 空消息也补一个 item，避免客户端拿到 output 为空的 completed。
				it.added = true
				t.emitRaw("response.output_item.added", map[string]any{
					"output_index": it.index,
					"item": map[string]any{
						"id": it.id, "type": "message", "role": "assistant",
						"status": "in_progress", "content": []any{},
					},
				})
			}
			text := it.text.String()
			t.emitRaw("response.output_text.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"content_index": 0,
				"text":          text,
			})
			t.emitRaw("response.content_part.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			})
			t.emitRaw("response.output_item.done", map[string]any{
				"output_index": it.index,
				"item": map[string]any{
					"id": it.id, "type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
				},
			})
		case "reasoning":
			text := it.text.String()
			if !it.added {
				it.added = true
				t.emitRaw("response.output_item.added", map[string]any{
					"output_index": it.index,
					"item":         map[string]any{"id": it.id, "type": "reasoning", "summary": []any{}},
				})
			}
			t.emitRaw("response.reasoning_summary_text.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"summary_index": 0,
				"text":          text,
			})
			t.emitRaw("response.reasoning_summary_part.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": text},
			})
			t.emitRaw("response.output_item.done", map[string]any{
				"output_index": it.index,
				"item":         t.itemPayload(it, "completed"),
			})
		case "function_call":
			args := it.args.String()
			ns, short := t.splitName(it.name)
			added := map[string]any{
				"id": it.id, "type": "function_call", "call_id": it.callID,
				"name": short, "arguments": "", "status": "in_progress",
			}
			if ns != "" {
				added["namespace"] = ns
			}
			if !it.added {
				it.added = true
				t.emitRaw("response.output_item.added", map[string]any{
					"output_index": it.index,
					"item":         added,
				})
			}
			t.emitRaw("response.function_call_arguments.done", map[string]any{
				"item_id":      it.id,
				"output_index": it.index,
				"arguments":    args,
			})
			t.emitRaw("response.output_item.done", map[string]any{
				"output_index": it.index,
				"item":         t.itemPayload(it, "completed"),
			})
		}
	}

	if t.failed {
		t.emitRaw("response.failed", map[string]any{
			"response": map[string]any{
				"id":         t.respID,
				"object":     "response",
				"created_at": t.created,
				"status":     "failed",
				"model":      t.model,
				"output":     []any{},
				"error": map[string]any{
					"code":    t.failCode,
					"message": t.failMsg,
				},
			},
		})
		return
	}
	t.emitRaw("response.completed", map[string]any{"response": t.completedResponse()})
}

// emitRaw 与 emit 相同但忽略 done 标志（finalize 内的收尾帧必须放行）。
func (t *respTranslator) emitRaw(typ string, payload map[string]any) {
	payload["type"] = typ
	payload["sequence_number"] = t.seq
	t.seq++
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var sb strings.Builder
	sb.WriteString("event: ")
	sb.WriteString(typ)
	sb.WriteString("\ndata: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	if _, err := io.WriteString(t.w, sb.String()); err != nil {
		return
	}
	if t.fl != nil {
		t.fl.Flush()
	}
}

func (t *respTranslator) completedResponse() map[string]any {
	out := make([]map[string]any, 0, len(t.items))
	for _, it := range t.items {
		out = append(out, t.itemPayload(it, "completed"))
	}
	m := map[string]any{
		"id":                 t.respID,
		"object":             "response",
		"created_at":         t.created,
		"status":             "completed",
		"model":              t.model,
		"output":             out,
		"error":              nil,
		"incomplete_details": nil,
	}
	if t.usage != nil {
		m["usage"] = t.usage
	}
	return m
}

// ---------------------------------------------------------------------------
// 非流式：chat.completion → response
// ---------------------------------------------------------------------------

func chatCompletionToResponse(body []byte, model string, ns map[string]nsRef) (map[string]any, error) {
	var cr struct {
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, err
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("upstream returned no choices")
	}
	if cr.Model != "" {
		model = cr.Model
	}
	created := cr.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	msg := cr.Choices[0].Message

	output := make([]map[string]any, 0, 3)
	// 顺序对齐流式：reasoning → message → function_call。
	if r := msg.ReasoningContent; r != "" {
		output = append(output, map[string]any{
			"id": "rs_" + randHex(12), "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": r}},
		})
	}
	if s := msg.Content; s != "" {
		output = append(output, map[string]any{
			"id": "msg_" + randHex(12), "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": s, "annotations": []any{}}},
		})
	}
	for _, tc := range msg.ToolCalls {
		id := tc.ID
		if id == "" {
			id = newCallID()
		}
		callNS, short := splitFlatName(ns, tc.Function.Name)
		item := map[string]any{
			"id": "fc_" + randHex(12), "type": "function_call", "call_id": id,
			"name": short, "arguments": tc.Function.Arguments, "status": "completed",
		}
		if callNS != "" {
			item["namespace"] = callNS
		}
		output = append(output, item)
	}
	if len(output) == 0 {
		output = append(output, map[string]any{
			"id": "msg_" + randHex(12), "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": "", "annotations": []any{},
			}},
		})
	}

	resp := map[string]any{
		"id":                 "resp_" + randHex(12),
		"object":             "response",
		"created_at":         created,
		"status":             "completed",
		"model":              model,
		"output":             output,
		"error":              nil,
		"incomplete_details": nil,
	}
	if cr.Usage.PromptTokens > 0 || cr.Usage.CompletionTokens > 0 {
		u := &respUsage{
			InputTokens:  cr.Usage.PromptTokens,
			OutputTokens: cr.Usage.CompletionTokens,
			TotalTokens:  cr.Usage.TotalTokens,
		}
		u.InputTokensDetails.CachedTokens = cr.Usage.PromptTokensDetails.CachedTokens
		u.OutputTokensDetails.ReasoningTokens = cr.Usage.CompletionTokensDetails.ReasoningTokens
		resp["usage"] = u
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func newCallID() string { return "call_" + randHex(10) }
