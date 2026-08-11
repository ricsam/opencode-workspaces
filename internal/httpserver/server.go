package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ricsam/opencode-workspaces/internal/auth"
	"github.com/ricsam/opencode-workspaces/internal/config"
	"github.com/ricsam/opencode-workspaces/internal/database"
	"github.com/ricsam/opencode-workspaces/internal/kube"
	"github.com/ricsam/opencode-workspaces/internal/model"
	"github.com/ricsam/opencode-workspaces/internal/oidc"
	workspaceproxy "github.com/ricsam/opencode-workspaces/internal/proxy"
	webassets "github.com/ricsam/opencode-workspaces/internal/web"
)

type Server struct {
	Config     config.Config
	Store      *database.Store
	Auth       *auth.Manager
	OIDC       *oidc.Manager
	Controller *kube.Controller
	Gateway    *workspaceproxy.Gateway
	log        *slog.Logger
}
type viewData struct {
	Title        string
	Error        string
	CSRF         string
	User         model.User
	Brand        model.Branding
	OIDC         model.OIDCSettings
	HasLocalAuth bool
	Workspace    model.Workspace
	Status       model.WorkspaceStatus
	Rows         []adminRow
	Audit        []model.AuditEvent
	CallbackURL  string
}
type adminRow struct {
	User      model.User
	Workspace model.Workspace
	Status    model.WorkspaceStatus
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{2,64}$`)

func New(cfg config.Config, store *database.Store, authManager *auth.Manager, oidcManager *oidc.Manager, controller *kube.Controller, logger *slog.Logger) *Server {
	return &Server{Config: cfg, Store: store, Auth: authManager, OIDC: oidcManager, Controller: controller, Gateway: workspaceproxy.New(store, controller, logger), log: logger}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(webassets.Assets, "static")
	mux.Handle("GET /platform/static/", http.StripPrefix("/platform/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.Store.Healthy(ctx); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("ready"))
	})
	mux.HandleFunc("GET /platform/setup", s.setupGet)
	mux.HandleFunc("POST /platform/setup", s.setupPost)
	mux.HandleFunc("GET /platform/login", s.loginGet)
	mux.HandleFunc("POST /platform/login", s.loginPost)
	mux.HandleFunc("POST /platform/logout", s.logout)
	mux.HandleFunc("GET /platform/auth/oidc/login", func(w http.ResponseWriter, r *http.Request) {
		if err := s.OIDC.Start(w, r); err != nil {
			s.error(w, r, err, 400)
		}
	})
	mux.HandleFunc("GET /platform/auth/oidc/callback", s.oidcCallback)
	mux.HandleFunc("GET /platform/api/v1/bootstrap", s.bootstrapAPI)
	mux.HandleFunc("GET /platform/", s.home)
	mux.HandleFunc("POST /platform/workspace/stop", s.stopWorkspace)
	mux.HandleFunc("GET /platform/admin", s.admin)
	mux.HandleFunc("POST /platform/admin/users", s.createUser)
	mux.HandleFunc("POST /platform/admin/users/{id}", s.updateUser)
	mux.HandleFunc("POST /platform/account/password", s.changePassword)
	mux.HandleFunc("POST /platform/admin/branding", s.saveBranding)
	mux.HandleFunc("POST /platform/admin/oidc", s.saveOIDC)
	mux.HandleFunc("POST /platform/admin/workspaces/{id}/{action}", s.workspaceAction)
	mux.HandleFunc("/", s.gateway)
	return securityHeaders(s.Auth.Middleware(mux))
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if strings.HasPrefix(r.URL.Path, "/platform/") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setupGet(w http.ResponseWriter, r *http.Request) {
	count, err := s.Store.UserCount(r.Context())
	if err != nil {
		s.error(w, r, err, 500)
		return
	}
	if count > 0 {
		http.Redirect(w, r, "/platform/login", 303)
		return
	}
	s.render(w, "setup", s.data(r))
}
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.error(w, r, err, 400)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	if !usernamePattern.MatchString(username) {
		s.renderError(w, r, "setup", "invalid username")
		return
	}
	hash, err := auth.HashPassword(r.FormValue("password"))
	if err != nil {
		s.renderError(w, r, "setup", err.Error())
		return
	}
	user, err := s.Store.BootstrapAdmin(r.Context(), username, strings.TrimSpace(r.FormValue("display_name")), strings.TrimSpace(r.FormValue("email")), hash)
	if err != nil {
		s.renderError(w, r, "setup", err.Error())
		return
	}
	if _, err := s.Auth.NewSession(r.Context(), w, r, user.ID); err != nil {
		s.error(w, r, err, 500)
		return
	}
	http.Redirect(w, r, "/platform/admin", 303)
}
func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	count, _ := s.Store.UserCount(r.Context())
	if count == 0 {
		http.Redirect(w, r, "/platform/setup", 303)
		return
	}
	if _, ok := auth.UserFromContext(r.Context()); ok {
		http.Redirect(w, r, "/platform/", 303)
		return
	}
	s.render(w, "login", s.data(r))
}
func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	key := r.RemoteAddr + ":" + strings.ToLower(r.FormValue("username"))
	if !s.Auth.LoginAllowed(key) {
		http.Error(w, "too many login attempts", 429)
		return
	}
	user, hash, err := s.Store.AuthenticateLocal(r.Context(), r.FormValue("username"))
	if err != nil || !auth.VerifyPassword(hash, r.FormValue("password")) || user.Disabled {
		s.renderError(w, r, "login", "invalid username or password")
		return
	}
	s.Auth.LoginSucceeded(key)
	s.Store.RecordLogin(r.Context(), user.ID)
	if _, err := s.Auth.NewSession(r.Context(), w, r, user.ID); err != nil {
		s.error(w, r, err, 500)
		return
	}
	http.Redirect(w, r, "/platform/", 303)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.CSRFValid(r) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	s.Auth.ClearSession(r.Context(), w, r)
	http.Redirect(w, r, "/platform/login", 303)
}
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	user, err := s.OIDC.Callback(w, r)
	if err != nil {
		s.error(w, r, err, 401)
		return
	}
	if _, err := s.Auth.NewSession(r.Context(), w, r, user.ID); err != nil {
		s.error(w, r, err, 500)
		return
	}
	s.Store.RecordLogin(r.Context(), user.ID)
	http.Redirect(w, r, "/platform/", 303)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	data := s.data(r)
	data.User = user
	var err error
	data.HasLocalAuth, err = s.Store.HasLocalCredential(r.Context(), user.ID)
	if err != nil {
		s.error(w, r, err, 500)
		return
	}
	data.Workspace, _ = s.Store.Workspace(r.Context(), user.ID)
	data.Status, _ = s.Controller.Status(r.Context(), data.Workspace)
	s.render(w, "home", data)
}
func (s *Server) stopWorkspace(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if !s.csrf(w, r) {
		return
	}
	if err := s.Controller.Stop(r.Context(), user.ID, user.ID, r.RemoteAddr); err != nil {
		s.error(w, r, err, 500)
		return
	}
	http.Redirect(w, r, "/platform/", 303)
}
func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	data := s.data(r)
	data.User = user
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.error(w, r, err, 500)
		return
	}
	workspaces, err := s.Store.ListWorkspaces(r.Context())
	if err != nil {
		s.error(w, r, err, 500)
		return
	}
	workspacesByUser := make(map[string]model.Workspace, len(workspaces))
	for _, workspace := range workspaces {
		workspacesByUser[workspace.UserID] = workspace
	}
	statuses, err := s.Controller.Statuses(r.Context(), workspaces)
	if err != nil {
		s.error(w, r, err, 500)
		return
	}
	for _, u := range users {
		workspace := workspacesByUser[u.ID]
		status := statuses[workspace.ResourceName]
		data.Rows = append(data.Rows, adminRow{u, workspace, status})
	}
	data.Audit, _ = s.Store.AuditEvents(r.Context(), 50)
	data.CallbackURL = strings.TrimSuffix(s.Config.PublicURL.String(), "/") + "/platform/auth/oidc/callback"
	s.render(w, "admin", data)
}
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok || !s.csrf(w, r) {
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	role := r.FormValue("role")
	if !usernamePattern.MatchString(username) || (role != "user" && role != "admin") {
		s.error(w, r, errors.New("invalid username or role"), 400)
		return
	}
	hash, err := auth.HashPassword(r.FormValue("password"))
	if err != nil {
		s.error(w, r, err, 400)
		return
	}
	_, err = s.Store.CreateLocalUser(r.Context(), actor.ID, username, r.FormValue("display_name"), r.FormValue("email"), role, hash, r.RemoteAddr)
	if err != nil {
		s.error(w, r, err, 400)
		return
	}
	http.Redirect(w, r, "/platform/admin", 303)
}
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok || !s.csrf(w, r) {
		return
	}
	id := r.PathValue("id")
	role := r.FormValue("role")
	if role != "user" && role != "admin" {
		s.error(w, r, errors.New("invalid role"), 400)
		return
	}
	var hash string
	var err error
	if password := r.FormValue("password"); password != "" {
		hash, err = auth.HashPassword(password)
		if err != nil {
			s.error(w, r, err, 400)
			return
		}
	}
	disabled := r.FormValue("disabled") != ""
	if id == actor.ID && (disabled || role != "admin") {
		s.error(w, r, errors.New("an administrator cannot disable or demote their own account"), 400)
		return
	}
	if err := s.Store.UpdateUser(r.Context(), actor.ID, id, strings.TrimSpace(r.FormValue("display_name")), strings.TrimSpace(r.FormValue("email")), role, disabled, hash, r.RemoteAddr); err != nil {
		s.error(w, r, err, 400)
		return
	}
	http.Redirect(w, r, "/platform/admin", 303)
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok || !s.csrf(w, r) {
		return
	}
	current, oldHash, err := s.Store.AuthenticateLocal(r.Context(), user.Username)
	if err != nil || current.ID != user.ID || !auth.VerifyPassword(oldHash, r.FormValue("current_password")) {
		s.error(w, r, errors.New("current password is invalid"), 403)
		return
	}
	hash, err := auth.HashPassword(r.FormValue("new_password"))
	if err != nil {
		s.error(w, r, err, 400)
		return
	}
	if err := s.Store.UpdateUser(r.Context(), user.ID, user.ID, user.DisplayName, user.Email, user.Role, user.Disabled, hash, r.RemoteAddr); err != nil {
		s.error(w, r, err, 500)
		return
	}
	http.Redirect(w, r, "/platform/", 303)
}
func (s *Server) saveBranding(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok || !s.csrf(w, r) {
		return
	}
	brand := model.Branding{Name: strings.TrimSpace(r.FormValue("name")), LogoURL: safeHTTPURL(r.FormValue("logo_url")), Favicon: safeHTTPURL(r.FormValue("favicon_url")), Accent: r.FormValue("accent")}
	if brand.Name == "" || !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(brand.Accent) {
		s.error(w, r, errors.New("invalid branding"), 400)
		return
	}
	if err := s.Store.SetSetting(r.Context(), actor.ID, "branding", brand, false, r.RemoteAddr); err != nil {
		s.error(w, r, err, 500)
		return
	}
	http.Redirect(w, r, "/platform/admin", 303)
}
func (s *Server) saveOIDC(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok || !s.csrf(w, r) {
		return
	}
	old, _ := s.OIDC.Settings(r.Context())
	secret := r.FormValue("client_secret")
	if secret == "" {
		secret = old.ClientSecret
	}
	settings := model.OIDCSettings{Enabled: r.FormValue("enabled") != "", Issuer: r.FormValue("issuer"), ClientID: r.FormValue("client_id"), ClientSecret: secret, Scopes: []string{"openid", "profile", "email"}, AutoProvision: r.FormValue("auto_provision") != "", LinkByEmail: r.FormValue("link_by_email") != "", AllowedDomains: csv(r.FormValue("allowed_domains")), AdminGroups: csv(r.FormValue("admin_groups"))}
	if err := s.OIDC.Save(r.Context(), actor.ID, settings, r.RemoteAddr); err != nil {
		s.error(w, r, err, 400)
		return
	}
	http.Redirect(w, r, "/platform/admin", 303)
}
func (s *Server) workspaceAction(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok || !s.csrf(w, r) {
		return
	}
	id, action := r.PathValue("id"), r.PathValue("action")
	var err error
	switch action {
	case "start":
		err = s.Controller.Start(r.Context(), id)
	case "stop":
		err = s.Controller.Stop(r.Context(), actor.ID, id, r.RemoteAddr)
	case "restart":
		err = s.Controller.Restart(r.Context(), actor.ID, id, r.RemoteAddr)
	case "purge":
		err = s.Controller.Purge(r.Context(), actor.ID, id, r.RemoteAddr)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.error(w, r, err, 500)
		return
	}
	http.Redirect(w, r, "/platform/admin", 303)
}
func (s *Server) bootstrapAPI(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	brand := s.branding(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"id": user.ID, "username": user.Username, "displayName": user.DisplayName, "role": user.Role, "isAdmin": user.IsAdmin()}, "branding": brand, "adminUrl": "/platform/admin"})
}
func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		if r.Method == "GET" && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/platform/login?next="+url.QueryEscape(r.URL.RequestURI()), 302)
			return
		}
		http.Error(w, "authentication required", 401)
		return
	}
	s.Gateway.ServeHTTP(w, r, user)
}

func (s *Server) data(r *http.Request) viewData {
	user, _ := auth.UserFromContext(r.Context())
	csrf := ""
	if c, err := r.Cookie("ocw_csrf"); err == nil {
		csrf = c.Value
	}
	oidcSettings, _ := s.OIDC.Settings(r.Context())
	oidcSettings.ClientSecret = ""
	return viewData{User: user, CSRF: csrf, Brand: s.branding(r.Context()), OIDC: oidcSettings}
}
func (s *Server) branding(ctx context.Context) model.Branding {
	brand := model.Branding{Name: "OpenCode Workspaces", Accent: "#7c3aed"}
	s.Store.Setting(ctx, "branding", &brand)
	if brand.Name == "" {
		brand.Name = "OpenCode Workspaces"
	}
	if brand.Accent == "" {
		brand.Accent = "#7c3aed"
	}
	return brand
}
func (s *Server) render(w http.ResponseWriter, name string, data viewData) {
	files := []string{"templates/base.html", "templates/" + name + ".html"}
	tmpl, err := template.New("base").Funcs(template.FuncMap{"join": func(v []string) string { return strings.Join(v, ",") }}).ParseFS(webassets.Assets, files...)
	if err != nil {
		s.error(w, nil, err, 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		s.log.Error("render", "error", err)
	}
}
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, name, message string) {
	data := s.data(r)
	data.Error = message
	s.render(w, name, data)
}
func (s *Server) error(w http.ResponseWriter, r *http.Request, err error, status int) {
	s.log.Warn("request failed", "error", err, "status", status)
	http.Error(w, http.StatusText(status), status)
}
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (model.User, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/platform/login", 302)
		return user, false
	}
	return user, true
}
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (model.User, bool) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return user, false
	}
	if !user.IsAdmin() {
		http.Error(w, "forbidden", 403)
		return user, false
	}
	return user, true
}
func (s *Server) csrf(w http.ResponseWriter, r *http.Request) bool {
	if !s.Auth.CSRFValid(r) {
		http.Error(w, "invalid CSRF token", 403)
		return false
	}
	return true
}
func csv(value string) []string {
	parts := strings.Split(value, ",")
	out := []string{}
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
func safeHTTPURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return parsed.String()
}

var _ = fmt.Sprint
