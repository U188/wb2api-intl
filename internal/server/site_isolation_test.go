package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func siteTestHandler(t *testing.T, calls map[string]int) *Handler {
	t.Helper()
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "cn", Site: auth.SiteCN, AccessToken: "cn-token", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "intl", Site: auth.SiteIntl, AccessToken: "intl-token", ExpiresAt: 9999999999})
	p.SetCredits("cn", 1000, 0)
	p.SetCredits("intl", 1000, 0)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls[r.Header.Get("Authorization")]++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		ChatBaseCN: "https://cn.invalid", ChatBaseIntl: "https://intl.invalid",
	}
	sess := session.New(session.Config{Store: redisstore.Noop{}, AvailableForSiteModel: p.AvailableUIDsForSiteModel})
	return NewHandler(Config{Pool: p, Upstream: up, Session: sess})
}

func TestSiteChatRoutesAndLegacyCN(t *testing.T) {
	calls := map[string]int{}
	h := siteTestHandler(t, calls)
	body := `{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"same"}}`
	for _, path := range []string{"/cn/v1/chat/completions", "/v1/chat/completions", "/v1/cn/chat/completions"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"/intl/v1/chat/completions", "/v1/intl/chat/completions"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body)
		}
	}
	if calls["Bearer cn-token"] != 3 || calls["Bearer intl-token"] != 2 {
		t.Fatalf("routing crossed site: calls=%v", calls)
	}
	if h.cfg.Session.Count() != 2 {
		t.Fatalf("same raw conversation must have independent site namespaces, binds=%d", h.cfg.Session.Count())
	}
}

func TestSiteModelsFallbackDoesNotUseOtherSiteAccount(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched, dynamicModelsCache.lastFail = time.Time{}, time.Time{}
	dynamicModelsCache.intl = siteModelsCache{}
	dynamicModelsCache.Unlock()

	calls := map[string]int{}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn", Site: auth.SiteCN, AccessToken: "cn-token", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls[r.Header.Get("Authorization")]++
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("boom")), Header: make(http.Header)}, nil
	})}, ChatBaseCN: "https://cn.invalid", ChatBaseIntl: "https://intl.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up, Default1MContext: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/intl/models", nil))
	if rec.Code != 200 || calls["Bearer cn-token"] != 0 {
		t.Fatalf("Intl models must static-fallback without CN fetch: code=%d calls=%v", rec.Code, calls)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Data) == 0 {
		t.Fatalf("models response: %v %s", err, rec.Body)
	}
	foundV41Flash := false
	for _, m := range resp.Data {
		if m["site"] != auth.SiteIntl {
			t.Fatalf("model site=%v want intl", m["site"])
		}
		if m["id"] == "deepseek-v4.1-flash" {
			foundV41Flash = true
			if m["max_input_tokens"] != float64(1_000_000) || m["max_output_tokens"] != float64(384_000) {
				t.Fatalf("deepseek-v4.1-flash limits=%v", m)
			}
			contextWindow, ok := m["contextWindow"].(map[string]any)
			if !ok || !reflect.DeepEqual(contextWindow["supportedLengths"], []any{float64(200_000), float64(1_000_000)}) ||
				contextWindow["defaultLength"] != float64(1_000_000) {
				t.Fatalf("deepseek-v4.1-flash contextWindow=%v", m["contextWindow"])
			}
		}
	}
	if !foundV41Flash {
		t.Fatal("international model catalog is missing deepseek-v4.1-flash")
	}
}

func TestIntlModelsDefaultContextHotSwitch(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{Default1MContext: true})
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), Live: live})
	assertDefault := func(want float64) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/intl/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
		var resp struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		for _, model := range resp.Data {
			if model["id"] != "deepseek-v4.1-flash" {
				continue
			}
			snake := model["context_window"].(map[string]any)
			camel := model["contextWindow"].(map[string]any)
			if snake["default_length"] != want || camel["defaultLength"] != want {
				t.Fatalf("default context=%v/%v want=%v", snake["default_length"], camel["defaultLength"], want)
			}
			return
		}
		t.Fatal("deepseek-v4.1-flash missing")
	}
	assertDefault(float64(1_000_000))
	live.Store(livecfg.Snapshot{Default1MContext: false})
	assertDefault(float64(200_000))
}

func TestSiteHealthEndpoints(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn", Site: auth.SiteCN})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	for path, want := range map[string]int{"/healthz": 200, "/cn/healthz": 200, "/healthz/cn": 200, "/intl/healthz": 503, "/healthz/intl": 503} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("%s code=%d want %d", path, rec.Code, want)
		}
	}
}

func TestIntlRetryStaysWithinIntl(t *testing.T) {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "intl-bad", Site: auth.SiteIntl, AccessToken: "intl-bad", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "intl-good", Site: auth.SiteIntl, AccessToken: "intl-good", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "cn", Site: auth.SiteCN, AccessToken: "cn", ExpiresAt: 9999999999})
	p.SetCredits("intl-bad", 2000, 0)
	p.SetCredits("intl-good", 1000, 0)
	p.SetCredits("cn", 9999, 0)
	calls := map[string]int{}
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		token := r.Header.Get("Authorization")
		calls[token]++
		if token == "Bearer intl-bad" {
			return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":500}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})}, ChatBaseCN: "https://cn.invalid", ChatBaseIntl: "https://intl.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/intl/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 || calls["Bearer intl-bad"] != 1 || calls["Bearer intl-good"] != 1 || calls["Bearer cn"] != 0 {
		t.Fatalf("Intl retry crossed site: code=%d calls=%v body=%s", rec.Code, calls, rec.Body)
	}
}

func TestDynamicModelCachesSeparatedBySite(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched, dynamicModelsCache.lastFail = time.Time{}, time.Time{}
	dynamicModelsCache.intl = siteModelsCache{}
	dynamicModelsCache.Unlock()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn", Site: auth.SiteCN, AccessToken: "cn", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "intl", Site: auth.SiteIntl, AccessToken: "intl", ExpiresAt: 9999999999})
	calls := map[string]int{}
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		token := r.Header.Get("Authorization")
		calls[token]++
		id := "cn-model"
		body := `{"code":0,"data":{"models":[{"id":"` + id + `","maxInputTokens":1,"maxOutputTokens":1}],"agents":[{"name":"cli","models":["` + id + `"]}]}}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}, ChatBaseCN: "https://cn.invalid", ChatBaseIntl: "https://intl.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up})
	for _, tc := range []struct{ path, id, site string }{{"/cn/v1/models", "cn-model", auth.SiteCN}, {"/v1/cn/models", "cn-model", auth.SiteCN}, {"/intl/v1/models", "gpt-5.6-sol", auth.SiteIntl}, {"/v1/intl/models", "gpt-5.6-sol", auth.SiteIntl}, {"/cn/v1/models", "cn-model", auth.SiteCN}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"`+tc.id+`"`) || !strings.Contains(rec.Body.String(), `"site":"`+tc.site+`"`) {
			t.Fatalf("%s leaked cache: %s", tc.path, rec.Body)
		}
	}
	if calls["Bearer cn"] != 1 || calls["Bearer intl"] != 0 {
		t.Fatalf("cache/catalog calls=%v", calls)
	}
}
