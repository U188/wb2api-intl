// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 站点标识：决定出站 base URL / Origin/Referer / 可用的任务体系。
// 国内版（workbuddy.cn / codebuddy.cn / copilot.tencent.com）有成长任务+签到积分体系；
// 国际版（codebuddy.ai）协议同构但无 gamification，只跑 chat/余额/token 保活。
const (
	SiteCN   = "cn"
	SiteIntl = "intl"
)

// SiteFrom 归一化站点串。空/未知值回落 SiteCN 以兼容旧凭证；常见国际别名
// 大小写不敏感，避免合法国际凭证因拼写形态落入国内路由。
func SiteFrom(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SiteIntl, "international", "global":
		return SiteIntl
	default:
		return SiteCN
	}
}

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
	Site         string // 站点（auth.SiteCN 默认 / auth.SiteIntl）；决定出站域与任务体系
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	DeviceToken string
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	// 顶层 site 键（嵌套形/扁平形共用）；显式值优先。旧国际凭证若缺 site，
	// 在完成字段解析后可由 codebuddy.ai domain 安全识别，避免误入国内池。
	parsedSite := SiteCN
	hasExplicitSite := false
	if rawSite, ok := probe["site"]; ok {
		var s string
		if err := json.Unmarshal(rawSite, &s); err == nil {
			parsedSite = SiteFrom(s)
			hasExplicitSite = strings.TrimSpace(s) != ""
		}
	}
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
			Site:         parsedSite,
		}
		a.Site = SiteFrom(a.Site)
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
			Site:         parsedSite,
		}
		a.Site = SiteFrom(a.Site)
	}
	if !hasExplicitSite && strings.Contains(strings.ToLower(a.Domain), "codebuddy.ai") {
		a.Site = SiteIntl
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"site": a.Site,
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", tmp, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
