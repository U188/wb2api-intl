// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>` 与
	// 白名单头组的 X-IDE-Version）。对齐官方 WorkBuddy Desktop 分发包版本（5.5.4）。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	defaultClientVersion = "5.5.4"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐官方内置 CLI（2.137.1）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.137.1"

	originRefererCN   = "https://www.codebuddy.cn"
	originRefererIntl = "https://www.codebuddy.ai"
)

// originRefererFor 返回该账号所属站点的 Origin/Referer 根：
// 国内版 web 域 codebuddy.cn；国际版 codebuddy.ai（与出站 base URL 同域）。
// a 为 nil / 未知 site 时回落国内版（向后兼容）。
// 注意：仅供兼容旧调用；请求主路径优先使用同一 Snapshot 的 commonHeadersSnapshot。
func originRefererFor(a *auth.Auth) string {
	if a != nil && auth.SiteFrom(a.Snapshot().Site) == auth.SiteIntl {
		return originRefererIntl
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则内置默认 defaultClientVersion。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUA 组装默认客户端出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<clientVersion> WorkBuddy/<clientVersion> CLI/<cliVersion>`。
func (c *Client) defaultWorkBuddyUA() string {
	return "WorkBuddy/" + c.clientVersion() + " WorkBuddy/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// userAgent 返回当前出站 UA（客户端出站路径）：
// Client.UserAgent（config user_agent）显式覆盖 > 默认 WorkBuddy 三段式。
func (c *Client) userAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUA()
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。仅当 client_name 配置（非空）才生效。
func (c *Client) billingUA() string {
	if c == nil || c.ClientName == "" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil {
		return c.resolveDeviceTokenSnapshot(a.Snapshot())
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// resolveDeviceTokenSnapshot 解析本次请求的设备 token，账号快照优先于全局配置与文件。
func (c *Client) resolveDeviceTokenSnapshot(s auth.Snapshot) string {
	if s.DeviceToken != "" {
		return s.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

func (c *Client) injectDeviceTokenSnapshot(req *http.Request, s auth.Snapshot) {
	if tok := c.resolveDeviceTokenSnapshot(s); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// CommonHeaders 设置所有 API 共享的请求头。每次调用只取一次 Auth Snapshot，保证
// token refresh 并发时各字段来自同一版本。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	c.commonHeadersSnapshot(req, a.Snapshot())
}

func (c *Client) commonHeadersSnapshot(req *http.Request, s auth.Snapshot) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererCN
	if auth.SiteFrom(s.Site) == auth.SiteIntl {
		origin = originRefererIntl
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent())
}

func (c *Client) chatHeadersSnapshot(req *http.Request, s auth.Snapshot, clientIP string) {
	c.commonHeadersSnapshot(req, s)
	if s.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if s.Domain != "" {
		req.Header.Set("X-Domain", s.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	c.injectAttribution(req)
	c.injectClientIP(req, clientIP)
	c.injectDeviceTokenSnapshot(req, s)
}

// ChatHeaders 在 common 之上加 chat 专属账号头。凭证字段来自一次 Snapshot，
// 防止 refresh 并发更新时拼出新 token + 旧 domain 的混合请求。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string) {
	c.chatHeadersSnapshot(req, a.Snapshot(), clientIP)
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// ClientName 非空时全量跟随该值，空则只保留 X-Product="SaaS"（旧行为，向后兼容）。
func (c *Client) injectAttribution(req *http.Request) {
	if c == nil || c.ClientName == "" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", c.ClientName)
	req.Header.Set("X-IDE-Type", c.ClientName)
	req.Header.Set("X-IDE-Version", c.clientVersion())
	req.Header.Set("X-Product", c.ClientName)
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头；所有账号字段来自同一 Snapshot。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	c.billingHeadersSnapshot(req, a.Snapshot())
}

func (c *Client) billingHeadersSnapshot(req *http.Request, s auth.Snapshot) {
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
		req.Header.Set("X-Tenant-Id", s.EnterpriseID)
	}
	if s.Domain != "" {
		req.Header.Set("X-Domain", s.Domain)
	}
	c.injectDeviceTokenSnapshot(req, s)
}

// RefreshHeaders builds the refresh headers from one immutable snapshot.
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.refreshHeadersSnapshot(req, a.Snapshot())
}

func (c *Client) refreshHeadersSnapshot(req *http.Request, s auth.Snapshot) {
	c.commonHeadersSnapshot(req, s)
	req.Header.Set("X-Refresh-Token", s.RefreshToken)
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}
