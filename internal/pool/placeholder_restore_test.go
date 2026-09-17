package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestSyncToDirRemovesRestoredPlaceholderMissingCredentials(t *testing.T) {
	p := New("")
	p.mu.Lock()
	p.applyAccountsLocked(map[string]stateAccount{"missing": {Credits: 1000}})
	p.mu.Unlock()
	p.SyncToDir(nil)
	if got := p.AuthByUID("missing"); got != nil {
		t.Fatal("restored placeholder missing from auth scan must be removed")
	}
	if got := p.PickForSite(auth.SiteCN); got != nil {
		t.Fatal("missing credentials must not remain selectable after sync")
	}
}

func TestSyncToDirReplacesRestoredPlaceholderWithIntlCredentials(t *testing.T) {
	p := New("")
	p.mu.Lock()
	p.applyAccountsLocked(map[string]stateAccount{"intl": {Credits: 1000}})
	p.mu.Unlock()
	p.SyncToDir([]*auth.Auth{{UID: "intl", Site: auth.SiteIntl, AccessToken: "at"}})
	if got := p.PickForSite(auth.SiteIntl); got == nil || got.Snapshot().AccessToken == "" {
		t.Fatal("restored placeholder should be replaced by full INTL credentials")
	}
	if got := p.PickForSite(auth.SiteCN); got != nil {
		t.Fatal("INTL restored credentials leaked into CN selection")
	}
}
