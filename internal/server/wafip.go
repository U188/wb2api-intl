// wafip.go 按站点隔离的 WAF IP 级 fail-fast 状态机。
// CN 与 INTL 绝不共享命中窗口：一个通道被拦不会阻断另一个通道。
package server

import (
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

var wafIPWindow = 60 * time.Second

const wafIPThreshold = 2

type wafIPGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time
	until time.Time
}

func (g *wafIPGate) note(uid string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.until) {
		return true
	}
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	g.hits[uid] = now
	for u, t := range g.hits {
		if now.Sub(t) > wafIPWindow {
			delete(g.hits, u)
		}
	}
	if len(g.hits) >= wafIPThreshold {
		g.until = now.Add(wafIPWindow)
		g.hits = map[string]time.Time{}
		return true
	}
	return false
}

func (g *wafIPGate) active(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return now.Before(g.until)
}

type siteWAFGates struct {
	cn   wafIPGate
	intl wafIPGate
}

func (g *siteWAFGates) forSite(site string) *wafIPGate {
	if auth.SiteFrom(site) == auth.SiteIntl {
		return &g.intl
	}
	return &g.cn
}
