// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		validEvents   int
		sawDone       bool // 上游显式发过 data: [DONE]（正常收尾）
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
		// toolSeq 缺 index 的 tool_call 的分配序号源：跨帧延续「最近分配」槽位，
		// 同帧内递增（见 mergeToolCallsChunk 注释）。
		toolSeq int
		// idIndex id → 已分配的 index：跨帧持续（延续碎片的 id 常出现在后续帧），
		// 供缺 index 时按 id 归位既有调用（见 mergeToolCallsChunk 注释）。
		idIndex = map[string]int{}
	)
	// appendContent 是「已取到正文」（gotAnyContent latch）的唯一写入点：delta 与
	// message 两路 content 都必须经此并入，规约只有一份——
	//   S1 空串不算「已取到正文」、不占 latch 名额：OpenAI 风格 role-only 首帧
	//      （delta.content=""）和整条空 message 帧是常态帧，若空串置位 latch，
	//      后续真正文会被 message 回退分支的 !gotAnyContent 守卫静默拒绝；
	//   S2 空串本身也无可追加字节，跳过 WriteString 与追加语义自洽。
	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}
	// mergeToolCallsChunk 把一段 tool_calls 数组按 index 合并进累计表。
	// delta（流式分片，按 index 累积）与 message（非 delta 整条）共用同一合并逻辑，
	// 保证「上游给的身份/函数名不丢、arguments 拼接语义一致」。
	//
	// index 缺失兼容：OpenAI 规范要求 delta 帧的 tool_call 带 index（标记分片归属），
	// 但部分上游省略它。此前缺 index 一律归 0——多调用场景下不同 call 被合并进同一
	// index 槽，arguments 串联、name 互相覆盖（数据污染）。修法按「id 优先、lastIdx 兜底」：
	//   - 带 index → 按 index 累积（合规形态，零改动）；
	//   - 缺 index 带 id 且 id 已见过 → 延续该 id 所在 index；
	//   - 缺 index 带 id 且 id 是新的 → 开新序号（多调用不合并）；
	//   - 缺 index 无 id → 追加到最近收到碎片的 index（单个调用的延续分片无 id
	//     是标准形态），无既往则开新号。
	// 带 index 的碎片照常按 index 累积，不受影响。
	// nextToolIndex 分配下一个不冲突的缺 index 序号：从 toolSeq 起递增跳过既有
	// index（合规流的 index 是 0..N-1，缺 index 的补位不能覆盖它们）。
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	mergeToolCallsChunk := func(tcs []any) {
		for _, tc := range tcs {
			call, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			idx := -1
			if v, ok := call["index"].(float64); ok {
				idx = int(v)
			} else if cid, _ := call["id"].(string); cid != "" {
				if mid, seen := idIndex[cid]; seen {
					idx = mid // 该 id 已归位过：延续既有调用（跨帧有效）
				} else {
					idx = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1] // 无 id 碎片：延续最近调用
			} else {
				idx = nextToolIndex()
			}
			merged, seen := toolCalls[idx]
			if !seen {
				merged = map[string]any{"index": idx}
				toolCalls[idx] = merged
				toolOrder = append(toolOrder, idx)
			}
			if cid, _ := call["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			if cid, _ := merged["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			mergeToolCallDelta(merged, call)
		}
	}
	// mergeMessageFields 把非 delta 的完整 message 内容并入聚合（message 是整条下发，
	// 非流式拼接，content 只取一次）。role/reasoning_content/tool_calls 与 delta 分支
	// 同构透出；content 同样经 appendContent 置 latch，与 delta 路径的 latch 语义一致
	// （一帧整条 message 之后，后续 delta 帧不重复追加）。
	mergeMessageFields := func(msg map[string]any) {
		if r2, ok := msg["role"].(string); ok && r2 != "" {
			role = r2
		}
		if txt, ok := msg["content"].(string); ok {
			appendContent(txt)
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			reasoning.WriteString(rc)
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			mergeToolCallsChunk(tcs)
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
				sawDone = true
				break
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					// 有效事件计数：仅 JSON 解析成功的数据帧计入（解析失败沿用静默 continue）。
					validEvents++
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									appendContent(txt)
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									mergeToolCallsChunk(tcs)
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）：
							// 整条并入（role/content/reasoning_content/tool_calls），
							// 与 delta 分支同构。delta 已取过正文（gotAnyContent）则跳过
							// （避免与 delta 路径重复拼接——latch 语义）。
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								mergeMessageFields(msg)
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, fmt.Errorf("upstream stream contained no valid data events")
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		// P1b：流被截断时 tool_call 的 arguments 是残缺 JSON（解析失败），不把脏参数
		// 交给客户端——残留分片会被客户端解析成非法 JSON 卡死会话。截断的两个来源：
		//   - finish_reason=="length"（模型因 max_tokens 提前中止）；
		//   - 上游连接中断（EOF 收尾但未发 data: [DONE]，sawDone=false）。
		// 完整参数原样保留（正例零改动）；空参数（无参工具）不是截断，同样保留。
		if finishReason == "length" || !sawDone {
			calls = dropTruncatedToolCalls(calls)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		// OpenAI 非流式 usage 必含 total_tokens。上游若只发 prompt_tokens +
		// completion_tokens（部分上游末帧缺 total），网关合成补齐——否则严格按
		// schema 校验的客户端收不到 total_tokens。已有 total 或二者缺一不补
		// （不臆造：单边有值无法合成可信的 total）。
		resp["usage"] = ensureUsageTotal(usage)
	}
	return resp, nil
}

// ensureUsageTotal 在 usage 缺 total_tokens 但 prompt_tokens/completion_tokens 都在时
// 补齐 total = prompt + completion（通过新 map 合并，不修改原上游 map）。
// 任一缺失或已有 total 时原样返回。
func ensureUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := num64(u["prompt_tokens"])
	ct, cok := num64(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
	return out
}

// num64 把 JSON number（float64/int64 均可）归一为 float64；非数字返回 ok=false。
//
// 注意与 payload.go 的类型 switch 语义不同：那边是翻译请求别名字段（非数字拒绝
// 整个请求），这边是聚合响应求和（非数字只跳过合成）。防后人合并两处。
func num64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
// 流式策略：逐帧透传（规范化已剥空 content 噪声），恢复与上游一致的平滑流式。
func Stream(w http.ResponseWriter, r io.Reader) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
	// 仅 JSON 解析成功时计数记为一次有效转发（JSON 解析失败照常降级原样写出，但不计数）。
	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		valid := 0
		if json.Unmarshal([]byte(payload), &obj) == nil {
			if raw, err := json.Marshal(normalizeFrame(obj)); err == nil {
				payload = string(raw)
			}
			valid = 1
		}
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return 0, werr
		}
		if fl != nil {
			fl.Flush()
		}
		return valid, nil
	}

	// writeRaw 原样写出一帧（绕过 normalizeFrame）并 flush。空流错误帧需保留 error 字段，
	// 不能被白名单剥掉，故不经 writeFrame 规范化。
	writeRaw := func(payload string) error {
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	validFrames := 0
readLoop:
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data: [DONE]"):
			// 上游显式结束：停止读取，DONE 之后的任何数据（含垃圾帧）一律不再透传。
			// [DONE] 统一在循环结束后写出，保证恰好一个。
			break readLoop
		case strings.HasPrefix(trimmed, "data: "):
			n, werr := writeFrame(strings.TrimPrefix(trimmed, "data: "))
			validFrames += n
			if werr != nil {
				return werr
			}
		case trimmed != "":
			// 注释/其他行：原样透传
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	// 空流（0 有效帧）：先写一帧 error（绕过 normalizeFrame 原样保留 error 字段），
	// 再补 [DONE] 保证客户端能正常收尾，并返回非 nil error 供调用方记录。
	if validFrames == 0 {
		_ = writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error"}}`)
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if validFrames == 0 {
		return fmt.Errorf("upstream stream contained no valid data events")
	}
	return nil
}
