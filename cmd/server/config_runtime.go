package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// baseline is the effective startup configuration, never the last saved config.
// The shared transaction lock also serializes the legacy saveConfig wrapper.
var configTransactionMu sync.Mutex

type runtimeConfigManager struct {
	mu       sync.Mutex
	path     string
	baseline *Config
	live     *livecfg.Holder
	pool     *pool.Pool
	up       *upstream.Client
	sch      *scheduler.Scheduler
}

func newRuntimeConfigManager(path string, startup *Config, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) *runtimeConfigManager {
	return &runtimeConfigManager{path: path, baseline: cloneRuntimeConfig(startup), live: live, pool: p, up: up, sch: sch}
}

func cloneRuntimeConfig(c *Config) *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Schedule.CheckinHours = append([]int(nil), c.Schedule.CheckinHours...)
	out.Schedule.TravelHours = append([]int(nil), c.Schedule.TravelHours...)
	out.Schedule.ActivityHours = append([]int(nil), c.Schedule.ActivityHours...)
	out.Schedule.KeepaliveHours = append([]int(nil), c.Schedule.KeepaliveHours...)
	out.Schedule.BlackcatHours = append([]int(nil), c.Schedule.BlackcatHours...)
	return &out
}

func (m *runtimeConfigManager) Save(raw []byte) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	configTransactionMu.Lock()
	defer configTransactionMu.Unlock()
	return m.saveLocked(raw)
}

func (m *runtimeConfigManager) Reload() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	configTransactionMu.Lock()
	defer configTransactionMu.Unlock()
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if _, err := configObject(raw); err != nil {
		return nil, err
	}
	c, err := loadConfigBytes(raw)
	if err != nil {
		return nil, err
	}
	applyManagedRuntimeConfig(c, m.live, m.pool, m.up, m.sch)
	return changedRestartFields(m.baseline, c), nil
}

// loadConfigBytes 与 Load 的解析/env/normalize 顺序一致，但只消费同一份磁盘字节。
func loadConfigBytes(raw []byte) (*Config, error) {
	c := Default()
	if _, err := ParseConfigInto(raw, c); err != nil {
		return nil, err
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func configObject(raw []byte) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("parse config object: %w", err)
	}
	if obj == nil {
		return nil, fmt.Errorf("config must be a non-null JSON object")
	}
	return obj, nil
}

func (m *runtimeConfigManager) saveLocked(raw []byte) ([]string, error) {
	incoming, err := configObject(raw)
	if err != nil {
		return nil, err
	}
	old, err := os.ReadFile(m.path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	cur, err := configObject(old)
	if err != nil {
		return nil, fmt.Errorf("current config: %w", err)
	}
	out, err := json.MarshalIndent(mergeRuntimeConfigMaps(cur, incoming), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	// Load the staged file: exactly the startup validation and env precedence.
	f, err := os.CreateTemp(filepath.Dir(m.path), "."+filepath.Base(m.path)+"-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create config temporary file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("chmod config: %w", err)
	}
	if _, err := f.Write(out); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("sync config: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close config: %w", err)
	}
	c, err := loadConfigBytes(out)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return nil, fmt.Errorf("replace config: %w", err)
	}
	applyManagedRuntimeConfig(c, m.live, m.pool, m.up, m.sch)
	return changedRestartFields(m.baseline, c), nil
}

// saveConfigCompat is the implementation for main.go's legacy saveConfig wrapper.
func saveConfigCompat(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	configTransactionMu.Lock()
	defer configTransactionMu.Unlock()
	startup, err := Load(path)
	if err != nil {
		return nil, err
	}
	m := newRuntimeConfigManager(path, startup, live, p, up, sch)
	return m.saveLocked(raw)
}

func mergeRuntimeConfigMaps(cur, incoming map[string]any) map[string]any {
	if cur == nil {
		cur = make(map[string]any)
	}
	for k, v := range incoming {
		if in, ok := v.(map[string]any); ok {
			old, _ := cur[k].(map[string]any)
			cur[k] = mergeRuntimeConfigMaps(old, in)
		} else {
			cur[k] = v
		}
	}
	return cur
}

func changedRestartFields(startup, current *Config) []string {
	if startup == nil || current == nil {
		return nil
	}
	var out []string
	add := func(key string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, key)
		}
	}
	add("listen", startup.Listen, current.Listen)
	add("auth_dir", startup.AuthDir, current.AuthDir)
	add("state_file", startup.StateFile, current.StateFile)
	add("server.max_body_mb", startup.Server.MaxBodyMB, current.Server.MaxBodyMB)
	// Enumerate each upstream JSON field independently, including UA/device headers.
	a, b := reflect.ValueOf(startup.Upstream), reflect.ValueOf(current.Upstream)
	for i := 0; i < a.NumField(); i++ {
		key := strings.Split(a.Type().Field(i).Tag.Get("json"), ",")[0]
		add("upstream."+key, a.Field(i).Interface(), b.Field(i).Interface())
	}
	add("upstash.url", startup.Upstash.URL, current.Upstash.URL)
	add("upstash.token", startup.Upstash.Token, current.Upstash.Token)
	add("session_sticky.enabled", startup.SessionSticky.Enabled, current.SessionSticky.Enabled)
	add("session_sticky.ttl", startup.SessionTTL, current.SessionTTL)
	add("session_sticky.gc_interval", startup.SessionGCInterval, current.SessionGCInterval)
	add("prompt.mode", startup.Prompt.Mode, current.Prompt.Mode)
	add("prompt.file", startup.Prompt.File, current.Prompt.File)
	add("prompt.text", startup.PromptText, current.PromptText)
	return out
}

func applyManagedRuntimeConfig(c *Config, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) {
	if live != nil {
		live.Store(livecfg.Snapshot{APIKey: c.APIKey, SoftCooldown: c.SoftRateDur, SanitizeFingerprints: c.Features.SanitizeBlacklistFingerprints, Default1MContext: c.Features.Default1MContext})
	}
	if up != nil {
		up.SetSanitizeFingerprints(c.Features.SanitizeBlacklistFingerprints)
	}
	if p != nil {
		p.SetBreaker(c.Pool.BreakerThreshold, c.BreakerCooldownDur, c.BreakerCooldownMaxD)
		p.SetDegrade(c.Pool.DegradeThreshold, c.DegradeCooldownDur, c.DegradeCooldownMaxD)
		p.SetMaxInFlight(c.Pool.MaxInFlight)
		p.SetSoftRateMax(c.SoftRateMaxDur)
		p.SetWeights(c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if sch != nil {
		sch.Reconfigure(c.Schedule.CheckinHours, c.Schedule.TravelHours, c.Schedule.ActivityHours, c.Schedule.KeepaliveHours, c.Schedule.BlackcatHours, !c.Schedule.CheckinEnabled, !c.Schedule.TravelEnabled, !c.Schedule.ActivityEnabled, !c.Schedule.KeepaliveEnabled, !c.Schedule.BlackcatEnabled)
		sch.SetBalanceInterval(c.BalanceRefreshInterval)
	}
}
