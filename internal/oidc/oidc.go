package oidc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/ricsam/opencode-workspaces/internal/auth"
	"github.com/ricsam/opencode-workspaces/internal/database"
	"github.com/ricsam/opencode-workspaces/internal/model"
	"golang.org/x/oauth2"
)

const settingKey = "auth.oidc"
const flowCookie = "ocw_oidc_flow"

type Manager struct {
	Store        *database.Store
	Auth         *auth.Manager
	PublicURL    *url.URL
	SigningKey   []byte
	CookieSecure bool
}

type flow struct {
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Expires  time.Time `json:"expires"`
}
type claims struct {
	Subject           string   `json:"sub"`
	Email             string   `json:"email"`
	EmailVerified     bool     `json:"email_verified"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Nonce             string   `json:"nonce"`
	Groups            []string `json:"groups"`
}

func (m *Manager) Settings(ctx context.Context) (model.OIDCSettings, error) {
	var settings model.OIDCSettings
	ok, err := m.Store.Setting(ctx, settingKey, &settings)
	if err != nil {
		return settings, err
	}
	settings.Configured = ok && settings.Issuer != "" && settings.ClientID != "" && settings.ClientSecret != ""
	settings.HasClientSecret = settings.ClientSecret != ""
	return settings, nil
}

func (m *Manager) Save(ctx context.Context, actor string, settings model.OIDCSettings, remote string) error {
	settings.Issuer = strings.TrimSuffix(strings.TrimSpace(settings.Issuer), "/")
	settings.ClientID = strings.TrimSpace(settings.ClientID)
	if len(settings.Scopes) == 0 {
		settings.Scopes = []string{coreoidc.ScopeOpenID, "profile", "email"}
	}
	if settings.Enabled && (settings.Issuer == "" || settings.ClientID == "" || settings.ClientSecret == "") {
		return errors.New("issuer, client ID, and client secret are required when OIDC is enabled")
	}
	if settings.Issuer != "" {
		if _, err := coreoidc.NewProvider(ctx, settings.Issuer); err != nil {
			return fmt.Errorf("OIDC discovery failed: %w", err)
		}
	}
	settings.Configured = false
	settings.HasClientSecret = false
	return m.Store.SetSetting(ctx, actor, settingKey, settings, true, remote)
}

func (m *Manager) Start(w http.ResponseWriter, r *http.Request) error {
	settings, err := m.Settings(r.Context())
	if err != nil {
		return err
	}
	if !settings.Enabled || !settings.Configured {
		return errors.New("OIDC login is not enabled")
	}
	provider, err := coreoidc.NewProvider(r.Context(), settings.Issuer)
	if err != nil {
		return err
	}
	state, err := token(24)
	if err != nil {
		return err
	}
	nonce, err := token(24)
	if err != nil {
		return err
	}
	verifier, err := token(48)
	if err != nil {
		return err
	}
	f := flow{State: state, Nonce: nonce, Verifier: verifier, Expires: time.Now().Add(10 * time.Minute)}
	encoded, err := m.encodeFlow(f)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: encoded, Path: "/platform/auth/oidc", HttpOnly: true, Secure: m.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	config := oauth2.Config{ClientID: settings.ClientID, ClientSecret: settings.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: m.callbackURL(), Scopes: settings.Scopes}
	challenge := base64.RawURLEncoding.EncodeToString(hash([]byte(verifier)))
	http.Redirect(w, r, config.AuthCodeURL(state, coreoidc.Nonce(nonce), oauth2.SetAuthURLParam("code_challenge", challenge), oauth2.SetAuthURLParam("code_challenge_method", "S256")), http.StatusFound)
	return nil
}

func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) (model.User, error) {
	cookie, err := r.Cookie(flowCookie)
	if err != nil {
		return model.User{}, errors.New("OIDC flow cookie is missing")
	}
	f, err := m.decodeFlow(cookie.Value)
	if err != nil || time.Now().After(f.Expires) {
		return model.User{}, errors.New("OIDC flow expired")
	}
	if !hmac.Equal([]byte(f.State), []byte(r.URL.Query().Get("state"))) {
		return model.User{}, errors.New("OIDC state mismatch")
	}
	settings, err := m.Settings(r.Context())
	if err != nil {
		return model.User{}, err
	}
	if !settings.Enabled || !settings.Configured {
		return model.User{}, errors.New("OIDC is disabled")
	}
	provider, err := coreoidc.NewProvider(r.Context(), settings.Issuer)
	if err != nil {
		return model.User{}, err
	}
	config := oauth2.Config{ClientID: settings.ClientID, ClientSecret: settings.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: m.callbackURL(), Scopes: settings.Scopes}
	tokenSet, err := config.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.SetAuthURLParam("code_verifier", f.Verifier))
	if err != nil {
		return model.User{}, fmt.Errorf("OIDC token exchange: %w", err)
	}
	raw, ok := tokenSet.Extra("id_token").(string)
	if !ok {
		return model.User{}, errors.New("OIDC response has no ID token")
	}
	verified, err := provider.Verifier(&coreoidc.Config{ClientID: settings.ClientID}).Verify(r.Context(), raw)
	if err != nil {
		return model.User{}, err
	}
	var c claims
	if err := verified.Claims(&c); err != nil {
		return model.User{}, err
	}
	if c.Nonce != f.Nonce {
		return model.User{}, errors.New("OIDC nonce mismatch")
	}
	if c.Subject == "" || c.Email == "" || !c.EmailVerified {
		return model.User{}, errors.New("OIDC requires a verified email")
	}
	if !domainAllowed(c.Email, settings.AllowedDomains) {
		return model.User{}, errors.New("email domain is not allowed")
	}
	user, err := m.Store.OIDCUser(r.Context(), settings.Issuer, c.Subject)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, database.ErrNotFound) {
		return model.User{}, err
	}
	if !settings.AutoProvision {
		return model.User{}, errors.New("OIDC account has not been provisioned")
	}
	role := "user"
	if intersects(c.Groups, settings.AdminGroups) {
		role = "admin"
	}
	username := safeUsername(c.PreferredUsername, c.Email)
	return m.Store.ProvisionOIDCUser(r.Context(), settings.Issuer, c.Subject, username, c.Name, c.Email, role, r.RemoteAddr)
}

func (m *Manager) callbackURL() string {
	return strings.TrimSuffix(m.PublicURL.String(), "/") + "/platform/auth/oidc/callback"
}
func domainAllowed(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	parts := strings.Split(strings.ToLower(email), "@")
	if len(parts) != 2 {
		return false
	}
	for _, d := range allowed {
		if parts[1] == strings.ToLower(strings.TrimSpace(d)) {
			return true
		}
	}
	return false
}
func intersects(values, allowed []string) bool {
	for _, v := range values {
		for _, a := range allowed {
			if v == a {
				return true
			}
		}
	}
	return false
}

var unsafeUsername = regexp.MustCompile(`[^a-z0-9._-]+`)

func safeUsername(preferred, email string) string {
	v := strings.ToLower(strings.TrimSpace(preferred))
	if v == "" {
		v = strings.Split(email, "@")[0]
	}
	v = unsafeUsername.ReplaceAllString(v, "-")
	v = strings.Trim(v, "-._")
	if len(v) < 2 {
		v = "user-" + v
	}
	return v
}
func token(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hash(v []byte) []byte { sum := sha256.Sum256(v); return sum[:] }
func (m *Manager) encodeFlow(f flow) (string, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, m.SigningKey)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (m *Manager) decodeFlow(value string) (flow, error) {
	var f flow
	p := strings.Split(value, ".")
	if len(p) != 2 {
		return f, errors.New("invalid flow")
	}
	mac := hmac.New(sha256.New, m.SigningKey)
	mac.Write([]byte(p[0]))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(p[1])) {
		return f, errors.New("invalid flow signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}
