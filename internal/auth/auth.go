package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ricsam/opencode-workspaces/internal/database"
	"github.com/ricsam/opencode-workspaces/internal/model"
	"golang.org/x/crypto/argon2"
)

const CookieName = "ocw_session"
const sessionLifetime = 7 * 24 * time.Hour

type contextKey string

const userKey contextKey = "user"

type Manager struct {
	Store        *database.Store
	SigningKey   []byte
	CookieSecure bool
	limiter      *loginLimiter
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func New(store *database.Store, key []byte, secure bool) *Manager {
	return &Manager{Store: store, SigningKey: key, CookieSecure: secure, limiter: &loginLimiter{attempts: map[string][]time.Time{}}}
}

func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return "argon2id$v=19$m=65536,t=3,p=2$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

func ValidatePassword(password string) error {
	if len(password) < 12 {
		return errors.New("password must contain at least 12 characters")
	}
	if len(password) > 1024 {
		return errors.New("password is too long")
	}
	return nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[3])
	expected, err2 := base64.RawStdEncoding.DecodeString(parts[4])
	if err1 != nil || err2 != nil || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return hmac.Equal(actual, expected)
}

func (m *Manager) LoginAllowed(key string) bool { return m.limiter.allow(key) }
func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-10 * time.Minute)
	old := l.attempts[key]
	recent := old[:0]
	for _, t := range old {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 10 {
		l.attempts[key] = recent
		return false
	}
	l.attempts[key] = append(recent, now)
	return true
}
func (m *Manager) LoginSucceeded(key string) {
	m.limiter.mu.Lock()
	delete(m.limiter.attempts, key)
	m.limiter.mu.Unlock()
}

func (m *Manager) NewSession(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return "", err
	}
	signed := token + "." + m.sign(token)
	err = m.Store.CreateSession(ctx, database.Hash(token), database.Hash(csrf), userID, r.UserAgent(), remoteAddr(r), time.Now().Add(sessionLifetime))
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: signed, Path: "/", HttpOnly: true, Secure: m.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionLifetime.Seconds())})
	http.SetCookie(w, &http.Cookie{Name: "ocw_csrf", Value: csrf, Path: "/platform", HttpOnly: false, Secure: m.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionLifetime.Seconds())})
	return csrf, nil
}

func (m *Manager) ClearSession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(CookieName); err == nil {
		if token, ok := m.token(cookie.Value); ok {
			m.Store.DeleteSession(ctx, database.Hash(token))
		}
	}
	for _, name := range []string{CookieName, "ocw_csrf"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: func() string {
			if name == "ocw_csrf" {
				return "/platform"
			}
			return "/"
		}(), HttpOnly: name == CookieName, Secure: m.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	}
}

func (m *Manager) User(r *http.Request) (model.User, bool) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return model.User{}, false
	}
	token, ok := m.token(cookie.Value)
	if !ok {
		return model.User{}, false
	}
	user, _, _, err := m.Store.Session(r.Context(), database.Hash(token))
	return user, err == nil
}

func (m *Manager) CSRFValid(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	token, ok := m.token(cookie.Value)
	if !ok {
		return false
	}
	_, expected, _, err := m.Store.Session(r.Context(), database.Hash(token))
	if err != nil {
		return false
	}
	provided := r.Header.Get("X-CSRF-Token")
	if provided == "" {
		provided = r.FormValue("csrf")
	}
	return hmac.Equal(expected, database.Hash(provided))
}

func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := m.User(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}
func UserFromContext(ctx context.Context) (model.User, bool) {
	user, ok := ctx.Value(userKey).(model.User)
	return user, ok
}

func (m *Manager) sign(token string) string {
	mac := hmac.New(sha256.New, m.SigningKey)
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (m *Manager) token(value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return "", false
	}
	return parts[0], hmac.Equal([]byte(parts[1]), []byte(m.sign(parts[0])))
}
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func remoteAddr(r *http.Request) string {
	if value := r.Header.Get("X-Forwarded-For"); value != "" {
		return strings.TrimSpace(strings.Split(value, ",")[0])
	}
	return r.RemoteAddr
}
