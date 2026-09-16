package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestEffortCacheSeparatedBySite(t *testing.T) {
	var sent = map[string][]byte{}
	modelCalls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models") {
			modelCalls++
			body := `{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":1,"maxOutputTokens":1,"reasoning":{"supportedEfforts":["low"]}}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`
			return jsonResp(200, body), nil
		}
		raw, _ := io.ReadAll(r.Body)
		sent[r.URL.Host] = raw
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	})
	c.ChatBaseCN = "https://cn.invalid"
	c.ChatBaseIntl = "https://intl.invalid"
	cn := &auth.Auth{UID: "cn", Site: auth.SiteCN, AccessToken: "x"}
	intl := &auth.Auth{UID: "intl", Site: auth.SiteIntl, AccessToken: "y"}
	if _, err := c.FetchModels(cn); err != nil {
		t.Fatal(err)
	}
	models, err := c.FetchModels(intl)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) < 17 || modelCalls != 1 {
		t.Fatalf("intl catalog=%d model network calls=%d, want >=17 and CN-only call", len(models), modelCalls)
	}
	for _, tc := range []struct {
		a    *auth.Auth
		want string
	}{{cn, "low"}, {intl, "xhigh"}} {
		if rc, _, _, err := c.ChatStream(tc.a, []byte(`{"model":"glm-5.2","reasoning_effort":"max"}`), ""); err != nil {
			t.Fatal(err)
		} else {
			rc.Close()
		}
		var obj map[string]any
		if err := json.Unmarshal(sent[c.chatBase(tc.a)[8:]], &obj); err != nil {
			t.Fatalf("body: %v", err)
		}
		if obj["reasoning_effort"] != tc.want {
			t.Errorf("site=%s effort=%v want %s", tc.a.Site, obj["reasoning_effort"], tc.want)
		}
	}
}
