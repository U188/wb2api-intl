package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestWAFSiteGateBlocksOnlyAffectedRoute(t *testing.T) {
	oldBase := rotateBackoffBase
	rotateBackoffBase = 0
	defer func() { rotateBackoffBase = oldBase }()

	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "cn1", Site: auth.SiteCN, AccessToken: "cn1-at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "cn2", Site: auth.SiteCN, AccessToken: "cn2-at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "intl", Site: auth.SiteIntl, AccessToken: "intl-at", ExpiresAt: 9999999999})
	for _, uid := range []string{"cn1", "cn2", "intl"} {
		p.SetCredits(uid, 1000, 0)
	}
	calls := map[string]int{}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			uid := r.Header.Get("X-User-Id")
			calls[uid]++
			if uid == "intl" {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			}
			return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("<html>APISIX WAF</html>"))}, nil
		})},
		ChatBaseCN: "https://cn.invalid", ChatBaseIntl: "https://intl.invalid",
	}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec
	}
	first := get("/cn/v1/chat/completions")
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("CN WAF code=%d body=%s", first.Code, first.Body)
	}
	if calls["cn1"]+calls["cn2"] != 2 {
		t.Fatalf("expected two distinct CN WAF probes, calls=%v", calls)
	}
	if !h.waf.forSite(auth.SiteCN).active(time.Now()) {
		t.Fatal("CN gate not active after two distinct WAF hits")
	}
	before := calls["cn1"] + calls["cn2"]
	blocked := get("/cn/v1/chat/completions")
	if blocked.Code != http.StatusServiceUnavailable || !strings.Contains(blocked.Body.String(), "waf_ip_blocked") {
		t.Fatalf("active CN gate did not fail fast: code=%d body=%s", blocked.Code, blocked.Body)
	}
	if calls["cn1"]+calls["cn2"] != before {
		t.Fatalf("active CN gate still hit upstream: calls=%v", calls)
	}
	intl := get("/intl/v1/chat/completions")
	if intl.Code != http.StatusOK || calls["intl"] != 1 {
		t.Fatalf("CN WAF gate leaked to INTL: code=%d calls=%v", intl.Code, calls)
	}
}
