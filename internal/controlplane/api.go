package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/owainlewis/machinist/internal/config"
)

// The documented API authenticates machine clients with scoped bearer tokens
// stored hashed in the control plane database. Browsers keep the existing
// same-origin CSRF defense and are not asked to hold tokens.

// identityContextKey carries the authenticated principal to handlers.
type identityContextKey struct{}

// anonymous names changes made without a token (browser CSRF or auth-off).
var anonymous = TokenIdentity{Name: "anonymous"}

// workerTokenIdentity names changes made with the shared worker token.
var workerTokenIdentity = TokenIdentity{Name: "worker-token"}

// authOff reports whether authentication is disabled for this server.
func (s *Server) authOff() bool { return s.authMode == config.AuthNone }

// bearerToken extracts the plaintext bearer credential from a request.
func bearerToken(request *http.Request) (string, bool) {
	header := request.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")
	return token, found && token != ""
}

// authenticate resolves the request's bearer token to an identity and scope.
// The shared worker token authenticates as a write-capable principal so
// existing automations keep working; database tokens carry explicit scopes.
func (s *Server) authenticate(request *http.Request) (TokenIdentity, string, bool) {
	token, ok := bearerToken(request)
	if !ok {
		return TokenIdentity{}, "", false
	}
	if constantTimeEquals(token, s.workerToken) {
		return workerTokenIdentity, TokenScopeWrite, true
	}
	return s.store.AuthenticateToken(request.Context(), token)
}

// authorizeRead requires a valid bearer token with any scope, falling back to
// the same-origin browser CSRF defense. Auth-off servers pass everything.
func (s *Server) authorizeRead(next http.HandlerFunc) http.HandlerFunc {
	return s.authorize(next, TokenScopeRead)
}

// authorizeWrite requires a write-scope bearer token (or the browser CSRF
// defense) and attaches the responsible principal so handlers can audit.
func (s *Server) authorizeWrite(next http.HandlerFunc) http.HandlerFunc {
	return s.authorize(next, TokenScopeWrite)
}

// authorizeTokenList guards GET /api/v1/tokens. Bearer callers need write
// scope; browsers use the artifact read defense because browsers omit the
// Origin header on same-origin GET requests, which the write-path browser
// check requires.
func (s *Server) authorizeTokenList(next http.HandlerFunc) http.HandlerFunc {
	return s.authorizeWithBrowser(next, TokenScopeWrite, s.validBrowserReadRequest)
}

// authorize enforces the token-or-CSRF rule for the required scope. Browser
// writes must also carry Fetch Metadata: the CSRF token is served by the open
// status endpoint and Origin is self-reported, so a credential-less client
// could forge both and write anyway; browsers always attach Sec-Fetch-Site
// and forbid pages from setting it.
func (s *Server) authorize(next http.HandlerFunc, required string) http.HandlerFunc {
	browserCheck := s.validBrowserRequest
	switch required {
	case TokenScopeWrite:
		browserCheck = s.validBrowserWriteRequest
	case TokenScopeRead:
		// Browsers omit the Origin header on same-origin GET requests, so the
		// read defense is the artifact-read one: valid CSRF, no cross-site mark.
		browserCheck = s.validBrowserReadRequest
	}
	return s.authorizeWithBrowser(next, required, browserCheck)
}

// authorizeWithBrowser enforces the token rule with the given browser check
// for requests that present no bearer credential.
func (s *Server) authorizeWithBrowser(next http.HandlerFunc, required string, validBrowser func(*http.Request) bool) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if s.authOff() {
			next(response, request.WithContext(withIdentity(request.Context(), anonymous)))
			return
		}
		identity, scope, authenticated := s.authenticate(request)
		if !authenticated {
			if _, hasBearer := bearerToken(request); hasBearer {
				// A presented credential that does not authenticate.
				writeUnauthorized(response)
				return
			}
			if !validBrowser(request) {
				if required == TokenScopeWrite {
					writeError(response, http.StatusForbidden, errors.New("write requests require a write-scope token or a same-origin browser request"))
				} else {
					writeUnauthorized(response)
				}
				return
			}
			identity = anonymous
		} else if required == TokenScopeWrite && scope != TokenScopeWrite {
			if scope == TokenScopeRead {
				writeForbidden(response, errors.New("this action requires a write-scope token"))
			} else {
				writeForbidden(response, errors.New("token scope is not recognized"))
			}
			return
		}
		next(response, request.WithContext(withIdentity(request.Context(), identity)))
	}
}

// withIdentity attaches the authenticated principal to the request context.
func withIdentity(ctx context.Context, identity TokenIdentity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// identityFrom returns the request's authenticated principal, defaulting to
// the anonymous browser principal when the route carries no token.
func identityFrom(request *http.Request) TokenIdentity {
	if identity, ok := request.Context().Value(identityContextKey{}).(TokenIdentity); ok {
		return identity
	}
	return anonymous
}

// recordAudit writes one audit entry for the change behind this request.
func (s *Server) recordAudit(request *http.Request, action, detail string) {
	if err := s.store.RecordAudit(request.Context(), identityFrom(request), action, detail); err != nil {
		log.Printf("audit %q: %v", action, err)
	}
}

// authorizedArtifactBearer extends artifact access to scoped API tokens so
// bots can read run transcripts.
func (s *Server) authorizedArtifactBearer(request *http.Request) bool {
	_, _, ok := s.authenticate(request)
	return ok
}

// listJobsHandler serves GET /api/v1/jobs with an optional ?state= filter.
func (s *Server) listJobsHandler(response http.ResponseWriter, request *http.Request) {
	jobs, err := s.store.ListJobs(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if state := request.URL.Query().Get("state"); state != "" {
		filtered := jobs[:0]
		for _, job := range jobs {
			if job.State == state {
				filtered = append(filtered, job)
			}
		}
		jobs = filtered
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, map[string][]Job{"jobs": jobs})
}

// getJobHandler serves GET /api/v1/jobs/{id}.
func (s *Server) getJobHandler(response http.ResponseWriter, request *http.Request) {
	job, err := s.store.GetJob(request.Context(), request.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(response, http.StatusNotFound, errors.New("job not found"))
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, job)
}

// getRunHandler serves GET /api/v1/runs/{id}.
func (s *Server) getRunHandler(response http.ResponseWriter, request *http.Request) {
	run, err := s.store.GetRun(request.Context(), request.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(response, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, run)
}

// getSettingsHandler serves GET /api/v1/settings.
func (s *Server) getSettingsHandler(response http.ResponseWriter, request *http.Request) {
	values, err := s.store.Settings(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, map[string]map[string]string{"settings": values})
}

// putSettingHandler serves PUT /api/v1/settings/{key} with {"value": ...}.
func (s *Server) putSettingHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	key := request.PathValue("key")
	if !validSettingKey(key) {
		writeError(response, http.StatusBadRequest, errors.New("setting key must be letters, digits, dash or underscore"))
		return
	}
	if err := validateProjectSetting(key, input.Value); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err := s.store.PutSetting(request.Context(), key, input.Value); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.recordAudit(request, "settings.write", key)
	writeJSON(response, http.StatusOK, map[string]string{"key": key, "value": input.Value})
}

// validSettingKey keeps settings keys to a conservative charset.
func validSettingKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// listTokensHandler serves GET /api/v1/tokens. Token management needs write
// scope; revoked tokens stay listed so the audit trail stays legible.
func (s *Server) listTokensHandler(response http.ResponseWriter, request *http.Request) {
	tokens, err := s.store.ListTokens(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, map[string][]APIToken{"tokens": tokens})
}

// createTokenHandler serves POST /api/v1/tokens with {"name","scope"} and
// returns the plaintext secret exactly once.
func (s *Server) createTokenHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	token, plaintext, err := s.store.CreateToken(request.Context(), input.Name, input.Scope)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	s.recordAudit(request, "tokens.create", token.Name+" ("+token.Scope+")")
	writeJSON(response, http.StatusCreated, map[string]any{
		"token": token, "secret": plaintext,
	})
}

// revokeTokenHandler serves DELETE /api/v1/tokens/{id}.
func (s *Server) revokeTokenHandler(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if err := s.store.RevokeToken(request.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(response, http.StatusNotFound, errors.New("token not found"))
			return
		}
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.recordAudit(request, "tokens.revoke", id)
	response.WriteHeader(http.StatusNoContent)
}

// auditLogHandler serves GET /api/v1/audit-log?limit=N (default 100).
func (s *Server) auditLogHandler(response http.ResponseWriter, request *http.Request) {
	limit := 100
	if raw := request.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}
	entries, err := s.store.AuditLog(request.Context(), limit)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, map[string]any{"entries": entries})
}

// writeForbidden answers a validly authenticated request that lacks scope.
func writeForbidden(response http.ResponseWriter, err error) {
	writeError(response, http.StatusForbidden, err)
}

// trustedListenHost reports whether the bind host is loopback or within the
// Tailscale CGNAT range, the two interfaces where reduced auth is acceptable.
func trustedListenHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// 100.64.0.0/10 is the carrier-grade NAT range Tailscale assigns nodes.
	if ip4 := ip.To4(); ip4 != nil {
		return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
	}
	return false
}

// validateListenAddress accepts loopback binds always and Tailscale CGNAT
// binds; public interfaces are rejected. Auth-off mode additionally requires a
// trusted interface, which callers enforce with warnOnAuthDisabled.
func validateListenAddress(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", listen, err)
	}
	if trustedListenHost(host) {
		return nil
	}
	return fmt.Errorf("listen address %q must use a loopback or Tailscale host", listen)
}

// warnOnAuthDisabled logs the startup warning required when auth is opt-out.
func warnOnAuthDisabled(address net.Addr) {
	log.Printf("WARNING: API authentication is disabled (auth = \"none\"); anyone who can reach %s may read and change Machinist state", address)
}
