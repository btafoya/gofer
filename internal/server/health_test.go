package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithHealthAnswersProbesBeforeNext(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	ready := func(context.Context) error { return nil }
	handler := withHealth(next, ready)

	for _, tt := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodHead, "/readyz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusTeapot},
		{http.MethodGet, "/login", http.StatusTeapot},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(tt.method, "http://gofer.example"+tt.path, nil))
		if recorder.Code != tt.want {
			t.Fatalf("%s %s = %d, want %d", tt.method, tt.path, recorder.Code, tt.want)
		}
	}
}

func TestWithHealthReadinessFollowsDependency(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	failing := withHealth(next, func(context.Context) error { return errors.New("db down") })
	recorder := httptest.NewRecorder()
	failing.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with failing dependency = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	failing.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz must stay live when dependency fails, got %d", recorder.Code)
	}
}

func TestHealthURLUsesLoopbackForWildcardBinds(t *testing.T) {
	for input, want := range map[string]string{
		"0.0.0.0:8090":   "http://127.0.0.1:8090/healthz",
		"[::]:8090":      "http://127.0.0.1:8090/healthz",
		"127.0.0.1:9000": "http://127.0.0.1:9000/healthz",
		"":               "http://127.0.0.1:8090/healthz",
	} {
		got, err := healthURL(input)
		if err != nil || got != want {
			t.Fatalf("healthURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := healthURL("not-an-address"); err == nil {
		t.Fatal("healthURL accepted an address without a port")
	}
}
