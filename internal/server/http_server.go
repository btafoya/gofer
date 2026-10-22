package server

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// newHTTPServer sets the timeouts Go leaves off by default. Without them a
// client can hold connections open by sending its request headers very
// slowly, or by leaving idle connections open, until the server runs out.
// Request bodies and responses have no overall limit: uploads can be large
// and the live event stream stays open.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

// endStreamsOnCancel cancels event-stream requests when ctx is done, so
// Shutdown does not wait on streams that never end. Other requests keep their context.
func endStreamsOnCancel(ctx context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			streamCtx, cancel := context.WithCancel(r.Context())
			defer cancel()
			stop := context.AfterFunc(ctx, cancel)
			defer stop()
			r = r.WithContext(streamCtx)
		}
		next.ServeHTTP(w, r)
	})
}
