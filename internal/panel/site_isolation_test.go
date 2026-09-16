package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestAccountCNByUIDRejectsIntl422(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "intl-1", Site: auth.SiteIntl})
	p := &Panel{cfg: Config{Pool: pl}}
	rec := httptest.NewRecorder()
	if got := p.accountCNByUID(rec, "intl-1"); got != nil {
		t.Fatal("international account passed CN task helper")
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), cnGamificationOnly) {
		t.Fatalf("unstable error body: %s", rec.Body.String())
	}
}

func TestTaskExecutorsRejectIntlDefensively(t *testing.T) {
	p := &Panel{}
	a := &auth.Auth{UID: "intl-1", Site: auth.SiteIntl}
	results := p.runAutoAll(a)
	if len(results) != 1 || results[0]["status"] != "error" {
		t.Fatalf("runAutoAll did not reject intl: %#v", results)
	}
	if _, err := p.runGrowthQueued(a, "any"); err == nil {
		t.Fatal("runGrowthQueued did not reject intl")
	}
}

func TestModelsRequiresExplicitValidSite(t *testing.T) {
	p := New(Config{Pool: pool.New("")})
	for _, path := range []string{"/panel/api/models", "/panel/api/models?site=other"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s status = %d, want 422", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/models?site=cn", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("site=cn status = %d, want 503 for empty site pool", rec.Code)
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/models?site=intl", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"source":"catalog"`) {
		t.Errorf("intl catalog status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestIntlModelsDefaultContextHotSwitch(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{Default1MContext: true})
	p := New(Config{Pool: pool.New(""), Live: live})
	assertDefault := func(want float64) {
		t.Helper()
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/models?site=intl", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
		var resp struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		for _, model := range resp.Models {
			if model["id"] == "deepseek-v4.1-flash" {
				window := model["context_window"].(map[string]any)
				if window["default_length"] != want {
					t.Fatalf("panel default context=%v want=%v", window["default_length"], want)
				}
				return
			}
		}
		t.Fatal("deepseek-v4.1-flash missing")
	}
	assertDefault(float64(1_000_000))
	live.Store(livecfg.Snapshot{Default1MContext: false})
	assertDefault(float64(200_000))
}

func TestModelsSelectsRequestedSiteAndReturnsSite(t *testing.T) {
	cnCalls, intlCalls := 0, 0
	server := func(calls *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*calls = *calls + 1
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{\"code\":0,\"data\":{\"models\":[{\"id\":\"m1\",\"name\":\"M1\"}],\"agents\":[{\"name\":\"cli\",\"models\":[\"m1\"]}]}}"))
		}))
	}
	cnSrv, intlSrv := server(&cnCalls), server(&intlCalls)
	defer cnSrv.Close()
	defer intlSrv.Close()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "cn-1", Site: auth.SiteCN, AccessToken: "cn-token"})
	pl.Add(&auth.Auth{UID: "intl-1", Site: auth.SiteIntl, AccessToken: "intl-token"})
	p := New(Config{Pool: pl, Upstream: &upstream.Client{HTTP: cnSrv.Client(), ChatBaseCN: cnSrv.URL, ChatBaseIntl: intlSrv.URL}})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/models?site=intl", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "\"site\":\"intl\"") {
		t.Fatalf("unexpected response: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cnCalls != 0 || intlCalls != 0 {
		t.Fatalf("intl catalog must be zero-network: calls cn=%d intl=%d", cnCalls, intlCalls)
	}
}
