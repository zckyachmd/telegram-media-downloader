package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/webassets"
)

func TestMCPControlDashboardRoutes(t *testing.T) {
	static, err := fs.Sub(webassets.FS, "public")
	if err != nil {
		t.Fatal(err)
	}
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir(), Port: 0, Static: static})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	login := httptest.NewRecorder()
	a.Handler().ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"password":"test-admin-password"}`)))
	if login.Code != http.StatusOK || len(login.Result().Cookies()) == 0 {
		t.Fatalf("admin login = %d %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	for _, tc := range []struct {
		path string
		key  string
	}{
		{"/api/mcp/agents", "agents"},
		{"/api/mcp/approvals", "approvals"},
		{"/api/mcp/audit?limit=100", "entries"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.AddCookie(cookie)
			response := httptest.NewRecorder()
			a.Handler().ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s = %d %s", tc.path, response.Code, response.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body[tc.key]; !ok {
				t.Fatalf("GET %s omitted %q: %s", tc.path, tc.key, response.Body.String())
			}
		})
	}

	asset := httptest.NewRecorder()
	a.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/js/mcp-control.js", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "/api/mcp/agents") {
		t.Fatalf("MCP dashboard asset = %d", asset.Code)
	}
}
