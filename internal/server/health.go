package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/httpguard"
)

// withHealth answers /healthz and /readyz before host, network and auth checks,
// so kubelet and Docker probes work without a trusted Host header.
func withHealth(next http.Handler, ready func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isProbe := r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
		if !isProbe || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/readyz" {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := ready(ctx); err != nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
}

// ProbeHealth queries the local /healthz endpoint for `gofer healthcheck`.
func ProbeHealth(listenAddr string) error {
	url, err := healthURL(listenAddr)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
}

// healthURL maps a listen address to a loopback URL when it binds all interfaces.
func healthURL(listenAddr string) (string, error) {
	if strings.TrimSpace(listenAddr) == "" {
		listenAddr = httpguard.DefaultListenAddr
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
