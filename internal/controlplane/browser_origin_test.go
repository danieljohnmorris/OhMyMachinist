package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The browser origin check accepts any same-host Origin (loopback, a tailnet
// IP or a tailnet hostname), because the UI is served over Tailscale as well
// as loopback, and keeps rejecting cross-host origins.
func TestValidBrowserRequestAcceptsSameHostAndRejectsCrossHost(t *testing.T) {
	server, webServer := newTestHTTPServer(t)
	defer webServer.Close()

	cases := []struct {
		name   string
		host   string
		origin string
		csrf   string
		want   bool
	}{
		{"loopback", "127.0.0.1:7331", "http://127.0.0.1:7331", server.csrfToken, true},
		{"localhost", "localhost:7331", "http://localhost:7331", server.csrfToken, true},
		{"tailnet ip same host", "192.0.2.10:7331", "http://192.0.2.10:7331", server.csrfToken, true},
		{"tailnet name same host", "machinist.tailnet.example:7331", "http://machinist.tailnet.example:7331", server.csrfToken, true},
		{"host compare is case-insensitive", "Machinist.Tailnet.Example:7331", "http://machinist.tailnet.example:7331", server.csrfToken, true},
		{"cross-host origin", "192.0.2.10:7331", "http://evil.example", server.csrfToken, false},
		{"cross-host origin to loopback", "127.0.0.1:7331", "http://192.0.2.10:7331", server.csrfToken, false},
		{"same hostname other port", "192.0.2.10:7331", "http://192.0.2.10:8080", server.csrfToken, false},
		{"https scheme", "192.0.2.10:7331", "https://192.0.2.10:7331", server.csrfToken, false},
		{"null origin", "192.0.2.10:7331", "null", server.csrfToken, false},
		{"missing origin", "192.0.2.10:7331", "", server.csrfToken, false},
		{"missing csrf", "192.0.2.10:7331", "http://192.0.2.10:7331", "", false},
		{"wrong csrf", "192.0.2.10:7331", "http://192.0.2.10:7331", "not-the-token", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", nil)
			request.Host = tc.host
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			if tc.csrf != "" {
				request.Header.Set("X-Machinist-CSRF", tc.csrf)
			}
			if got := server.validBrowserRequest(request); got != tc.want {
				t.Fatalf("validBrowserRequest = %v, want %v", got, tc.want)
			}
			request.Header.Set("Sec-Fetch-Site", "same-origin")
			if got := server.validBrowserWriteRequest(request); got != tc.want {
				t.Fatalf("validBrowserWriteRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// A browser write over the tailnet with a same-host Origin gets through the
// auth layer; the same write with a cross-host Origin is refused with 403.
func TestBrowserWriteFromTailnetHostAcceptsSameHostRejectsCrossHost(t *testing.T) {
	server, webServer := newTestHTTPServer(t)
	defer webServer.Close()

	post := func(origin string) int {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"prompt":"x"}`))
		request.Host = "machinist.tailnet.example:7331"
		request.Header.Set("Origin", origin)
		request.Header.Set("X-Machinist-CSRF", server.csrfToken)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response.Code
	}
	// Past auth: the body is an invalid selection, so the handler answers 400.
	if code := post("http://machinist.tailnet.example:7331"); code != http.StatusBadRequest {
		t.Fatalf("same-host tailnet write status = %d, want 400 from the handler", code)
	}
	if code := post("http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-host write status = %d, want 403", code)
	}
}

// Read-scope routes the web UI loads (skills, settings, trackers, tickets)
// accept a same-origin browser GET, which carries the CSRF header but no
// Origin, and still refuse a cross-site marker or a missing token.
func TestBrowserReadRoutesUseCSRFReadCheck(t *testing.T) {
	server, webServer := newTestHTTPServer(t)
	defer webServer.Close()

	for _, path := range []string{"/api/v1/skills", "/api/v1/settings", "/api/v1/trackers", "/api/v1/tickets"} {
		get := func(headers map[string]string) int {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Host = "machinist.tailnet.example:7331"
			for key, value := range headers {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			return response.Code
		}
		if code := get(map[string]string{"X-Machinist-CSRF": server.csrfToken}); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Fatalf("%s same-origin browser GET status = %d, want past auth", path, code)
		}
		if code := get(map[string]string{"X-Machinist-CSRF": server.csrfToken, "Sec-Fetch-Site": "cross-site"}); code != http.StatusUnauthorized {
			t.Fatalf("%s cross-site GET status = %d, want 401", path, code)
		}
		if code := get(nil); code != http.StatusUnauthorized {
			t.Fatalf("%s credential-less GET status = %d, want 401", path, code)
		}
	}
}
