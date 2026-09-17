package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

func runtimeFixture(t *testing.T, raw string) (*runtimeConfigManager, *Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(livecfg.Snapshot{APIKey: c.APIKey, SoftCooldown: c.SoftRateDur, Default1MContext: c.Features.Default1MContext, SanitizeFingerprints: c.Features.SanitizeBlacklistFingerprints})
	return newRuntimeConfigManager(path, c, live, nil, nil, nil), c
}

func TestRuntimeInvalidSaveAndReloadRollback(t *testing.T) {
	m, _ := runtimeFixture(t, `{"api_key":"original"}`)
	before, _ := os.ReadFile(m.path)
	for _, raw := range []string{`null`, `[]`, `1`, `"x"`, `{"cooldown":{"soft_rate":"bad"}}`, `{"server":{"max_body_mb":0}}`} {
		if _, err := m.Save([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
		after, _ := os.ReadFile(m.path)
		if string(after) != string(before) || m.live.Load().APIKey != "original" {
			t.Fatal("failed save changed state")
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"api_key":"new","schedule":{"checkin_hours":[24]}}`} {
		if err := os.WriteFile(m.path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Reload(); err == nil {
			t.Fatalf("accepted reload %s", raw)
		}
		if m.live.Load().APIKey != "original" {
			t.Fatal("failed reload changed runtime")
		}
		after, _ := os.ReadFile(m.path)
		if string(after) != raw {
			t.Fatal("reload rewrote file")
		}
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(m.path), "*.tmp"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files leaked: %v %v", files, err)
	}
}

func TestRuntimeBaselineAndDeepMerge(t *testing.T) {
	m, startup := runtimeFixture(t, `{"extension":{"nested":{"keep":1}},"pool":{"unknown":true}}`)
	originalHour := startup.Schedule.CheckinHours[0]
	startup.Schedule.CheckinHours[0] = 0
	startup.Listen = ":1234"
	if m.baseline.Schedule.CheckinHours[0] != originalHour || m.baseline.Listen == startup.Listen {
		t.Fatal("baseline aliased startup")
	}
	for _, raw := range []string{`{}`, `{"listen":":7863","session_sticky":{"ttl":"1800s"}}`, `{"api_key":"hot","pool":{"max_in_flight":7},"extension":{"nested":{"added":2}}}`} {
		fields, err := m.Save([]byte(raw))
		if err != nil || len(fields) != 0 {
			t.Fatalf("same/hot config requires restart: %v %v", fields, err)
		}
	}
	raw, _ := os.ReadFile(m.path)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	nested := obj["extension"].(map[string]any)["nested"].(map[string]any)
	if nested["keep"] != float64(1) || nested["added"] != float64(2) || obj["pool"].(map[string]any)["unknown"] != true {
		t.Fatal("unknown keys lost")
	}
	fields, err := m.Save([]byte(`{"listen":":8888"}`))
	if err != nil || !reflect.DeepEqual(fields, []string{"listen"}) {
		t.Fatalf("fields=%v err=%v", fields, err)
	}
	fields, err = m.Reload()
	if err != nil || !reflect.DeepEqual(fields, []string{"listen"}) {
		t.Fatal("baseline advanced after save")
	}
	fields, err = m.Save([]byte(`{"listen":":7863"}`))
	if err != nil || len(fields) != 0 {
		t.Fatal("reversion still requires restart")
	}
}

func TestRuntimeRestartPromptMaxBodyAndUA(t *testing.T) {
	m, _ := runtimeFixture(t, `{}`)
	fields, err := m.Save([]byte(`{"server":{"max_body_mb":16},"upstream":{"user_agent":"custom","device_token":"token"},"prompt":{"mode":"passthrough"}}`))
	want := []string{"server.max_body_mb", "upstream.user_agent", "upstream.device_token", "prompt.mode", "prompt.text"}
	if err != nil || !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields=%v want=%v err=%v", fields, want, err)
	}
	promptPath := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(promptPath, []byte("first prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"prompt": map[string]any{"file": promptPath}})
	n, _ := runtimeFixture(t, string(raw))
	if err := os.WriteFile(promptPath, []byte("changed prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	fields, err = n.Reload()
	if err != nil || !reflect.DeepEqual(fields, []string{"prompt.text"}) {
		t.Fatalf("prompt text fields=%v err=%v", fields, err)
	}
}

func TestRuntimeEnvSaveReloadParity(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "env-key")
	t.Setenv("WB2A_USER_AGENT", "env-UA")
	t.Setenv("WB2A_MAX_BODY_MB", "32")
	m, _ := runtimeFixture(t, `{}`)
	fields, err := m.Save([]byte(`{"api_key":"disk-key","upstream":{"user_agent":"disk-UA"},"server":{"max_body_mb":16}}`))
	if err != nil || len(fields) != 0 || m.live.Load().APIKey != "env-key" {
		t.Fatalf("env save: %v %v", fields, err)
	}
	fields, err = m.Reload()
	if err != nil || len(fields) != 0 || m.live.Load().APIKey != "env-key" {
		t.Fatal("reload differs from save")
	}
	t.Setenv("WB2A_USER_AGENT", "changed-UA")
	fields, err = m.Reload()
	if err != nil || !reflect.DeepEqual(fields, []string{"upstream.user_agent"}) {
		t.Fatalf("env delta=%v %v", fields, err)
	}
	t.Setenv("WB2A_SOFT_RATE", "invalid")
	before, _ := os.ReadFile(m.path)
	snapshot := m.live.Load()
	if _, err = m.Save([]byte(`{"api_key":"bad"}`)); err == nil {
		t.Fatal("invalid env save accepted")
	}
	if _, err = m.Reload(); err == nil {
		t.Fatal("invalid env reload accepted")
	}
	after, _ := os.ReadFile(m.path)
	if string(before) != string(after) || m.live.Load() != snapshot {
		t.Fatal("invalid env changed state")
	}
}

func TestRuntimeConcurrentSaveReload(t *testing.T) {
	m, _ := runtimeFixture(t, `{}`)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw := []byte(fmt.Sprintf(`{"extension":{"key%d":%d}}`, i, i))
			var err error
			if i%2 == 0 {
				_, err = m.Save(raw)
			} else {
				_, err = saveConfigCompat(raw, m.path, m.live, nil, nil, nil)
			}
			if err != nil {
				t.Errorf("save: %v", err)
			}
			if _, err = m.Reload(); err != nil {
				t.Errorf("reload: %v", err)
			}
		}(i)
	}
	wg.Wait()
	raw, _ := os.ReadFile(m.path)
	obj, err := configObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(obj["extension"].(map[string]any)) != 30 {
		t.Fatal("concurrent merge lost updates")
	}
}
