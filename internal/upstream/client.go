// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

var errCNGamificationOnly = errors.New("gamification is only available for cn accounts")

func requireCNGamification(a *auth.Auth) error {
	if a == nil || a.Snapshot().Site != auth.SiteCN {
		return errCNGamificationOnly
	}
	return nil
}

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	ErrWafBlock                      // 403 + 非业务信封体（空体/HTML）→ 账号软冷却；多号触发站点级 fail-fast
	ErrClient                        // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrWafBlock:
		return "waf_block"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// badParamsMarkers 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsMarkerMsg = "Unmarshal chat params failed"

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinMarkers = []string{"已签到", "already"}
var badParamsMarkerCode = `"code":11101`

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetRe 匹配「将在 … 重置」，捕获中间的时间串。
const softRateResetRe = `将在 (.+?) 重置`

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	re := regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	return re.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
// 内部先判 IsModelRateLimit：非模型级限流（非 6004）即使带"重置"字样也不返回——该重置
// 无冷却语义（如 11140 的通用限流提示），解析出来反而会错误收窄冷却。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	re := regexp.MustCompile(softRateResetRe)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func IsWafBlocked(status int, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	var env map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &env) == nil {
		if _, ok := env["code"]; ok {
			return false
		}
		if _, ok := env["msg"]; ok {
			return false
		}
	}
	return true
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  1. 402 / hardMarkers —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit，本次保持不变
//     （issue #28 已记录该反向误判风险，待上游原始响应确认后再定）。
//  2. sessionDeadMarkers —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时在此短路，
//     结果同为 soft_rate，与下一层一致。
//  4. status==429 —— body 无文案时的兜底识别。
//  5. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if IsWafBlocked(status, body) {
		return ErrWafBlock
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
		// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
		// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
		// 归 ErrBadParams：不冷却/不熔断/不计错，但**仍然轮转**（不同账号可能有不同的
		// 模型权限，值得再试一次）。
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// efforts 按站点隔离缓存各模型 supportedEfforts，防止同名模型能力跨站污染。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool
	sanitizeConfigured   atomic.Bool
	sanitizeFingerprints atomic.Bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
	// 空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空）。
	UserAgent string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + X-IDE-Version）。
	// 空 = 内置默认（对齐官方 5.5.4 分发包）。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认（官方内置 CLI 2.137.1）。
	CliVersion string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空 = 旧行为：X-Product="SaaS"，不设 X-IDE-*（向后兼容，不突变归因）。
	ClientName string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 传入 ChatStream。
	PassthroughIP bool

	// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底来源：config upstream.device_token。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceToken string

	// DeviceTokenFile 设备 token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
	DeviceTokenFile string

	ChatBaseCN    string
	BillingBaseCN string
	// WebBaseCN 官网（workbuddy.cn）域：部分「任务领奖」类接口只在此域提供
	// （Web 成长中心用；CLI 域 copilot.tencent.com 的同名路径返回 400）。
	WebBaseCN string
	// ChatBaseIntl / BillingBaseIntl 国际版（codebuddy.ai）基础域。
	// 国际版协议与国内版同构（chat/models/billing/refresh 路径一致），仅 base URL 与
	// Origin/Referer 不同；按账号 auth.Site 分发。国际版无成长任务/签到体系，
	// 任务类接口不会走到（调度与面板已按 site 屏蔽）。
	ChatBaseIntl    string
	BillingBaseIntl string
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	c := &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr},
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com", BillingBaseCN: "https://www.codebuddy.cn",
		WebBaseCN: "https://www.workbuddy.cn", ChatBaseIntl: "https://www.codebuddy.ai",
		BillingBaseIntl: "https://www.codebuddy.ai",
	}
	c.SetSanitizeFingerprints(true)
	return c
}

// SetSanitizeFingerprints 可并发热改出站指纹脱敏开关。
func (c *Client) SetSanitizeFingerprints(v bool) {
	c.sanitizeFingerprints.Store(v)
	c.sanitizeConfigured.Store(true)
}

func (c *Client) sanitizeEnabled() bool {
	if c.sanitizeConfigured.Load() {
		return c.sanitizeFingerprints.Load()
	}
	// 手工构造的测试/旧调用仍以公开字段为准。
	return c.SanitizeFingerprints
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

func accountSite(a *auth.Auth) string {
	if a == nil {
		return auth.SiteCN
	}
	return a.Snapshot().Site
}

func (c *Client) chatBase(a *auth.Auth) string { return c.chatBaseSnapshot(a.Snapshot()) }

func (c *Client) chatBaseSnapshot(s auth.Snapshot) string {
	if s.Site == auth.SiteIntl && c.ChatBaseIntl != "" {
		return c.ChatBaseIntl
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体；effort 能力严格按实际账号站点读取。
func (c *Client) prepareBody(a *auth.Auth, body []byte) []byte {
	return c.prepareBodySnapshot(a.Snapshot(), body)
}

func (c *Client) prepareBodySnapshot(s auth.Snapshot, body []byte) []byte {
	return PrepareBodyOptWithEfforts(body, c.sanitizeEnabled(), c.effortsSnapshot(s.Site))
}

// effortsSnapshot 返回指定站点 effort 能力缓存副本。国际目录是随程序内置的，
// 即使进程启动后尚未访问 models 接口，聊天也必须立刻按官方能力校正档位。
func (c *Client) effortsSnapshot(site string) map[string][]string {
	normalizedSite := auth.SiteFrom(site)
	c.effortsMu.RLock()
	siteCache := c.efforts[normalizedSite]
	if len(siteCache) > 0 {
		cp := make(map[string][]string, len(siteCache))
		for k, v := range siteCache {
			cp[k] = append([]string(nil), v...)
		}
		c.effortsMu.RUnlock()
		return cp
	}
	c.effortsMu.RUnlock()
	if normalizedSite == auth.SiteIntl {
		cp := make(map[string][]string)
		for _, model := range IntlBuiltinModels() {
			if len(model.Efforts) > 0 {
				cp[model.ID] = append([]string(nil), model.Efforts...)
			}
		}
		return cp
	}
	return nil
}

func (c *Client) billingBase(a *auth.Auth) string {
	if accountSite(a) == auth.SiteIntl && c.BillingBaseIntl != "" {
		return c.BillingBaseIntl
	}
	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
// 按账号站点分发：国际版无成长任务，实际不会走到；此处统一回落各站默认。
func (c *Client) webBase(a *auth.Auth) string { return c.webBaseSnapshot(a.Snapshot()) }

func (c *Client) webBaseSnapshot(s auth.Snapshot) string {
	if s.Site == auth.SiteIntl {
		if c.BillingBaseIntl != "" {
			return c.BillingBaseIntl
		}
		return "https://www.codebuddy.ai"
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath = "/v2/billing/meter/get-user-resource"
	dailyCheckinPath = "/v2/billing/meter/daily-checkin"
)

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken serializes refreshes per Auth, not snapshots or other requests.
// Only a generation change for the same identity is reusable as a successful refresh.
func (c *Client) RefreshToken(a *auth.Auth) error {
	if a == nil {
		return fmt.Errorf("nil auth")
	}
	before := a.Snapshot()
	a.LockRefresh()
	defer a.UnlockRefresh()
	s := a.Snapshot()
	if !sameRefreshIdentity(before, s) {
		return fmt.Errorf("refresh skipped: account identity changed")
	}
	if s.Generation != before.Generation {
		return nil
	}
	if !sameRefreshCredentials(before, s) {
		return fmt.Errorf("refresh skipped: credentials changed")
	}
	if strings.TrimSpace(s.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	urlBase := c.ChatBaseCN
	if s.Site == auth.SiteIntl && c.ChatBaseIntl != "" {
		urlBase = c.ChatBaseIntl
	}
	req, err := http.NewRequest(http.MethodPost, urlBase+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return err
	}
	c.refreshHeadersSnapshot(req, s)
	data, err := c.doJSON(req)
	if err != nil {
		return refreshResult(a.Snapshot(), s, err)
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if json.Unmarshal(data, &tok) != nil || tok.AccessToken == "" {
		return refreshResult(a.Snapshot(), s, fmt.Errorf("refresh_failed: no accessToken in response — re-login required"))
	}
	a.Lock()
	defer a.Unlock()
	current := a.SnapshotLocked()
	if !sameRefreshIdentity(s, current) {
		return fmt.Errorf("refresh skipped: account identity changed")
	}
	if current.Generation != s.Generation {
		return nil
	}
	if !sameRefreshCredentials(s, current) {
		return fmt.Errorf("refresh skipped: credentials changed")
	}
	a.AccessToken = tok.AccessToken
	a.Generation++
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

func sameRefreshIdentity(x, y auth.Snapshot) bool {
	return x.Site == y.Site && x.UID == y.UID && x.FilePath == y.FilePath
}

func sameRefreshCredentials(x, y auth.Snapshot) bool {
	return x.AccessToken == y.AccessToken && x.RefreshToken == y.RefreshToken &&
		x.Domain == y.Domain && x.ExpiresAt == y.ExpiresAt
}

func refreshResult(current, original auth.Snapshot, err error) error {
	if !sameRefreshIdentity(current, original) {
		return fmt.Errorf("refresh skipped: account identity changed")
	}
	if current.Generation != original.Generation {
		return nil
	}
	if !sameRefreshCredentials(current, original) {
		return fmt.Errorf("refresh skipped: credentials changed")
	}
	return err
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string) (io.ReadCloser, int, []byte, error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP)
}

// ChatStreamContext binds the upstream stream to the caller's lifetime.
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string) (rc io.ReadCloser, status int, respBody []byte, err error) {
	s := a.Snapshot()
	urlBase := c.ChatBaseCN
	if auth.SiteFrom(s.Site) == auth.SiteIntl && c.ChatBaseIntl != "" {
		urlBase = c.ChatBaseIntl
	}
	url := urlBase + "/v2/chat/completions"
	out := c.prepareBodySnapshot(s, body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	c.chatHeadersSnapshot(req, s, clientIP)
	streamCtx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(streamCtx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("chat_stream uid=%s: transport error", s.UID)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s", s.UID, resp.StatusCode, kind)
		return nil, resp.StatusCode, raw, nil
	}
	// Closing the returned body always releases the derived request context.
	return &cancelOnClose{ReadCloser: monitorBody(resp.Body, c.IdleTimeout, cancel), cancel: cancel}, resp.StatusCode, nil, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

// ModelInfo 动态模型信息（含物理 token 上限与客户端会话上下文档位）。
type ModelInfo struct {
	ID                   string
	Name                 string
	ContextWindow        int64    // = maxInputTokens（模型物理输入上限）
	MaxTokens            int64    // = maxOutputTokens（思考与最终回答共享此预算，上游无独立思考上限字段）
	MaxAllowedSize       int64    // = maxAllowedSize（单请求体大小上限，通常等于 maxInputTokens）
	ContextLengths       []int64  // contextWindow.supportedLengths（客户端会话预算/压缩阈值）
	DefaultContextLength int64    // contextWindow.defaultLength
	Efforts              []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort        string   // reasoning.defaultEffort（新模型键）或 reasoning.effort（老模型键）；空=未返回
	CanDisableThinking   bool     // reasoning.canDisableThinking：思考可关（off 档可用）
	SupportsReasoning    bool     // supportsReasoning：模型支持思考
	Credits              string   // credits：积分倍率（如 "x0.79"）
}

// EffectiveDefaultContextLength 在输出模型元数据时按运行期开关选择默认会话预算。
// ModelInfo 自身保持 canonical，且这里只返回元数据值，不参与聊天请求体处理。
func EffectiveDefaultContextLength(supported []int64, canonical, maxInput int64, default1M bool) int64 {
	target := int64(200_000)
	if default1M && maxInput >= 1_000_000 {
		target = 1_000_000
	}
	for _, n := range supported {
		if n == target {
			return target
		}
	}
	return canonical
}

var deepSeekV4ContextModel = regexp.MustCompile(`(?i)^deepseek-v4(?:[.]1)?-(?:flash|pro)(?:-|$)`)

func parseContextWindow(lengths []json.RawMessage, defaultRaw json.RawMessage) ([]int64, int64) {
	parsed := make([]int64, 0, len(lengths))
	for _, raw := range lengths {
		if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
			parsed = append(parsed, n)
		}
	}
	defaultLength, _ := strconv.ParseInt(string(defaultRaw), 10, 64)
	return parsed, defaultLength
}

func normalizeContextWindow(id string, maxInput, maxOutput, maxAllowed int64, lengths []int64, defaultLength int64) (int64, int64, int64, []int64, int64) {
	isDeepSeekV4 := deepSeekV4ContextModel.MatchString(id)
	if isDeepSeekV4 {
		// 官方 CN 目录给 V4/V4.1 标注 1M/50K；仅在动态接口缺失时补齐。
		// 若某个租户/私有变体明确返回更小上限，必须尊重该部署，禁止虚报 1M。
		if maxInput <= 0 {
			maxInput = 1_000_000
		}
		if maxAllowed <= 0 {
			maxAllowed = maxInput
		}
		// 50K 是 CN 侧腾讯官方 product.json 对 deepseek-v4-flash 的声明值，
		// 只在本函数（CN 动态目录）兜底。Intl 目录不经过此处，其 V4.1 Flash
		// 输出上限见 intl_catalog.go（384K，DeepSeek 官方/models.dev 共识）。
		if maxOutput <= 0 {
			maxOutput = 50_000
		}
	}
	seen := make(map[int64]struct{}, len(lengths))
	clean := make([]int64, 0, len(lengths))
	for _, n := range lengths {
		if n <= 0 || maxInput <= 0 || n > maxInput {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		clean = append(clean, n)
	}
	// 仅保留物理输入上限以内的正整数档位；default 也必须指向有效档位。
	sort.Slice(clean, func(i, j int) bool { return clean[i] < clean[j] })
	if defaultLength > 0 {
		if _, ok := seen[defaultLength]; !ok {
			defaultLength = 0
		}
	}
	if len(clean) == 0 && isDeepSeekV4 && maxInput >= 1_000_000 {
		clean = []int64{200_000, 1_000_000}
	}
	if defaultLength <= 0 || defaultLength > maxInput {
		defaultLength = 0
	}
	// canonical 目录默认使用 200K；运行时开关只在输出模型元数据时切换为 1M。
	for _, n := range clean {
		if n == 200_000 {
			defaultLength = 200_000
			break
		}
	}
	if defaultLength == 0 && len(clean) > 0 {
		defaultLength = clean[0]
	}
	return maxInput, maxOutput, maxAllowed, clean, defaultLength
}

// 字段名与上游实际返回对齐：物理上限来自 maxInputTokens/maxOutputTokens，
// contextWindow 仅描述客户端会话预算档位，不用于改写请求。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	if a == nil {
		return nil, fmt.Errorf("fetch models: account is nil")
	}
	s := a.Snapshot()
	// The international CLI catalog is bundled in product.json. The CN
	// enterprise models endpoint returns 500 on codebuddy.ai and must never be
	// used for international discovery.
	if s.Site == auth.SiteIntl {
		out := IntlBuiltinModels()
		c.refreshEfforts(auth.SiteIntl, out)
		return out, nil
	}
	urlBase := c.ChatBaseCN
	if s.Site == auth.SiteIntl && c.ChatBaseIntl != "" {
		urlBase = c.ChatBaseIntl
	}
	url := urlBase + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.commonHeadersSnapshot(req, s)
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID                string `json:"id"`
				Name              string `json:"name"`
				MaxInputTokens    int64  `json:"maxInputTokens"`
				MaxOutputTokens   int64  `json:"maxOutputTokens"`
				MaxAllowedSize    int64  `json:"maxAllowedSize"`
				Disabled          bool   `json:"disabled"`
				Credits           string `json:"credits"`
				SupportsReasoning bool   `json:"supportsReasoning"`
				ContextWindow     struct {
					SupportedLengths []json.RawMessage `json:"supportedLengths"`
					DefaultLength    json.RawMessage   `json:"defaultLength"`
				} `json:"contextWindow"`
				Reasoning struct {
					Effort             string   `json:"effort"`        // 老模型键（auto/hy3/glm-5.2 系）
					DefaultEffort      string   `json:"defaultEffort"` // 新模型键（glm-5.3 系只返回这个）
					CanDisableThinking bool     `json:"canDisableThinking"`
					SupportedEfforts   []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	type parsed struct {
		mi       ModelInfo
		disabled bool
	}
	dynMap := make(map[string]parsed, len(env.Data.Models))
	for _, m := range env.Data.Models {
		def := m.Reasoning.Effort
		if def == "" {
			def = m.Reasoning.DefaultEffort // 新旧双键兼容：glm-5.3 系只返回 defaultEffort
		}
		contextLengths, defaultContextLength := parseContextWindow(m.ContextWindow.SupportedLengths, m.ContextWindow.DefaultLength)
		maxInput, maxOutput, maxAllowed, contextLengths, defaultContextLength := normalizeContextWindow(
			m.ID, m.MaxInputTokens, m.MaxOutputTokens, m.MaxAllowedSize, contextLengths, defaultContextLength)
		dynMap[m.ID] = parsed{ModelInfo{
			ID:                   m.ID,
			Name:                 m.Name,
			ContextWindow:        maxInput,
			MaxTokens:            maxOutput,
			MaxAllowedSize:       maxAllowed,
			ContextLengths:       contextLengths,
			DefaultContextLength: defaultContextLength,
			Efforts:              m.Reasoning.SupportedEfforts,
			DefaultEffort:        def,
			CanDisableThinking:   m.Reasoning.CanDisableThinking,
			SupportsReasoning:    m.SupportsReasoning,
			Credits:              m.Credits,
		}, m.Disabled}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		if pm, ok := dynMap[id]; ok && !pm.disabled {
			out = append(out, pm.mi)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 CN effort 能力缓存；Intl 始终由独立内置目录刷新。
	c.refreshEfforts(auth.SiteCN, out)
	return out, nil
}

// UserResource 查询账号积分余额与总额度（所有套餐聚合）。remain 负值钳 0；
// total 取与 remain 同源的额度字段（CycleCapacitySize 优先，无周期额度退
// CapacitySize），上游缺 size 的套餐按 remain 兜底，保证百分比不超 100%。
func (c *Client) UserResource(a *auth.Auth) (remain, total int64, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingJSON(a, http.MethodPost, billingMeterPath, body)
	if err != nil {
		return 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r, size int64
		switch {
		case acct.CycleCapacitySize > 0:
			r, size = acct.CycleCapacityRemain, acct.CycleCapacitySize
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r, size = acct.CycleCapacityRemain, acct.CycleCapacitySize
		default:
			r, size = acct.CapacityRemain, acct.CapacitySize
		}
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
	}
	return remain, total, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	if err := requireCNGamification(a); err != nil {
		return err
	}
	_, err := c.billingJSON(a, http.MethodPost, dailyCheckinPath, map[string]any{})
	return err
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
