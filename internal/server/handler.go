// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏与默认上下文开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live             *livecfg.Holder
	Default1MContext bool

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:           h.cfg.APIKey,
		SoftCooldown:     h.cfg.SoftCooldown,
		Default1MContext: h.cfg.Default1MContext,
	}
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	waf     siteWAFGates
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	// 新版正式入口：站点前缀位于 /v1 之前，便于直接作为 OpenAI SDK base_url。
	h.mux.HandleFunc("POST /cn/v1/chat/completions", h.withAuth(h.chatForSite(auth.SiteCN)))
	h.mux.HandleFunc("GET /cn/v1/models", h.withAuth(h.modelsForSite(auth.SiteCN)))
	h.mux.HandleFunc("POST /intl/v1/chat/completions", h.withAuth(h.chatForSite(auth.SiteIntl)))
	h.mux.HandleFunc("GET /intl/v1/models", h.withAuth(h.modelsForSite(auth.SiteIntl)))
	h.mux.HandleFunc("GET /cn/healthz", h.healthzForSite(auth.SiteCN))
	h.mux.HandleFunc("GET /intl/healthz", h.healthzForSite(auth.SiteIntl))

	// 旧入口继续保留，避免已有客户端立即失效。
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatForSite(auth.SiteCN)))
	h.mux.HandleFunc("POST /v1/cn/chat/completions", h.withAuth(h.chatForSite(auth.SiteCN)))
	h.mux.HandleFunc("POST /v1/intl/chat/completions", h.withAuth(h.chatForSite(auth.SiteIntl)))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.modelsForSite(auth.SiteCN)))
	h.mux.HandleFunc("GET /v1/cn/models", h.withAuth(h.modelsForSite(auth.SiteCN)))
	h.mux.HandleFunc("GET /v1/intl/models", h.withAuth(h.modelsForSite(auth.SiteIntl)))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /healthz/cn", h.healthzForSite(auth.SiteCN))
	h.mux.HandleFunc("GET /healthz/intl", h.healthzForSite(auth.SiteIntl))
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	h.writeHealthz(w, "")
}

func (h *Handler) healthzForSite(site string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.writeHealthz(w, site) }
}

func (h *Handler) writeHealthz(w http.ResponseWriter, site string) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailedForSite(site)
	status := http.StatusOK
	if !h.cfg.Pool.ServableNowForSite(site) {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("X-Service", ServiceName)
	body := map[string]any{"healthy": healthy, "total": total, "service": ServiceName}
	if site != "" {
		body["site"] = site
	}
	writeJSON(w, status, body)
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// 静态模型表按站点独立持有，避免任何可变 map/slice 在 CN/Intl 间共享。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_input_tokens": 1000000, "max_allowed_size": 1000000, "max_output_tokens": 50000, "context_window": map[string]any{"supported_lengths": []int64{200000, 1000000}, "default_length": int64(200000)}, "contextWindow": map[string]any{"supportedLengths": []int64{200000, 1000000}, "defaultLength": int64(200000)}},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_input_tokens": 1000000, "max_allowed_size": 1000000, "max_output_tokens": 50000, "context_window": map[string]any{"supported_lengths": []int64{200000, 1000000}, "default_length": int64(200000)}, "contextWindow": map[string]any{"supportedLengths": []int64{200000, 1000000}, "defaultLength": int64(200000)}},
}

type siteModelsCache struct {
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// CN 字段保留原名以兼容包内测试；Intl 使用完全独立的缓存槽。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
	intl     siteModelsCache
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

func (h *Handler) modelsForSite(site string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"object": "list", "data": h.modelList(site)}
		if site == auth.SiteIntl {
			body["source"] = "catalog"
		}
		writeJSON(w, http.StatusOK, body)
	}
}

func (h *Handler) modelList(site string) []map[string]any {
	default1M := h.loadLive().Default1MContext
	// International discovery follows the official CLI: product.json carries
	// the catalog, and no enterprise console request is made.
	if site == auth.SiteIntl {
		infos := upstream.IntlBuiltinModels()
		if h.cfg.Upstream != nil {
			// FetchModels is intentionally local-only for Intl and also refreshes
			// the site's independent effort capability cache.
			infos, _ = h.cfg.Upstream.FetchModels(&auth.Auth{Site: auth.SiteIntl})
		}
		return modelInfoList(infos, site, default1M)
	}
	if infos := h.fetchDynamicModels(site); len(infos) > 0 {
		return modelInfoList(infos, site, default1M)
	}
	out := make([]map[string]any, 0, len(staticModels))
	for _, m := range staticModels {
		entry := make(map[string]any, len(m)+1)
		for k, v := range m {
			entry[k] = v
		}
		if snake, ok := m["context_window"].(map[string]any); ok {
			defaultLength := staticContextDefault(snake, m["max_input_tokens"], default1M)
			entry["context_window"] = map[string]any{"supported_lengths": snake["supported_lengths"], "default_length": defaultLength}
			if camel, ok := m["contextWindow"].(map[string]any); ok {
				entry["contextWindow"] = map[string]any{"supportedLengths": camel["supportedLengths"], "defaultLength": defaultLength}
			}
		}
		entry["site"] = site
		out = append(out, entry)
	}
	return out
}

func staticContextDefault(contextWindow map[string]any, maxInput any, default1M bool) int64 {
	lengths, _ := contextWindow["supported_lengths"].([]int64)
	canonical, _ := contextWindow["default_length"].(int64)
	physical, _ := maxInput.(int64)
	return upstream.EffectiveDefaultContextLength(lengths, canonical, physical, default1M)
}

func modelInfoList(infos []upstream.ModelInfo, site string, default1M bool) []map[string]any {
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		contextLength := mi.ContextWindow
		if contextLength == 0 {
			contextLength = 131072
		}
		entry := map[string]any{"id": mi.ID, "object": "model", "created": 1753600000,
			"owned_by": "workbuddy", "site": site, "context_length": contextLength,
			"max_input_tokens": contextLength, "max_output_tokens": mi.MaxTokens,
			"max_allowed_size": mi.MaxAllowedSize}
		if len(mi.ContextLengths) > 0 {
			defaultLength := upstream.EffectiveDefaultContextLength(mi.ContextLengths, mi.DefaultContextLength, contextLength, default1M)
			entry["context_window"] = map[string]any{"supported_lengths": mi.ContextLengths, "default_length": defaultLength}
			entry["contextWindow"] = map[string]any{"supportedLengths": mi.ContextLengths, "defaultLength": defaultLength}
		}
		if len(mi.Efforts) > 0 {
			entry["supported_efforts"] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			entry["default_effort"] = mi.DefaultEffort
		}
		if mi.SupportsReasoning {
			entry["supports_reasoning"] = true
			entry["can_disable_thinking"] = mi.CanDisableThinking
		}
		if mi.Credits != "" {
			entry["credits"] = mi.Credits
		}
		out = append(out, entry)
	}
	return out
}

// fetchDynamicModels 只用同站账号拉取，并只读写该站缓存；无同站账号直接静态回退。
func (h *Handler) fetchDynamicModels(site string) []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	c := siteModelsCache{ids: dynamicModelsCache.ids, fetched: dynamicModelsCache.fetched, lastFail: dynamicModelsCache.lastFail}
	if site == auth.SiteIntl {
		c = dynamicModelsCache.intl
	}
	if len(c.ids) > 0 && time.Since(c.fetched) < dynamicModelsTTL {
		out := append([]upstream.ModelInfo(nil), c.ids...)
		dynamicModelsCache.RUnlock()
		return out
	}
	if !c.lastFail.IsZero() && time.Since(c.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()
	acct := h.cfg.Pool.PickForSite(site)
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		if site == auth.SiteIntl {
			dynamicModelsCache.intl.lastFail = time.Now()
		} else {
			dynamicModelsCache.lastFail = time.Now()
		}
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	if site == auth.SiteIntl {
		dynamicModelsCache.intl = siteModelsCache{ids: infos, fetched: time.Now()}
	} else {
		dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = infos, time.Now(), time.Time{}
	}
	dynamicModelsCache.Unlock()
	return infos
}

func (h *Handler) chatForSite(site string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.chatCompletions(w, r, site) }
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request, site string) {

	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 会话键按站点命名空间化，解析与重分配都使用站点感知候选集。
	sessKey := ""
	// rawSessKey 供上游 prompt_cache_key 注入（不受会话粘性开关影响，纯省费优化）；
	// sessKey 是加站点前缀后的粘性路由键。
	rawSessKey := session.ExtractKey(body)
	stickyUID := ""
	if h.cfg.Session != nil {
		if rawSessKey != "" {
			sessKey = site + ":" + rawSessKey
			if uid, ok := h.cfg.Session.ResolveForSiteModel(sessKey, site, peek.Model); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, started time.Time) {
		delta.Model = peek.Model
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs
		if delta.HasCompletionTokens && delta.CompletionTokens >= 0 && latencyMs > 0 {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = float64(delta.CompletionTokens) * 1000 / float64(latencyMs)
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough 非降级期：透传客户端原始 system（不改写）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "passthrough" && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}
	// 站点级 WAF gate 激活期间，同站新请求直接 fail-fast，避免继续消耗账号池；
	// CN/INTL gate 独立，另一通道不受影响。
	if h.waf.forSite(site).active(time.Now()) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "waf_ip_blocked",
			"上游正在阻止该站点的网关出口 IP，请稍后重试")
		st.status = http.StatusServiceUnavailable
		return
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUID 已校验 health + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForSiteModel(stickyUID, peek.Model, site)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满/被当前模型限额）→ 解绑，本次回落普通轮换。
				unbindSticky()
			}
		}
		if acct == nil {
			// 首次普通选号与每次重试都携带路由入口固定的 site。
			acct = h.cfg.Pool.PickForSiteModel(tried, peek.Model, site)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		// 会话标识随请求传入，供上游注入 prompt_cache_key（前缀缓存复用，省费）；
		// rawSessKey 为 session.ExtractKey 的原始返回值（未加站点前缀），空则不复用。
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContextWithConversation(r.Context(), acct, body, clientIP, rawSessKey)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
			lastErr = terr
			fail(acct.UID)
			h.cfg.Pool.NoteFailures(acct.UID)
			if !sleepCtx(r.Context(), backoffAfter(i)) {
				if r.Context().Err() == nil {
					writeOpenAIError(w, http.StatusServiceUnavailable, "retry_cancelled", "上游重试已取消")
					st.status = http.StatusServiceUnavailable
				}
				return
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			// 内容拦截误报（passthrough 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试。
			// 第二次仍被拦（用户内容本身触发审核）→ 走既有错误路径返回客户端。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), peek.Model)
			fail(acct.UID)
			if kind == upstream.ErrClient || kind == upstream.ErrNone {
				h.cfg.Pool.NoteFailures(acct.UID)
			}
			// WAF 403 在同一站点短窗内命中多个不同账号，说明出口 IP 被拦；停止换号，
			// 避免把一个请求放大为 MaxRotate 次。CN/INTL 使用独立 gate。
			if kind == upstream.ErrWafBlock && h.waf.forSite(site).note(acct.UID, time.Now()) {
				log.Printf("WARN: waf ip-level block site=%s, rotation stopped", site)
				break
			}
			if !sleepCtx(r.Context(), backoffAfter(i)) {
				if r.Context().Err() == nil {
					writeOpenAIError(w, http.StatusServiceUnavailable, "retry_cancelled", "上游重试已取消")
					st.status = http.StatusServiceUnavailable
				}
				return
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = upstream.Stream(w, stats)
			recordAttempt(acct.UID, stats.Usage(), attemptStarted)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), attemptStarted)
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 七条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 默认 Cooldown(CoolSoft, soft_rate) 连续触发指数退避（封顶 soft_rate_max）；
//     若上游 body 为模型级 6004 且带重置时间 → CooldownSoftForModel（until=重置墙钟，
//     封顶 soft_rate_max，记录触发模型供切模型豁免）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError），passthrough 模式走降级重试。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 模型级 6004 且带「将在 … 重置」时间（issue #31）：冷却到上游明说的重置墙钟
		// （封顶 soft_rate_max），记录触发模型 → 该账号对**其他模型**请求可豁免冷却。
		// 解析失败（无时间文案 / 非 6004）→ 退回既有 600s 基数 + 指数退避现况。
		// 基数一律取 h.softCooldown()（热改优先），管理面板改 soft_rate 后立即生效。
		if upstream.IsModelRateLimit(body) {
			if resetAt, ok := upstream.ParseSoftRateReset(body); ok {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
		}
		// 其余 soft_rate：软冷却基数来自 soft_rate（默认 600s）；同一账号连续触发时
		// pool 内部按 softStreak 指数退避并封顶 soft_rate_max。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障喂熔断计数。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrWafBlock:
		// WAF 多为出口 IP/指纹维频控，不永久禁号；账号短冷却，站点 gate 决定是否停轮转。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, 60*time.Second, "waf 403 block")
	case upstream.ErrContentBlocked:
		// 内容策略拦截：内容问题非账号问题，不罚账号；passthrough 模式走降级重试。
	case upstream.ErrBadParams:
		// 请求体解析失败：不罚账号，仍允许不同账号的模型权限轮转。
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
