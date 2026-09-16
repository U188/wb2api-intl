package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestIntlGamificationRejectedWithoutNetwork(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, ChatBaseIntl: srv.URL,
		BillingBaseCN: srv.URL, BillingBaseIntl: srv.URL, WebBaseCN: srv.URL,
	}
	a := &auth.Auth{UID: "intl-1", AccessToken: "token", Site: auth.SiteIntl}

	if _, err := c.growthJSON(a, http.MethodGet, "/activity/growth/test", nil); err == nil {
		t.Error("growthJSON: expected local CN-only error")
	}
	if _, _, err := c.ClaimReward(a, "task"); err == nil {
		t.Error("ClaimReward: expected local CN-only error")
	}
	if err := c.schoolJSON(a, http.MethodGet, "/tasks", nil, nil); err == nil {
		t.Error("schoolJSON: expected local CN-only error")
	}
	if err := c.ReportMPEvent(a, map[string]any{"eventCode": "test"}); err == nil {
		t.Error("ReportMPEvent: expected local CN-only error")
	}
	if err := c.DailyCheckin(a); err == nil {
		t.Error("DailyCheckin: expected local CN-only error")
	}
	if err := c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "test"}); err == nil {
		t.Error("ReportDesktopEvent: expected local CN-only error")
	}
	if err := c.SetAppearanceTheme(a, "theme"); err == nil {
		t.Error("SetAppearanceTheme: expected local CN-only error")
	}
	if err := c.ReportWebEvent(a, "test", "https://www.workbuddy.cn", "", ""); err == nil {
		t.Error("ReportWebEvent: expected local CN-only error")
	}
	if err := c.ReportChatActivityModel(a, "conv", "req", "glm", "GLM"); err == nil {
		t.Error("ReportChatActivityModel: expected local CN-only error")
	}
	if _, err := c.ClaimGift(a); err == nil {
		t.Error("ClaimGift: expected local CN-only error")
	}
	if _, err := c.ClaimCompensation(a); err == nil {
		t.Error("ClaimCompensation: expected local CN-only error")
	}
	if _, err := c.MarketExpertList(a, ""); err == nil {
		t.Error("MarketExpertList: expected local CN-only error")
	}
	if _, _, err := c.DesktopChatWithExpert(a, "expert"); err == nil {
		t.Error("DesktopChatWithExpert: expected local CN-only error")
	}
	if _, err := c.RunNightChats(a, 1); err == nil {
		t.Error("RunNightChats: expected local CN-only error")
	}
	if calls != 0 {
		t.Fatalf("network calls = %d, want 0", calls)
	}
}
