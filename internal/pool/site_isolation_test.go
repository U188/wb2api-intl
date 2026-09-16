package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestSiteAwareSelectionNeverCrossesSite(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "cn", Site: auth.SiteCN})
	p.Add(&auth.Auth{UID: "intl", Site: auth.SiteIntl})
	p.SetCredits("cn", 100, 0)
	p.SetCredits("intl", 1000, 0)

	if got := p.PickForSiteModel(nil, "glm-5.2", auth.SiteCN); got == nil || got.UID != "cn" {
		t.Fatalf("CN pick crossed site: %+v", got)
	}
	if got := p.PickForSiteModel(nil, "glm-5.2", auth.SiteIntl); got == nil || got.UID != "intl" {
		t.Fatalf("Intl pick crossed site: %+v", got)
	}
	if got := p.PickByUIDForSiteModel("cn", "glm-5.2", auth.SiteIntl); got != nil {
		t.Fatalf("sticky UID direct pick crossed site: %+v", got)
	}

	cnUIDs := p.AvailableUIDsForSiteModel(auth.SiteCN, "glm-5.2")
	intlUIDs := p.AvailableUIDsForSiteModel(auth.SiteIntl, "glm-5.2")
	if len(cnUIDs) != 1 || cnUIDs[0] != "cn" || len(intlUIDs) != 1 || intlUIDs[0] != "intl" {
		t.Fatalf("site UID sets leaked: cn=%v intl=%v", cnUIDs, intlUIDs)
	}
	if !p.ServableNowForSite(auth.SiteCN) || !p.ServableNowForSite(auth.SiteIntl) {
		t.Fatal("both populated sites should be servable")
	}
}

func TestSiteAwareSelectionDoesNotFallbackAcrossSite(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn", Site: auth.SiteCN})
	if got := p.PickForSiteModel(nil, "glm-5.2", auth.SiteIntl); got != nil {
		t.Fatalf("missing Intl pool must return nil, got %+v", got)
	}
	if p.ServableNowForSite(auth.SiteIntl) {
		t.Fatal("missing Intl pool must not be servable")
	}
}

func TestSameUIDCannotOverwriteAcrossSites(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "same", Site: auth.SiteCN, AccessToken: "cn"})
	p.SetCredits("same", 123, 456)
	p.Add(&auth.Auth{UID: "same", Site: auth.SiteIntl, AccessToken: "intl"})
	got := p.AuthByUID("same")
	if got == nil || got.Site != auth.SiteCN || got.AccessToken != "cn" {
		t.Fatalf("cross-site UID overwrote account: %+v", got)
	}
	st, ok := p.Status("same")
	if !ok || st.Credits != 123 || st.CreditsTotal != 456 {
		t.Fatalf("cross-site UID corrupted state: %+v ok=%v", st, ok)
	}
}

func TestIntlCredentialReplacesPersistedPlaceholder(t *testing.T) {
	p := New("")
	p.byUID["intl"] = &entry{a: &auth.Auth{UID: "intl"}, credits: 321}
	p.SyncToDir([]*auth.Auth{{UID: "intl", Site: auth.SiteIntl, AccessToken: "token"}})
	got := p.AuthByUID("intl")
	if got == nil || got.Site != auth.SiteIntl || got.AccessToken != "token" {
		t.Fatalf("placeholder blocked international credential: %+v", got)
	}
	st, ok := p.Status("intl")
	if !ok || st.Credits != 321 {
		t.Fatalf("placeholder state was not preserved: %+v ok=%v", st, ok)
	}
}
