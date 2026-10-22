package httpguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAddressFromContextHonorsOnlyTrustedProxies(t *testing.T) {
	cfg, err := newConfig("0.0.0.0:8090", "http://localhost:8090", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg.trustedProxyCIDRs, err = parseCIDRs("test", "10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, peer, forwarded, want string
	}{
		{"direct client ignores forwarded spoof", "203.0.113.9:4000", "192.168.0.25", "203.0.113.9"},
		{"trusted proxy yields forwarded client", "10.0.0.1:4000", "198.51.100.7", "198.51.100.7"},
		{"trusted proxy chain", "10.0.0.1:4000", "198.51.100.7, 10.0.0.2", "198.51.100.7"},
		{"trusted proxy without header resolves nothing", "10.0.0.1:4000", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = ClientAddressFromContext(r.Context())
			})
			request := httptest.NewRequest(http.MethodGet, "http://localhost:8090/login", nil)
			request.RemoteAddr = tt.peer
			if tt.forwarded != "" {
				request.Header.Set("X-Forwarded-For", tt.forwarded)
			}
			cfg.Middleware(next).ServeHTTP(httptest.NewRecorder(), request)
			if got != tt.want {
				t.Fatalf("client address = %q, want %q", got, tt.want)
			}
		})
	}
}
