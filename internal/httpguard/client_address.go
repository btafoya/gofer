package httpguard

import (
	"context"
	"net/http"
	"net/netip"
)

type clientAddressKey struct{}

// ClientAddressFromContext returns the client address resolved by Config.Middleware.
func ClientAddressFromContext(ctx context.Context) (string, bool) {
	address, ok := ctx.Value(clientAddressKey{}).(netip.Addr)
	if !ok {
		return "", false
	}
	return address.String(), true
}

// withClientAddress stores the proxy-aware client address, honoring forwarded
// headers only from configured trusted proxies.
func (c *Config) withClientAddress(r *http.Request) *http.Request {
	address, ok := c.clientAddress(r)
	if !ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), clientAddressKey{}, address))
}
