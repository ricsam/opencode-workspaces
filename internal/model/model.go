package model

import "time"

type User struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
	Email       string    `json:"email,omitempty"`
	Role        string    `json:"role"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"createdAt"`
	LastLoginAt time.Time `json:"lastLoginAt,omitempty"`
}

func (u User) IsAdmin() bool { return u.Role == "admin" && !u.Disabled }

type Workspace struct {
	UserID         string    `json:"userId"`
	ResourceName   string    `json:"resourceName"`
	DesiredState   string    `json:"desiredState"`
	RestartNonce   int64     `json:"restartNonce"`
	LastActivityAt time.Time `json:"lastActivityAt"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	ConfigRevision int64     `json:"configRevision"`
}

type WorkspaceConfiguration struct {
	OpenCodeJSON []byte `json:"opencodeJson"`
	APIKey       string `json:"apiKey"`
	Revision     int64  `json:"revision"`
}

type WorkspaceStatus struct {
	Exists        bool      `json:"exists"`
	Ready         bool      `json:"ready"`
	Replicas      int32     `json:"replicas"`
	ReadyReplicas int32     `json:"readyReplicas"`
	Phase         string    `json:"phase"`
	Message       string    `json:"message,omitempty"`
	PodName       string    `json:"podName,omitempty"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
}

type Branding struct {
	Name    string `json:"name"`
	LogoURL string `json:"logoUrl,omitempty"`
	Favicon string `json:"faviconUrl,omitempty"`
	Accent  string `json:"accent,omitempty"`
}

type OIDCSettings struct {
	Enabled         bool     `json:"enabled"`
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"clientId"`
	ClientSecret    string   `json:"clientSecret,omitempty"`
	Scopes          []string `json:"scopes"`
	AutoProvision   bool     `json:"autoProvision"`
	LinkByEmail     bool     `json:"linkExistingUsersByEmail"`
	AllowedDomains  []string `json:"allowedDomains"`
	AdminGroups     []string `json:"adminGroups"`
	HasClientSecret bool     `json:"hasClientSecret"`
	Configured      bool     `json:"configured"`
}

type AuditEvent struct {
	ID          int64     `json:"id"`
	ActorUserID string    `json:"actorUserId,omitempty"`
	Action      string    `json:"action"`
	TargetType  string    `json:"targetType"`
	TargetID    string    `json:"targetId,omitempty"`
	Details     string    `json:"details"`
	RemoteAddr  string    `json:"remoteAddr"`
	CreatedAt   time.Time `json:"createdAt"`
}
