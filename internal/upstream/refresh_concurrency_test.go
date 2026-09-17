package upstream

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// blockingRoundTripper 允许测试在网络请求执行期间检查 Auth 锁是否仍可获取。
type blockingRoundTripper struct {
	started chan struct{}
	release chan struct{}
	body    string
	once    sync.Once
}

func (b *blockingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(b.body)),
	}, nil
}

func TestRefreshTokenDoesNotHoldAuthLockDuringNetwork(t *testing.T) {
	block := &blockingRoundTripper{
		started: make(chan struct{}), release: make(chan struct{}),
		body: `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`,
	}
	c := New()
	c.HTTP = &http.Client{Transport: block, Timeout: 5 * time.Second}
	a := &auth.Auth{AccessToken: "old-at", RefreshToken: "old-rt", UID: "u", Site: auth.SiteCN}
	done := make(chan error, 1)
	go func() { done <- c.RefreshToken(a) }()
	<-block.started

	// 网络仍阻塞时 Snapshot 必须立即可读；若 RefreshToken 持锁，此处会超时。
	snapDone := make(chan auth.Snapshot, 1)
	go func() { snapDone <- a.Snapshot() }()
	select {
	case s := <-snapDone:
		if s.AccessToken != "old-at" {
			t.Fatalf("snapshot during refresh=%+v", s)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("auth lock held during refresh network request")
	}
	close(block.release)
	if err := <-done; err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if got := a.Snapshot(); got.AccessToken != "new-at" || got.RefreshToken != "new-rt" {
		t.Fatalf("refresh not committed: %+v", got)
	}
}

func TestRefreshTokenStaleResponseDoesNotOverwriteNewerCredentials(t *testing.T) {
	block := &blockingRoundTripper{
		started: make(chan struct{}), release: make(chan struct{}),
		body: `{"code":0,"data":{"accessToken":"stale-at","refreshToken":"stale-rt","expiresIn":3600}}`,
	}
	c := New()
	c.HTTP = &http.Client{Transport: block, Timeout: 5 * time.Second}
	a := &auth.Auth{AccessToken: "old-at", RefreshToken: "old-rt", UID: "u", Site: auth.SiteCN}
	done := make(chan error, 1)
	go func() { done <- c.RefreshToken(a) }()
	<-block.started
	// 模拟另一轮/热加载先完成；即使 token 值碰巧相同，generation/site/file 变化也
	// 必须使旧响应失效（ABA 防护）。
	a.Lock()
	a.AccessToken, a.RefreshToken = "old-at", "old-rt"
	a.Generation++
	a.Site = auth.SiteIntl
	a.FilePath = "replacement.json"
	a.Unlock()
	close(block.release)
	if err := <-done; err != nil {
		t.Fatalf("stale refresh returned error: %v", err)
	}
	if got := a.Snapshot(); got.AccessToken != "old-at" || got.RefreshToken != "old-rt" ||
		got.Site != auth.SiteIntl || got.FilePath != "replacement.json" || got.Generation != 1 {
		t.Fatalf("stale response overwrote newer credentials: %+v", got)
	}
}
