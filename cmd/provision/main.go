package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/ricsam/opencode-workspaces/internal/config"
	"github.com/ricsam/opencode-workspaces/internal/database"
	"github.com/ricsam/opencode-workspaces/internal/model"
	"github.com/ricsam/opencode-workspaces/internal/oidc"
)

type manifest struct {
	Version int        `json:"version"`
	Users   []userSeed `json:"users"`
	OIDC    *oidcSeed  `json:"oidc,omitempty"`
}

type userSeed struct {
	Username       string          `json:"username"`
	DisplayName    string          `json:"displayName"`
	Email          string          `json:"email"`
	Role           string          `json:"role"`
	Disabled       bool            `json:"disabled"`
	OpenCodeConfig json.RawMessage `json:"opencodeConfig"`
	APIKey         string          `json:"apiKey"`
}

type oidcSeed struct {
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"clientId,omitempty"`
	ClientSecret   string   `json:"clientSecret,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	AllowedDomains []string `json:"allowedDomains,omitempty"`
	AdminGroups    []string `json:"adminGroups,omitempty"`
	AutoProvision  bool     `json:"autoProvision"`
	LinkByEmail    bool     `json:"linkExistingUsersByEmail"`
	Enabled        bool     `json:"enabled"`
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{2,64}$`)

func main() {
	if err := run(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, input io.Reader, output io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	store, err := database.Open(ctx, cfg.DatabaseURL, cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	var data manifest
	decoder := json.NewDecoder(io.LimitReader(input, 32<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return fmt.Errorf("decode provisioning manifest: %w", err)
	}
	if data.Version != 1 {
		return errors.New("provisioning manifest version must be 1")
	}
	users, err := store.ListUsers(ctx)
	if err != nil {
		return err
	}
	actor := ""
	for _, user := range users {
		if user.IsAdmin() {
			actor = user.ID
			break
		}
	}
	if actor == "" {
		return errors.New("create the initial enabled administrator before provisioning accounts")
	}

	provisioned := 0
	for index, seed := range data.Users {
		seed.Username = strings.TrimSpace(seed.Username)
		seed.Email = strings.TrimSpace(seed.Email)
		if !usernamePattern.MatchString(seed.Username) || !strings.Contains(seed.Email, "@") {
			return fmt.Errorf("users[%d] has an invalid username or email", index)
		}
		if seed.Role != "admin" && seed.Role != "user" {
			return fmt.Errorf("users[%d] role must be admin or user", index)
		}
		user, err := store.ProvisionUser(ctx, actor, database.ProvisionUserInput{
			Username: seed.Username, DisplayName: strings.TrimSpace(seed.DisplayName), Email: seed.Email,
			Role: seed.Role, Disabled: seed.Disabled,
		})
		if err != nil {
			return fmt.Errorf("provision user %q: %w", seed.Email, err)
		}
		if err := store.SetWorkspaceConfiguration(ctx, actor, user.ID, seed.OpenCodeConfig, seed.APIKey); err != nil {
			return fmt.Errorf("configure workspace for %q: %w", seed.Email, err)
		}
		provisioned++
	}

	if data.OIDC != nil {
		manager := &oidc.Manager{Store: store, PublicURL: cfg.PublicURL, SigningKey: cfg.SessionKey, CookieSecure: cfg.CookieSecure}
		settings := model.OIDCSettings{
			Enabled: data.OIDC.Enabled, Issuer: data.OIDC.Issuer, ClientID: data.OIDC.ClientID,
			ClientSecret: data.OIDC.ClientSecret, Scopes: data.OIDC.Scopes, AutoProvision: data.OIDC.AutoProvision,
			LinkByEmail: data.OIDC.LinkByEmail, AllowedDomains: data.OIDC.AllowedDomains, AdminGroups: data.OIDC.AdminGroups,
		}
		if err := manager.Save(ctx, actor, settings, "provisioning-command"); err != nil {
			return fmt.Errorf("configure OIDC: %w", err)
		}
	}

	return json.NewEncoder(output).Encode(map[string]any{"provisionedUsers": provisioned, "oidcConfigured": data.OIDC != nil})
}
