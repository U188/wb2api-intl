package panel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestControlRoutesRequireAuthAndRestartKey(t *testing.T) {
	called := 0
	p := New(Config{APIKey: "key", ReloadConfig: func() ([]string, error) { called++; return nil, nil }, Restart: func() error { called++; return nil }})
	for _, path := range []string{"/panel/api/config/reload", "/panel/api/restart"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated code=%d", path, rec.Code)
		}
	}
	if called != 0 {
		t.Fatal("unauthenticated control callback ran")
	}
	req := httptest.NewRequest(http.MethodPost, "/panel/api/restart", nil)
	req.Header.Set("Authorization", "Bearer key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Fatalf("restart code=%d body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatal("accepted response missing JSON content type")
	}
	empty := New(Config{Restart: func() error { t.Fatal("empty key restart callback ran"); return nil }})
	rec = httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/restart", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("empty key restart code=%d", rec.Code)
	}
}

func TestReloadErrorAndRestartConflict(t *testing.T) {
	p := New(Config{APIKey: "key", ReloadConfig: func() ([]string, error) { return nil, errors.New("invalid configuration") }, Restart: func() error { return errors.New("restart already pending") }})
	for _, tc := range []struct {
		path string
		code int
	}{{"/panel/api/config/reload", http.StatusBadRequest}, {"/panel/api/restart", http.StatusConflict}} {
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		req.Header.Set("Authorization", "Bearer key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("%s code=%d want=%d", tc.path, rec.Code, tc.code)
		}
	}
}
