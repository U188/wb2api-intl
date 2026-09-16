package upstream

import (
	"reflect"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestIntlBuiltinModelsOfficialCLIContract(t *testing.T) {
	type want struct {
		input, output, allowed int64
		credits                string
		efforts                []string
		defaultEffort          string
		canDisable             bool
		reasoning              bool
	}
	wants := map[string]want{
		"default-model":       {176000, 24000, 80000, "x0.79 credits", nil, "", false, true},
		"fast-model":          {200000, 32000, 200000, "x0.34 credits", nil, "medium", false, true},
		"balanced-model":      {256000, 32000, 256000, "x0.59 credits", nil, "medium", false, true},
		"primary-model":       {272000, 72000, 272000, "x3.31 credits", nil, "high", false, true},
		"deep-model":          {176000, 24000, 200000, "x3.33 credits", nil, "", false, false},
		"gpt-5.6-sol":         {1000000, 128000, 1000000, "x3.47 credits", []string{"low", "medium", "high", "xhigh"}, "high", false, true},
		"gpt-5.6-terra":       {1000000, 128000, 1000000, "x1.39 credits", []string{"low", "medium", "high", "xhigh"}, "high", false, true},
		"gpt-5.6-luna":        {1000000, 128000, 1000000, "x0.14 credits", []string{"low", "medium", "high", "xhigh"}, "high", false, true},
		"gpt-5.5":             {1000000, 72000, 1000000, "x3.31 credits", nil, "high", false, true},
		"gpt-5.4":             {272000, 128000, 0, "x1.65 credits", nil, "high", false, true},
		"gpt-5.3-codex":       {272000, 128000, 0, "x1.25 credits", nil, "high", false, true},
		"gemini-3.5-flash":    {1000000, 65536, 1000000, "x0.99 credits", nil, "medium", false, true},
		"deepseek-v4.1-flash": {1000000, 50000, 1000000, "x0.06 credits", nil, "high", false, true},
		"glm-5.3":             {1000000, 48000, 1000000, "x0.79 credits", []string{"low", "high", "max"}, "high", true, true},
		"glm-5.2":             {1000000, 48000, 1000000, "x0.79 credits", []string{"high", "xhigh"}, "high", true, true},
		"kimi-k3":             {1000000, 32000, 1000000, "x1.62 credits", nil, "medium", false, true},
		"kimi-k2.6":           {256000, 32000, 256000, "x0.52 credits", nil, "medium", false, true},
		"minimax-m3":          {512000, 128000, 512000, "x0.25 credits", nil, "medium", false, true},
	}
	models := IntlBuiltinModels()
	if len(models) != len(wants) {
		t.Fatalf("catalog length=%d want=%d", len(models), len(wants))
	}
	seen := make(map[string]bool, len(models))
	oneMCount := 0
	for _, m := range models {
		w, ok := wants[m.ID]
		if !ok {
			t.Errorf("unexpected model %q", m.ID)
			continue
		}
		if seen[m.ID] {
			t.Errorf("duplicate model %q", m.ID)
		}
		seen[m.ID] = true
		if m.ContextWindow != w.input || m.MaxTokens != w.output || m.MaxAllowedSize != w.allowed ||
			m.Credits != w.credits || !reflect.DeepEqual(m.Efforts, w.efforts) ||
			m.DefaultEffort != w.defaultEffort || m.CanDisableThinking != w.canDisable ||
			m.SupportsReasoning != w.reasoning {
			t.Errorf("model %s mismatch: %#v want %#v", m.ID, m, w)
		}
		if w.input >= 1_000_000 && w.allowed >= 1_000_000 {
			oneMCount++
			if !reflect.DeepEqual(m.ContextLengths, []int64{200_000, 1_000_000}) || m.DefaultContextLength != 200_000 {
				t.Errorf("model %s 1M choices=%v canonical default=%d", m.ID, m.ContextLengths, m.DefaultContextLength)
			}
		} else if len(m.ContextLengths) != 0 || m.DefaultContextLength != 0 {
			t.Errorf("model %s unexpectedly exposes context choices=%v default=%d", m.ID, m.ContextLengths, m.DefaultContextLength)
		}
	}
	if oneMCount != 9 {
		t.Fatalf("1M-selectable international models=%d want=9", oneMCount)
	}

}
func TestFetchModelsNilAccountReturnsError(t *testing.T) {
	if _, err := New().FetchModels(nil); err == nil {
		t.Fatal("FetchModels(nil) must return an error")
	}
}

func TestIntlEffortsAvailableBeforeModelsRequest(t *testing.T) {
	c := New()
	efforts := c.effortsSnapshot(auth.SiteIntl)
	if got := efforts["glm-5.2"]; !reflect.DeepEqual(got, []string{"high", "xhigh"}) {
		t.Fatalf("startup intl effort catalog=%v", got)
	}
}
