package upstream

import (
	"context"
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
			t.Fatal("unexpected snapshot during refresh")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("auth lock held during refresh network request")
	}
	close(block.release)
	if err := <-done; err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if got := a.Snapshot(); got.AccessToken != "new-at" || got.RefreshToken != "new-rt" {
		t.Fatal("refresh not committed")
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
	if err := <-done; err == nil {
		t.Fatal("identity replacement must not be reported as refresh success")
	}
	if got := a.Snapshot(); got.AccessToken != "old-at" || got.RefreshToken != "old-rt" ||
		got.Site != auth.SiteIntl || got.FilePath != "replacement.json" || got.Generation != 1 {
		t.Fatal("stale response overwrote newer credentials")
	}
}

func TestRefreshTokenSerializesRotatingToken(t *testing.T) {
	block := &blockingRoundTripper{started: make(chan struct{}), release: make(chan struct{}),
		body: `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt"}}`}
	c := New()
	var calls int
	var mu sync.Mutex
	c.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return block.RoundTrip(r)
	})}
	a := &auth.Auth{AccessToken: "old-at", RefreshToken: "old-rt", Site: auth.SiteCN}
	done := make(chan error, 2)
	go func() { done <- c.RefreshToken(a) }()
	<-block.started
	go func() { done <- c.RefreshToken(a) }()
	// Give the second caller time to capture its pre-wait snapshot.
	time.Sleep(50 * time.Millisecond)
	close(block.release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal("concurrent refresh failed")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 || a.Snapshot().Generation != 1 {
		t.Fatal("rotating refresh token was reused")
	}
}

func TestRefreshResultIdentityAndCredentialChecks(t *testing.T) {
	original := auth.Snapshot{Generation: 1, AccessToken: "a", RefreshToken: "r", Site: auth.SiteCN, UID: "u", FilePath: "f"}
	for _, field := range []string{"site", "uid", "file", "access", "refresh", "generation"} {
		t.Run(field, func(t *testing.T) {
			current := original
			switch field {
			case "site":
				current.Site = auth.SiteIntl
				current.Generation++
			case "uid":
				current.UID = "other"
				current.Generation++
			case "file":
				current.FilePath = "other"
				current.Generation++
			case "access":
				current.AccessToken = "other"
			case "refresh":
				current.RefreshToken = "other"
			case "generation":
				current.Generation++
			}
			err := refreshResult(current, original, io.ErrUnexpectedEOF)
			if (err == nil) != (field == "generation") {
				t.Fatal("inconsistent stale refresh error suppression")
			}
		})
	}
}

func TestChatStreamContextCancelAndClose(t *testing.T) {
	for _, idle := range []time.Duration{0, time.Second} {
		for _, cancelParent := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			c := New()
			c.IdleTimeout = idle
			var requestContext context.Context
			c.ChatHTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				requestContext = r.Context()
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: done\n\n"))}, nil
			})}
			rc, _, _, err := c.ChatStreamContext(ctx, &auth.Auth{AccessToken: "a"}, []byte(`{}`), "")
			if err != nil {
				cancel()
				t.Fatal("stream creation failed")
			}
			if cancelParent {
				cancel()
			} else {
				rc.Close()
			}
			select {
			case <-requestContext.Done():
			case <-time.After(time.Second):
				t.Fatal("stream context not released")
			}
			rc.Close()
			cancel()
		}
	}
}
