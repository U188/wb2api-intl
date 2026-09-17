// degrade.go 仅针对未知客户端/传输失败的连败临时出池，不与已分类的
// 429/WAF/402/5xx 惩罚叠加。单账号 entry 自带 site，天然双通道隔离。
package pool

import "time"

// NoteFailures 记录一次无权威分类的失败。达到阈值后临时出池；在降权期
// 内再次达阈不延长截止。成功时由 NoteSuccess 清零计数和截止。
func (p *Pool) NoteFailures(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byUID[uid]
	if e == nil {
		return
	}
	e.consecutiveFails++
	threshold := p.degradeThreshold
	if threshold <= 0 {
		threshold = defaultDegradeThreshold
	}
	if e.consecutiveFails < threshold {
		p.dirty.Store(true)
		return
	}
	e.consecutiveFails = 0
	now := time.Now()
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		p.dirty.Store(true)
		return
	}
	d := p.degradeCooldown
	if d <= 0 {
		d = defaultDegradeCooldown
	}
	max := p.degradeCooldownMax
	if max <= 0 {
		max = defaultDegradeCooldownMax
	}
	if d > max {
		d = max
	}
	e.degradeUntil = now.Add(d)
	p.dirty.Store(true)
}
