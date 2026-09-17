// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List）。
package pool

import (
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		p.dirty.Store(true)
	}
}

// NoteSessionDead 记录一次 ErrSessionDead（12153）——**不立即禁用**。
// 旧行为一次 12153 即 Disable，但 12153 会被临时性触发（网络抖动/上游闪断/refresh
// 竞态），一次失败就永久杀号会误杀健康账号（P0-1 侦察：13 个 disabled 号全部 refresh
// 成功，是历史误判的受害者）。改为连续 sessionDeadThreshold 次才禁用：
// 计数 +1，达到阈值 → Disable（reason=12153 session dead）并清计数；
// refresh 成功 / 任意成功 / 手工复活 → ClearSessionDead 清计数。
// 返回 true 表示本次已达阈值并完成禁用。
// 即使账号已 disabled，计数仍累计并返回 false 前 N-1 次——但 keepalive 会跳过
// disabled 号，实际只有「已 disabled 后复活且计数未清」这类场景才会走到这里。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		return false
	}
	e.disabled = true
	e.reason = sessionDeadReason
	e.sessionDeadFails = 0
	p.dirty.Store(true)
	return true
}

// ClearSessionDead 清连续 12153 计数——账号被证明未死的任何时刻调用：
// refresh 成功（RunKeepaliveNow）、chat 成功（NoteSuccess）、手工复活（ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.sessionDeadFails = 0
	}
}

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = false
		e.reason = ""
		e.sessionDeadFails = 0
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
		p.dirty.Store(true)
	}
}

// Revive 运维口径的"无条件恢复"：清禁用、冷却（含软退避计数）与熔断运行态。
// 与 ReviveDisabled（只清禁用）和 ReenableIfCredits（只清冷却、不动熔断）的区别：
// 本方法清除全部惩罚状态，供管理面板"解冻"按钮使用——人工判断该号可用时一键恢复。
// uid 不存在返回 false（供调用方区分"账号不存在"与"已复活"）。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.softRateModel = "" // 模型级限流豁免随冷却一并清（防泄漏到后续账号级限流）
	e.sessionDeadFails = 0
	e.fails = 0
	e.retryCount = 0
	e.breakerUntil = time.Time{}
	e.consecutiveFails = 0
	e.degradeUntil = time.Time{}
	p.dirty.Store(true)
	return true
}

// reviveCoolingLocked 只清冷却（until/coolKind/reason/softStreak）并更新 credits，不动熔断器
// （fails/retryCount/breakerUntil）。签到解冻走这里：签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx 信号）不应被签到覆盖。
// softStreak 属**冷却域**（与 until/coolKind 同域），故随冷却一并清零——与"解冻只清冷却
// 不清熔断"的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是历史软冷却累积。
// 调用方必须已持有 p.mu。
func (p *Pool) ReenableIfCredits(uid string, remain, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
			p.reviveCoolingLocked(e, remain, total)
		} else {
			e.credits = remain
			e.creditsTotal = total
		}
		p.dirty.Store(true)
	}
}

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal。
// 达到 breakerThreshold 触发熔断（指数退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails：成功证明 session 未死（与 ClearSessionDead 语义一致）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
		p.dirty.Store(true)
	}
}

// RecordTokenUsage 记录一次实际发起的聊天账号尝试及上游返回的 usage 增量。
// usage 字段缺失时仍累计请求次数，但只累计明确存在的 token 字段。
func (p *Pool) RecordTokenUsage(uid string, delta TokenUsageDelta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	usage := &e.tokenUsage
	usage.RequestCount++
	usage.LastUsedAt = time.Now()
	if delta.Model != "" {
		usage.LastModel = delta.Model
	}
	known := false
	if delta.HasPromptTokens && delta.PromptTokens >= 0 {
		usage.PromptTokens += delta.PromptTokens
		known = true
	}
	if delta.HasCompletionTokens && delta.CompletionTokens >= 0 {
		usage.CompletionTokens += delta.CompletionTokens
		known = true
	}
	if delta.HasTotalTokens && delta.TotalTokens >= 0 {
		usage.TotalTokens += delta.TotalTokens
		known = true
	}
	if known {
		usage.UsageCount++
	}
	if delta.HasLatencyMs && delta.LatencyMs >= 0 {
		usage.LastLatencyMs = delta.LatencyMs
	}
	if delta.HasTokensPerSecond && delta.TokensPerSecond >= 0 {
		speed := delta.TokensPerSecond
		usage.LastTokensPerSecond = &speed
	} else {
		// 失败或缺少 completion_tokens 时不展示上一次请求的旧吞吐速度。
		usage.LastTokensPerSecond = nil
	}
	p.dirty.Store(true)
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs 返回所有站点中当前 healthy 且未占满在途名额的 UID。
func (p *Pool) AvailableUIDs() []string {
	return p.AvailableUIDsForSiteModel("", "")
}

// AvailableUIDsForModel 保留旧的全池模型感知语义。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	return p.AvailableUIDsForSiteModel("", model)
}

// AvailableUIDsForSiteModel 返回指定站点、指定模型当前可用的 UID，供粘性路由
// 分配及命中校验。site 非空时绝不返回其他站点账号。
func (p *Pool) AvailableUIDsForSiteModel(site, model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if site != "" && (e.a == nil || auth.SiteFrom(e.a.Site) != site) {
			continue
		}
		if !e.healthyForModel(now, model) || p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUIDForModel 保留旧的全池模型感知直取语义。
func (p *Pool) PickByUIDForModel(uid, model string) *auth.Auth {
	return p.PickByUIDForSiteModel(uid, model, "")
}

// PickByUIDForSiteModel 对粘性 UID 做站点、模型、健康和在途的完整校验。
// site 非空而 UID 属于另一站点时返回 nil，禁止历史粘性绑定穿透路径站点。
func (p *Pool) PickByUIDForSiteModel(uid, model, site string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || (site != "" && (e.a == nil || auth.SiteFrom(e.a.Site) != site)) {
		return nil
	}
	now := time.Now()
	if !e.healthyForModel(now, model) || p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// PickByUID 若 uid 当前 healthy 且未占满在途名额，返回其凭证（记录 lastUsed 防撞号）；
// 否则返回 nil。供会话粘性路由命中校验与直取使用。
func (p *Pool) PickByUID(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// CountsDetailed 返回全池 total/healthy/cooling/disabled/inFlightFull 五类计数。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	return p.CountsDetailedForSite("")
}

// CountsDetailedForSite 返回指定站点的状态计数；空 site 表示全池。
func (p *Pool) CountsDetailedForSite(site string) (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if site != "" && (e.a == nil || auth.SiteFrom(e.a.Site) != site) {
			continue
		}
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow 报告全池当前是否可服务。
func (p *Pool) ServableNow() bool {
	return p.ServableNowForSite("")
}

// ServableNowForSite 报告指定站点是否存在真实可受理账号；空 site 保留全局语义。
func (p *Pool) ServableNowForSite(site string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if site != "" && (e.a == nil || auth.SiteFrom(e.a.Site) != site) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if e.healthy(now) || (e.modelExempt() && !now.Before(e.degradeUntil)) {
			return true
		}
	}
	return false
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}
func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	st := Status{
		UID:             uid,
		Nickname:        e.a.Nickname,
		Site:            e.a.Site,
		Credits:         e.credits,
		CreditsTotal:    e.creditsTotal,
		Cooling:         now.Before(e.until) || now.Before(e.breakerUntil) || now.Before(e.degradeUntil),
		Reason:          e.reason,
		Disabled:        e.disabled,
		SuccessCount:    e.successCount,
		ErrTotal:        e.errTotal,
		TokenUsage:      e.tokenUsage,
		LastSuccessTime: e.lastSuccess,
		LastErrTime:     e.lastErr,
		Until:           e.until,
		SoftStreak:      e.softStreak,
		InFlight:        int(e.inFlight.Load()),
		BreakerFails:    e.fails,
		BreakerUntil:    e.breakerUntil,
		DegradeFails:    e.consecutiveFails,
		DegradeUntil:    e.degradeUntil,
	}
	if st.Disabled {
		// 禁用账号透出禁用原因（运维看不到为什么死）。
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。
		// 保留既有冷却字段语义；仅连败降权作为唯一处罚时使用其截止。
		if now.Before(e.until) {
			st.CoolRemaining = int64(e.until.Sub(now).Seconds() + 0.999)
			st.CoolKind = e.coolKind.String()
		} else if now.Before(e.degradeUntil) && !now.Before(e.breakerUntil) {
			st.Until = e.degradeUntil
			st.CoolRemaining = int64(e.degradeUntil.Sub(now).Seconds() + 0.999)
			st.CoolKind = "degrade"
		}
	}
	return st
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------
