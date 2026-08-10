package proxy

import (
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestCachedBackendDoesNotCallController(t *testing.T) {
	g := &Gateway{log: slog.Default(), lastTouch: map[string]time.Time{}, backends: map[string]backend{}}
	g.backends["user-1"] = backend{target: mustURL(t, "http://workspace.example:4096"), password: "secret"}

	got, err := g.backend(t.Context(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.target.String() != "http://workspace.example:4096" || got.password != "secret" {
		t.Fatalf("unexpected backend: %#v", got)
	}
}

func TestNewUsesConnectionPoolingTransport(t *testing.T) {
	g := New(nil, nil, slog.Default())
	transport, ok := g.transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", g.transport)
	}
	if transport.MaxIdleConnsPerHost < 2 {
		t.Fatalf("per-host connection pool is too small: %d", transport.MaxIdleConnsPerHost)
	}
}

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
