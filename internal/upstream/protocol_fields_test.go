package upstream

import (
	"encoding/json"
	"testing"
)

func decodeProtocolBody(t *testing.T, body string) map[string]any {
	t.Helper()
	out := PrepareBodyOpt([]byte(body), false)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal prepared body: %v; body=%s", err, out)
	}
	return got
}

func TestMaxCompletionTokensCompatibility(t *testing.T) {
	got := decodeProtocolBody(t, `{"model":"glm-5.2","messages":[],"max_completion_tokens":4096}`)
	if got["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens=%v, want 4096", got["max_tokens"])
	}
	if _, exists := got["max_completion_tokens"]; exists {
		t.Fatal("max_completion_tokens should be removed")
	}

	got = decodeProtocolBody(t, `{"model":"glm-5.2","messages":[],"max_tokens":123,"max_completion_tokens":4096}`)
	if got["max_tokens"] != float64(123) {
		t.Fatalf("explicit max_tokens=%v, want 123", got["max_tokens"])
	}
}

func TestStreamOptionsUsageCompatibility(t *testing.T) {
	got := decodeProtocolBody(t, `{"model":"glm-5.2","messages":[]}`)
	opt, ok := got["stream_options"].(map[string]any)
	if !ok || opt["include_usage"] != true {
		t.Fatalf("stream_options=%#v, want include_usage=true", got["stream_options"])
	}

	got = decodeProtocolBody(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false,"x":1}}`)
	opt, ok = got["stream_options"].(map[string]any)
	if !ok || opt["include_usage"] != false || opt["x"] != float64(1) {
		t.Fatalf("explicit stream_options overwritten: %#v", got["stream_options"])
	}

	for _, malformed := range []string{"null", `"bad"`, "123"} {
		got = decodeProtocolBody(t, `{"model":"glm-5.2","messages":[],"stream_options":`+malformed+`}`)
		opt, ok = got["stream_options"].(map[string]any)
		if !ok || opt["include_usage"] != true {
			t.Fatalf("malformed stream_options %s not normalized: %#v", malformed, got["stream_options"])
		}
	}
}
