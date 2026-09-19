// sse_upstream_test.go 吸收上游 Sliverkiss/workbuddy2api 的 SSE 聚合回归用例：
// tool_call 缺 index 分派（5c2db2fc）、EOF 截断丢弃（65b2f33f）、
// usage total 合成（213e3627）、非 delta message 不重复（6701631c）。
package upstream

import (
	"strings"
	"testing"
)

func TestAggregateToolCallsNoIndexNotCollapsed(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"f2","arguments":"{\"b\":2}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("two distinct tool_calls must not collapse: %#v", msg["tool_calls"])
	}
	// 两个 call 各自独立：arguments 不串联、name 不被覆盖。
	argSets := map[string]bool{}
	names := map[string]bool{}
	for _, c := range calls {
		fn := c["function"].(map[string]any)
		argSets[fn["arguments"].(string)] = true
		names[fn["name"].(string)] = true
	}
	if !argSets[`{"a":1}`] || !argSets[`{"b":2}`] {
		t.Errorf("arguments must stay separate, got %v", argSets)
	}
	if !names["f1"] || !names["f2"] {
		t.Errorf("names must stay separate, got %v", names)
	}
}

// TestAggregateToolCallsMixedIndexAbsent 混合形态：全流共用一个 index-0 的合规 call，
// 中途混入一个缺 index 的新 call——缺 index 的补位不得覆盖既有 index-0 槽
// （nextToolIndex 从 toolSeq 递增跳过既有 index）。
func TestAggregateToolCallsMixedIndexAbsent(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"f2","arguments":"{\"b\":2}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("mixed index forms must produce 2 calls: %#v", msg["tool_calls"])
	}
	// 两个调用 index 互异（新分配的补位序号 ≠ 既有 0）。
	seen := map[int]bool{}
	for _, c := range calls {
		seen[c["index"].(int)] = true
	}
	if len(seen) != 2 || !seen[0] {
		t.Errorf("indexes=%v want two distinct incl. 0", seen)
	}
}

// TestAggregateToolCallsIdCarriedAcrossFrames 上游缺 index 但每帧都带同 id 的
// 延续形态：call_a 的碎片跨两帧（id 都带）必须归位到同一调用，不因跨帧拆分。
func TestAggregateToolCallsIdCarriedAcrossFrames(t *testing.T) {
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"f1","arguments":"{\"a\":"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","function":{"arguments":"1}"}}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("same-id continuation across frames must stay one call: %#v", msg["tool_calls"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["arguments"] != `{"a":1}` {
		t.Errorf("arguments=%q want {\"a\":1}", fn["arguments"])
	}
}

// TestAggregateEOFDropsTruncatedToolCalls RED：上游流被掐断（EOF 收尾、无 [DONE]、
// 无 finish_reason）时，tool_calls 的 arguments 是残缺 JSON。规约：截断来源
// 除 finish_reason=="length" 外还包括连接中断（sawDone=false）——残缺调用必须
// 丢弃，否则客户端解析非法 JSON 卡死会话；正常 [DONE] 收尾的完整调用不受影响。
func TestAggregateEOFDropsTruncatedToolCalls(t *testing.T) {
	raw := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_a","type":"function","function":{"name":"bash","arguments":"{\"cmd\":"},"index":0}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"ls\""},"index":0}]}}]}` // EOF 截断，无 DONE
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("EOF-truncated stream must still aggregate: %v", err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if calls, ok := msg["tool_calls"].([]map[string]any); ok && len(calls) != 0 {
		t.Fatalf("truncated tool_calls must be dropped: %#v", msg["tool_calls"])
	}
}

// TestAggregateDoneKeepsCompleteToolCall 回归：正常 [DONE] 收尾的完整 tool_calls
// 不被 dropTruncatedToolCalls 误伤（对照 EOF 分支，sawDone 不应触发丢弃）。
func TestAggregateDoneKeepsCompleteToolCall(t *testing.T) {
	raw := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"},"index":0}]}}]}
data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("complete tool_calls must be kept: %#v", msg["tool_calls"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("args=%v", fn["arguments"])
	}
}

// TestEnsureUsageTotalSynthesizesMissingTotal RED：上游末帧 usage 缺 total_tokens
// 但 prompt_tokens/completion_tokens 都在时，非流式聚合必须补齐 total（OpenAI
// 非流式 usage 必含该字段）。已有 total / 缺单边 / 无 usage 三形态保持原样。
func TestEnsureUsageTotalSynthesizesMissingTotal(t *testing.T) {
	// 缺 total：补齐
	syn := ensureUsageTotal(map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(5)})
	if syn["total_tokens"] != float64(15) {
		t.Errorf("synthesized total=%v want 15", syn["total_tokens"])
	}
	// 已有 total：不覆盖
	keep := ensureUsageTotal(map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(5), "total_tokens": float64(100)})
	if keep["total_tokens"] != float64(100) {
		t.Errorf("existing total must not be overridden: %v", keep["total_tokens"])
	}
	// 缺单边：不合成
	half := ensureUsageTotal(map[string]any{"prompt_tokens": float64(10)})
	if _, ok := half["total_tokens"]; ok {
		t.Errorf("must not synthesize with only one side present: %v", half)
	}
	// 集成：SSE 末帧 usage 缺 total → 聚合响应含补齐的 total
	raw := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}
data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}
data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	u := resp["usage"].(map[string]any)
	if u["total_tokens"] != float64(15) {
		t.Errorf("aggregated usage total=%v want 15 (integration)", u["total_tokens"])
	}
	// 集成不可变：合成走新 map，原上游 usage map 不被改写。
	src := map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(5)}
	_ = ensureUsageTotal(src)
	if _, polluted := src["total_tokens"]; polluted {
		t.Errorf("ensureUsageTotal must not mutate the input map: %#v", src)
	}
}

func TestAggregateNonDeltaMessageContentNotDuplicated(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"abc\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"abc\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"abc\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "abc" {
		t.Errorf("content=%q want %q（message 回退分支应只采一次）", msg["content"], "abc")
	}
}
