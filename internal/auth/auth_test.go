package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"domain":""},"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.AccessToken != "at" || sa.RefreshToken != "rt" || sa.ExpiresAt != 1753600000 {
		t.Errorf("tokens: %+v", sa)
	}
	if sa.UID != "u1" || sa.EnterpriseID != "e1" || sa.Nickname != "n1" {
		t.Errorf("account: %+v", sa)
	}
}

func TestParseSiteIntlPreserved(t *testing.T) {
	raw := []byte(`{"site":"intl","auth":{"accessToken":"at","refreshToken":"rt"},"account":{"uid":"u-intl"}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Site != SiteIntl {
		t.Fatalf("site lost during parse: got %q, want %q", a.Site, SiteIntl)
	}
}

func TestSiteFromInternationalAliases(t *testing.T) {
	for _, site := range []string{"intl", "INTL", " international ", "global"} {
		if got := SiteFrom(site); got != SiteIntl {
			t.Errorf("SiteFrom(%q) = %q, want intl", site, got)
		}
	}
}

func TestParseLegacyIntlDomainWithoutSite(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","domain":"https://www.codebuddy.ai"},"account":{"uid":"u-intl"}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Site != SiteIntl {
		t.Fatalf("legacy intl domain classified as %q", a.Site)
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2"}`)
	sa, err := Parse(raw)
	if err != nil || sa.UID != "u2" || sa.AccessToken != "at" {
		t.Fatalf("flat: %+v %v", sa, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp,
		Site: SiteIntl, DeviceToken: "device-1"}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" ||
		b.Site != SiteIntl || b.DeviceToken != "device-1" {
		t.Errorf("roundtrip: %+v", b)
	}
}

// TestLoadDirLoadsAllValid 不再按 region 过滤：所有可解析的 auth 文件都被加载，
// 解析失败的文件静默跳过。
func TestLoadDirLoadsAllValid(t *testing.T) {
	dir := t.TempDir()
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"cn1"}}`
	other := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"example.com"},"account":{"uid":"u2"}}`
	bad := `not json`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(cn), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-u2.json"), []byte(other), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(bad), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 valid accounts, got %+v", list)
	}
	for _, a := range list {
		if a.FilePath == "" {
			t.Error("FilePath not set")
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
}

func TestSnapshotReturnsConsistentCopy(t *testing.T) {
	a := &Auth{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 123, Domain: "d",
		UID: "u", EnterpriseID: "e", Nickname: "n", Site: "GLOBAL",
		FilePath: "auth.json", DeviceToken: "dt",
	}
	s := a.Snapshot()
	if s.AccessToken != "at" || s.RefreshToken != "rt" || s.ExpiresAt != 123 ||
		s.Domain != "d" || s.UID != "u" || s.EnterpriseID != "e" ||
		s.Nickname != "n" || s.Site != SiteIntl || s.FilePath != "auth.json" || s.DeviceToken != "dt" {
		t.Fatalf("snapshot mismatch: %+v", s)
	}
	// 快照必须独立于后续字段更新。
	a.Lock()
	a.AccessToken = "new"
	a.Unlock()
	if s.AccessToken != "at" {
		t.Fatalf("snapshot mutated with auth: %+v", s)
	}
}

func TestNilSnapshot(t *testing.T) {
	var a *Auth
	if got := a.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("nil snapshot=%+v", got)
	}
}
