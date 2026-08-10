package proxy

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ricsam/opencode-workspaces/internal/database"
	"github.com/ricsam/opencode-workspaces/internal/kube"
	"github.com/ricsam/opencode-workspaces/internal/model"
)

type backend struct {
	target   *url.URL
	password string
}

type Gateway struct {
	Store      *database.Store
	Controller *kube.Controller
	log        *slog.Logger
	transport  http.RoundTripper
	mu         sync.Mutex
	lastTouch  map[string]time.Time
	backends   map[string]backend
}

func New(store *database.Store, controller *kube.Controller, logger *slog.Logger) *Gateway {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0,
	}
	return &Gateway{Store: store, Controller: controller, log: logger, transport: transport, lastTouch: map[string]time.Time{}, backends: map[string]backend{}}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request, user model.User) {
	workspace, err := g.Store.Workspace(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "workspace unavailable", http.StatusServiceUnavailable)
		return
	}
	if workspace.DesiredState != "running" {
		g.forgetBackend(user.ID)
		if err := g.Controller.Start(r.Context(), user.ID); err != nil {
			g.log.Error("start workspace", "error", err)
			http.Error(w, "failed to start workspace", http.StatusServiceUnavailable)
			return
		}
		workspace.DesiredState = "running"
	}
	backend, err := g.backend(r.Context(), user.ID)
	if err != nil {
		g.starting(w, r, workspace)
		return
	}
	g.touch(r.Context(), user.ID)
	proxy := httputil.NewSingleHostReverseProxy(backend.target)
	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		req.Host = backend.target.Host
		req.Header.Del("Cookie")
		req.Header.Del("Authorization")
		req.Header.Del("X-CSRF-Token")
		req.Header.Del("X-Forwarded-User")
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:"+backend.password)))
		req.Header.Set("X-Forwarded-User", user.ID)
	}
	proxy.Transport = g.transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		g.forgetBackend(user.ID)
		g.log.Warn("workspace proxy error", "workspace", workspace.ResourceName, "error", err)
		http.Error(w, "workspace connection unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

func (g *Gateway) backend(ctx context.Context, userID string) (backend, error) {
	g.mu.Lock()
	cached, ok := g.backends[userID]
	g.mu.Unlock()
	if ok {
		return cached, nil
	}

	address, password, err := g.Controller.Backend(ctx, userID)
	if err != nil {
		return backend{}, err
	}
	target, err := url.Parse(address)
	if err != nil {
		return backend{}, err
	}
	resolved := backend{target: target, password: password}
	g.mu.Lock()
	g.backends[userID] = resolved
	g.mu.Unlock()
	return resolved, nil
}

func (g *Gateway) forgetBackend(userID string) {
	g.mu.Lock()
	delete(g.backends, userID)
	g.mu.Unlock()
}

func (g *Gateway) touch(ctx context.Context, userID string) {
	g.mu.Lock()
	last := g.lastTouch[userID]
	if time.Since(last) < time.Minute {
		g.mu.Unlock()
		return
	}
	g.lastTouch[userID] = time.Now()
	g.mu.Unlock()
	g.Store.TouchWorkspace(ctx, userID)
}
func (g *Gateway) starting(w http.ResponseWriter, r *http.Request, workspace model.Workspace) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Refresh", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Starting workspace</title><style>body{margin:0;background:#09090b;color:#fafafa;font:16px system-ui;display:grid;place-items:center;min-height:100vh}.card{max-width:32rem;padding:2rem;border:1px solid #27272a;border-radius:1rem;background:#18181b}a{color:#a78bfa}</style></head><body><div class="card"><h1>Starting your workspace</h1><p>OpenCode is being prepared. This page refreshes automatically.</p><p><a href="/platform/">Workspace controls</a></p></div></body></html>`))
		return
	}
	http.Error(w, "workspace is starting; retry shortly", http.StatusServiceUnavailable)
}
