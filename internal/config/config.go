package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address       string
	DatabaseURL   string
	PublicURL     *url.URL
	SessionKey    []byte
	EncryptionKey []byte
	Namespace     string
	OwnerName     string
	CookieSecure  bool
	TrustProxy    bool
	Workspace     Workspace
}

type Workspace struct {
	Image             string
	ImagePullPolicy   string
	ImagePullSecret   string
	StorageClass      string
	StorageSize       string
	RuntimeClassName  string
	NodeSelector      map[string]string
	CPURequest        string
	MemoryRequest     string
	CPULimit          string
	MemoryLimit       string
	IdleTimeout       time.Duration
	ReconcileInterval time.Duration
	Port              int32
}

func Load() (Config, error) {
	publicURL, err := url.Parse(env("PUBLIC_URL", "http://localhost:8080"))
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return Config{}, errors.New("PUBLIC_URL must be an absolute http(s) URL")
	}
	if publicURL.Scheme != "http" && publicURL.Scheme != "https" {
		return Config{}, errors.New("PUBLIC_URL must use http or https")
	}

	session, err := key("SESSION_SECRET")
	if err != nil {
		return Config{}, err
	}
	encryption, err := key("ENCRYPTION_KEY")
	if err != nil {
		return Config{}, err
	}
	port, err := strconv.Atoi(env("WORKSPACE_PORT", "4096"))
	if err != nil || port < 1 || port > 65535 {
		return Config{}, errors.New("WORKSPACE_PORT must be a valid port")
	}
	idle, err := time.ParseDuration(env("WORKSPACE_IDLE_TIMEOUT", "30m"))
	if err != nil {
		return Config{}, fmt.Errorf("WORKSPACE_IDLE_TIMEOUT: %w", err)
	}
	interval, err := time.ParseDuration(env("RECONCILE_INTERVAL", "15s"))
	if err != nil || interval <= 0 {
		return Config{}, errors.New("RECONCILE_INTERVAL must be a positive duration")
	}
	selector := map[string]string{}
	if raw := strings.TrimSpace(os.Getenv("WORKSPACE_NODE_SELECTOR")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &selector); err != nil {
			return Config{}, fmt.Errorf("WORKSPACE_NODE_SELECTOR must be a JSON object of string labels: %w", err)
		}
		for key, value := range selector {
			if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
				return Config{}, errors.New("WORKSPACE_NODE_SELECTOR labels and values must not be empty")
			}
		}
	}

	cfg := Config{
		Address:       env("HTTP_ADDRESS", ":8080"),
		DatabaseURL:   strings.TrimSpace(os.Getenv("DATABASE_URL")),
		PublicURL:     publicURL,
		SessionKey:    session,
		EncryptionKey: encryption,
		Namespace:     env("POD_NAMESPACE", "opencode-workspaces"),
		OwnerName:     env("WORKSPACE_OWNER_NAME", "opencode-workspaces-owner"),
		CookieSecure:  publicURL.Scheme == "https",
		TrustProxy:    boolEnv("TRUST_PROXY_HEADERS", false),
		Workspace: Workspace{
			Image:             env("WORKSPACE_IMAGE", "ghcr.io/ricsam/opencode-workspace:latest"),
			ImagePullPolicy:   env("WORKSPACE_IMAGE_PULL_POLICY", "IfNotPresent"),
			ImagePullSecret:   strings.TrimSpace(os.Getenv("WORKSPACE_IMAGE_PULL_SECRET")),
			StorageClass:      env("WORKSPACE_STORAGE_CLASS", "rook-ceph-block"),
			StorageSize:       env("WORKSPACE_STORAGE_SIZE", "10Gi"),
			RuntimeClassName:  strings.TrimSpace(os.Getenv("WORKSPACE_RUNTIME_CLASS")),
			NodeSelector:      selector,
			CPURequest:        env("WORKSPACE_CPU_REQUEST", "250m"),
			MemoryRequest:     env("WORKSPACE_MEMORY_REQUEST", "512Mi"),
			CPULimit:          env("WORKSPACE_CPU_LIMIT", "2"),
			MemoryLimit:       env("WORKSPACE_MEMORY_LIMIT", "4Gi"),
			IdleTimeout:       idle,
			ReconcileInterval: interval,
			Port:              int32(port),
		},
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func boolEnv(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func key(name string) ([]byte, error) {
	value := os.Getenv(name)
	if len(value) < 32 {
		return nil, fmt.Errorf("%s must contain at least 32 characters", name)
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	sum := sha256.Sum256([]byte(value))
	return sum[:], nil
}
