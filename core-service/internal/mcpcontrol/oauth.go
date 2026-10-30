package mcpcontrol

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (s *Store) OAuthHandler(issuer string) http.Handler {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if err := r.ParseForm(); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
			return
		}
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID == "" || secret == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="tgdl-mcp"`)
			oauthError(w, http.StatusUnauthorized, "invalid_client")
			return
		}
		token, scopes, expires, err := s.IssueClientToken(r.Context(), clientID, secret, r.Form.Get("scope"))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="tgdl-mcp"`)
			oauthError(w, http.StatusUnauthorized, "invalid_client")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		writeJSON(w, http.StatusOK, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": int(expires.Sub(nowUTC()).Seconds()), "scope": strings.Join(scopes, " ")})
	})
	metadata := map[string]any{
		"issuer":                                issuer,
		"token_endpoint":                        issuer + "/oauth/token",
		"grant_types_supported":                 []string{"client_credentials"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		"scopes_supported":                      sortedScopes(knownScopes),
	}
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, metadata)
	})
	resource := map[string]any{
		"resource":                 issuer + "/mcp",
		"authorization_servers":    []string{issuer},
		"scopes_supported":         sortedScopes(knownScopes),
		"bearer_methods_supported": []string{"header"},
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, resource)
	})
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, resource)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func nowUTC() time.Time { return time.Now().UTC() }
