package session

import (
	"runtime"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// routerWithGC 同 routerWith，但显式注入 GCInterval。GCInterval 必须在 StartGC
// **之前**注入：StartGC 之后写 r.cfg.GCInterval 会与 GC goroutine 里的
// time.NewTicker(r.cfg.GCInterval) 竞争（cfg 非并发安全），测试自身即犯规。
func routerWithGC(store redisstore.Store, avail []string, ttl, gcInterval time.Duration) *Router {
	return New(Config{
		TTL:        ttl,
		GCInterval: gcInterval,
		Store:      store,
		Available:  func() []string { return avail },
	})
}

// TestStopGCStopsGoroutine 关停语义回归：StopGC 后后台 GC goroutine 必须真的退出
// （N 轮「StartGC → StopGC」不泄漏 goroutine）。
//
// 原实现缺陷：goroutine 在 select 里**每轮无锁重读** r.stop，而 StopGC 持写锁把它
// 置 nil —— 既是数据竞争（-race 报 Write@StopGC vs Read@StartGC.func1），又会在读到
// nil 后让该 case 永久阻塞（nil channel 永不就绪），关停彻底失效、goroutine 泄漏。
// 修复后 stop channel 在启 goroutine 前捕获到局部变量，close 与 select 观测同一
// channel。本用例在 -race 下必然抓住回归。
func TestStopGCStopsGoroutine(t *testing.T) {
	r := routerWithGC(redisstore.Noop{}, []string{"a1"}, time.Minute, time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		r.StartGC()
		r.StopGC()
	}
	// 给被关闭的 goroutine 一点退出时间。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("GC goroutine 泄漏：before=%d after=%d", before, after)
	}
}

// TestStartGCIdempotent 幂等：重复 StartGC 不重复起 goroutine，StopGC 幂等可重入。
func TestStartGCIdempotent(t *testing.T) {
	r := routerWithGC(redisstore.Noop{}, []string{"a1"}, time.Minute, time.Hour)
	before := runtime.NumGoroutine()
	r.StartGC()
	r.StartGC()
	r.StartGC()
	time.Sleep(20 * time.Millisecond)
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Errorf("StartGC 非幂等：before=%d after=%d", before, got)
	}
	r.StopGC()
	r.StopGC()
}
