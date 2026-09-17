package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestFailureDegradeThresholdAndRecovery(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Minute, time.Hour)
	p.Add(&auth.Auth{UID: "cn", Site: auth.SiteCN})
	p.Add(&auth.Auth{UID: "intl", Site: auth.SiteIntl})
	p.NoteFailures("cn")
	if got := p.PickForSiteModel(nil, "m", auth.SiteCN); got == nil {
		t.Fatal("first failure should not degrade")
	}
	p.NoteFailures("cn")
	if got := p.PickForSiteModel(nil, "m", auth.SiteCN); got != nil {
		t.Fatalf("degraded account selected (including fallback): %+v", got)
	}
	if got := p.PickForSiteModel(nil, "m", auth.SiteIntl); got == nil || got.UID != "intl" {
		t.Fatal("CN degrade leaked into INTL")
	}
	p.NoteSuccess("cn")
	if got := p.PickForSiteModel(nil, "m", auth.SiteCN); got == nil {
		t.Fatal("success should clear degrade")
	}
}

func TestFailureDegradeDoesNotExtendActiveWindow(t *testing.T) {
	p := New("")
	p.SetDegrade(1, time.Minute, time.Hour)
	p.Add(&auth.Auth{UID: "u", Site: auth.SiteCN})
	p.NoteFailures("u")
	p.mu.RLock()
	first := p.byUID["u"].degradeUntil
	p.mu.RUnlock()
	p.NoteFailures("u")
	p.mu.RLock()
	second := p.byUID["u"].degradeUntil
	p.mu.RUnlock()
	if !first.Equal(second) {
		t.Fatalf("active degrade window extended: %v -> %v", first, second)
	}
}

func TestDegradeStateAndModelExemption(t *testing.T) {
	p := New("")
	p.SetDegrade(1, time.Minute, time.Hour)
	p.Add(&auth.Auth{UID: "u", Site: auth.SiteCN})
	p.mu.Lock()
	e := p.byUID["u"]
	e.until, e.coolKind, e.softRateModel = time.Now().Add(time.Hour), CoolSoft, "limited"
	p.mu.Unlock()
	if !p.ServableNow() {
		t.Fatal("model exemption should remain servable")
	}
	p.NoteFailures("u")
	if p.ServableNow() || p.ServableNowForSite(auth.SiteCN) {
		t.Fatal("health probe bypassed degrade")
	}
	if len(p.AvailableUIDsForModel("other")) != 0 || p.PickByUIDForModel("u", "other") != nil || p.PickExcludingForModel(nil, "other") != nil {
		t.Fatal("model exemption or fallback bypassed degrade")
	}
	_, healthy, cooling, _, _ := p.CountsDetailed()
	if healthy != 0 || cooling != 1 {
		t.Fatalf("counts healthy=%d cooling=%d", healthy, cooling)
	}
	st, _ := p.Status("u")
	if !st.Cooling || st.DegradeUntil.IsZero() {
		t.Fatalf("missing degrade status: %+v", st)
	}
	p.ReviveDisabled("u")
	st, _ = p.Status("u")
	if !st.DegradeUntil.IsZero() || st.DegradeFails != 0 || !p.ServableNow() {
		t.Fatal("revive disabled should clear degrade even when not disabled")
	}
	p.NoteFailures("u")
	if !p.Revive("u") || !p.ServableNow() {
		t.Fatal("manual revive did not recover")
	}
}

func TestDegradePersistenceAndExpiredFiltering(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Minute, time.Hour)
	p.Add(&auth.Auth{UID: "partial"})
	p.Add(&auth.Auth{UID: "active"})
	p.Add(&auth.Auth{UID: "expired"})
	p.NoteFailures("partial")
	p.NoteFailures("active")
	p.NoteFailures("active")
	p.mu.Lock()
	p.byUID["expired"].degradeUntil = time.Now().Add(-time.Second)
	p.byUID["expired"].consecutiveFails = 1
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	if !sf.Accounts["expired"].DegradeUntil.IsZero() || sf.Accounts["expired"].ConsecutiveFails != 0 {
		t.Fatal("expired state saved")
	}
	q := New("")
	q.applySnapshotLocked(snapshot{stateFile: sf})
	if q.byUID["partial"].consecutiveFails != 1 || q.byUID["active"].healthy(time.Now()) {
		t.Fatal("snapshot lost degrade state")
	}
	q.applyAccountsLocked(map[string]stateAccount{"expired": {DegradeUntil: time.Now().Add(-time.Second), ConsecutiveFails: 4}})
	if !q.byUID["expired"].degradeUntil.IsZero() || q.byUID["expired"].consecutiveFails != 0 {
		t.Fatal("expired state restored")
	}
	p.stateFp = t.TempDir() + "/state.json"
	p.dirty.Store(true)
	p.Flush()
	r := New("")
	r.stateFp = p.stateFp
	r.load()
	if r.byUID["partial"].consecutiveFails != 1 || r.byUID["active"].healthy(time.Now()) {
		t.Fatal("local persistence lost degrade")
	}
}

func TestDegradeDefaultCapExpiryAndSuccessReset(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u"})
	for i := 0; i < 4; i++ {
		p.NoteFailures("u")
	}
	st, _ := p.Status("u")
	if st.DegradeFails != 4 || st.Cooling {
		t.Fatal("default threshold must be 5")
	}
	p.NoteSuccess("u")
	st, _ = p.Status("u")
	if st.DegradeFails != 0 {
		t.Fatal("success did not reset partial streak")
	}
	p.SetDegrade(1, 3*time.Hour, 2*time.Hour)
	p.NoteFailures("u")
	st, _ = p.Status("u")
	if !st.Cooling || st.CoolKind != "degrade" || st.CoolRemaining <= 0 || time.Until(st.DegradeUntil) > 2*time.Hour {
		t.Fatalf("invalid cap/status: %+v", st)
	}
	p.mu.Lock()
	p.byUID["u"].degradeUntil = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if p.Pick() == nil || !p.ServableNow() {
		t.Fatal("expired degrade did not recover")
	}
}
