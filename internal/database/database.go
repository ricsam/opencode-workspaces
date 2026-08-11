package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ricsam/opencode-workspaces/internal/model"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct {
	Pool *pgxpool.Pool
	aead cipher.AEAD
}

func Open(ctx context.Context, databaseURL string, encryptionKey []byte) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		pool.Close()
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool, aead: aead}, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(0x4f435753)); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(0x4f435753)) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var applied bool
		err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", entry.Name()).Scan(&applied)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(body)); err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1) ON CONFLICT DO NOTHING", entry.Name())
		}
		if err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			return fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Healthy(ctx context.Context) error { return s.Pool.Ping(ctx) }

func (s *Store) UserCount(ctx context.Context) (int, error) {
	var count int
	err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count)
	return count, err
}

func (s *Store) BootstrapAdmin(ctx context.Context, username, displayName, email, passwordHash string) (model.User, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return model.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "LOCK TABLE users IN EXCLUSIVE MODE"); err != nil {
		return model.User{}, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users)").Scan(&exists); err != nil {
		return model.User{}, err
	}
	if exists {
		return model.User{}, ErrAlreadyBootstrapped
	}
	user, err := insertUser(ctx, tx, username, displayName, email, "admin", passwordHash)
	if err != nil {
		return model.User{}, err
	}
	if err := audit(ctx, tx, user.ID, "bootstrap.admin", "user", user.ID, `{}`, ""); err != nil {
		return model.User{}, err
	}
	return user, tx.Commit(ctx)
}

var ErrAlreadyBootstrapped = errors.New("installation already bootstrapped")
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

func (s *Store) CreateLocalUser(ctx context.Context, actor, username, displayName, email, role, passwordHash, remote string) (model.User, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return model.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	user, err := insertUser(ctx, tx, username, displayName, email, role, passwordHash)
	if err != nil {
		return model.User{}, err
	}
	if err := audit(ctx, tx, actor, "user.create", "user", user.ID, `{}`, remote); err != nil {
		return model.User{}, err
	}
	return user, tx.Commit(ctx)
}

func insertUser(ctx context.Context, tx pgx.Tx, username, displayName, email, role, passwordHash string) (model.User, error) {
	id := uuid.NewString()
	resource := "ws-" + strings.ReplaceAll(id[:18], "-", "")
	var user model.User
	err := tx.QueryRow(ctx, `INSERT INTO users(id, username, display_name, email, role)
		VALUES($1,$2,$3,NULLIF($4,''),$5) RETURNING id,username,display_name,COALESCE(email,''),role,disabled,created_at`,
		id, username, displayName, email, role).Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Disabled, &user.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return model.User{}, ErrConflict
		}
		return model.User{}, err
	}
	if passwordHash != "" {
		if _, err := tx.Exec(ctx, "INSERT INTO local_credentials(user_id,password_hash) VALUES($1,$2)", id, passwordHash); err != nil {
			return model.User{}, err
		}
	}
	if _, err := tx.Exec(ctx, "INSERT INTO workspaces(user_id,resource_name) VALUES($1,$2)", id, resource); err != nil {
		return model.User{}, err
	}
	return user, nil
}

func (s *Store) AuthenticateLocal(ctx context.Context, username string) (model.User, string, error) {
	var user model.User
	var hash string
	err := s.Pool.QueryRow(ctx, `SELECT u.id,u.username,u.display_name,COALESCE(u.email,''),u.role,u.disabled,u.created_at,COALESCE(u.last_login_at,'epoch'),c.password_hash
		FROM users u JOIN local_credentials c ON c.user_id=u.id WHERE lower(u.username)=lower($1)`, username).
		Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Disabled, &user.CreatedAt, &user.LastLoginAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, "", ErrNotFound
	}
	return user, hash, err
}

func (s *Store) HasLocalCredential(ctx context.Context, userID string) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM local_credentials WHERE user_id=$1)", userID).Scan(&exists)
	return exists, err
}

func (s *Store) RecordLogin(ctx context.Context, userID string) {
	s.Pool.Exec(ctx, "UPDATE users SET last_login_at=now() WHERE id=$1", userID) //nolint:errcheck
}

func scanUser(row pgx.Row) (model.User, error) {
	var user model.User
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Disabled, &user.CreatedAt, &user.LastLoginAt)
	return user, err
}

var userColumns = userColumnsFor("")

func userColumnsFor(relation string) string {
	prefix := ""
	if relation != "" {
		prefix = relation + "."
	}
	return prefix + "id," + prefix + "username," + prefix + "display_name,COALESCE(" + prefix + "email,'')," +
		prefix + "role," + prefix + "disabled," + prefix + "created_at,COALESCE(" + prefix + "last_login_at,'epoch')"
}

func (s *Store) UserByID(ctx context.Context, id string) (model.User, error) {
	user, err := scanUser(s.Pool.QueryRow(ctx, "SELECT "+userColumns+" FROM users WHERE id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	return user, err
}

func (s *Store) ListUsers(ctx context.Context) ([]model.User, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+userColumns+" FROM users ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []model.User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

type ProvisionUserInput struct {
	Username    string
	DisplayName string
	Email       string
	Role        string
	Disabled    bool
}

// ProvisionUser creates or updates an externally managed account by normalized
// email. It intentionally does not create a local credential; OIDC can link the
// pre-provisioned account on first login without inventing a shared password.
func (s *Store) ProvisionUser(ctx context.Context, actor string, input ProvisionUserInput) (model.User, error) {
	var user model.User
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE lower(email)=lower($1)`, input.Email).
			Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Disabled, &user.CreatedAt, &user.LastLoginAt)
		if errors.Is(err, pgx.ErrNoRows) {
			user, err = insertUser(ctx, tx, input.Username, input.DisplayName, input.Email, input.Role, "")
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if user.ID == actor {
				input.Role = "admin"
				input.Disabled = false
			}
			err = tx.QueryRow(ctx, `UPDATE users SET username=$2,display_name=$3,role=$4,disabled=$5,updated_at=now() WHERE id=$1
				RETURNING `+userColumns, user.ID, input.Username, input.DisplayName, input.Role, input.Disabled).
				Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Disabled, &user.CreatedAt, &user.LastLoginAt)
			if err != nil {
				return err
			}
		}
		if input.Disabled && !user.Disabled {
			if err := tx.QueryRow(ctx, `UPDATE users SET disabled=true,updated_at=now() WHERE id=$1 RETURNING `+userColumns, user.ID).
				Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Disabled, &user.CreatedAt, &user.LastLoginAt); err != nil {
				return err
			}
		}
		return audit(ctx, tx, actor, "user.provision", "user", user.ID, `{}`, "")
	})
	return user, err
}

func (s *Store) UpdateUser(ctx context.Context, actor, id, displayName, email, role string, disabled bool, passwordHash, remote string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET display_name=$2,email=NULLIF($3,''),role=$4,disabled=$5,updated_at=now() WHERE id=$1`, id, displayName, email, role, disabled)
		if err != nil || tag.RowsAffected() == 0 {
			if err == nil {
				return ErrNotFound
			}
			return err
		}
		if passwordHash != "" {
			_, err = tx.Exec(ctx, `INSERT INTO local_credentials(user_id,password_hash) VALUES($1,$2)
				ON CONFLICT(user_id) DO UPDATE SET password_hash=EXCLUDED.password_hash,changed_at=now()`, id, passwordHash)
			if err != nil {
				return err
			}
		}
		return audit(ctx, tx, actor, "user.update", "user", id, `{}`, remote)
	})
}

func (s *Store) CreateSession(ctx context.Context, tokenHash, csrfHash []byte, userID, userAgent, remote string, expires time.Time) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO sessions(id_hash,user_id,csrf_hash,expires_at,user_agent,remote_addr)
		VALUES($1,$2,$3,$4,$5,$6)`, tokenHash, userID, csrfHash, expires, userAgent, remote)
	return err
}

func (s *Store) Session(ctx context.Context, tokenHash []byte) (model.User, []byte, time.Time, error) {
	var user model.User
	var csrf []byte
	var expires time.Time
	err := s.Pool.QueryRow(ctx, `SELECT u.id,u.username,u.display_name,COALESCE(u.email,''),u.role,u.disabled,u.created_at,COALESCE(u.last_login_at,'epoch'),s.csrf_hash,s.expires_at
		FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.id_hash=$1 AND s.expires_at>now()`, tokenHash).
		Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Disabled, &user.CreatedAt, &user.LastLoginAt, &csrf, &expires)
	if errors.Is(err, pgx.ErrNoRows) || user.Disabled {
		return model.User{}, nil, time.Time{}, ErrNotFound
	}
	if err == nil {
		s.Pool.Exec(ctx, "UPDATE sessions SET last_seen_at=now() WHERE id_hash=$1 AND last_seen_at<now()-interval '5 minutes'", tokenHash) //nolint:errcheck
	}
	return user, csrf, expires, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM sessions WHERE id_hash=$1", tokenHash)
	return err
}

func (s *Store) Workspace(ctx context.Context, userID string) (model.Workspace, error) {
	var w model.Workspace
	err := s.Pool.QueryRow(ctx, `SELECT w.user_id,w.resource_name,w.desired_state,w.restart_nonce,w.last_activity_at,w.created_at,w.updated_at,COALESCE(c.revision,0)
		FROM workspaces w LEFT JOIN workspace_configurations c ON c.user_id=w.user_id WHERE w.user_id=$1`, userID).
		Scan(&w.UserID, &w.ResourceName, &w.DesiredState, &w.RestartNonce, &w.LastActivityAt, &w.CreatedAt, &w.UpdatedAt, &w.ConfigRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]model.Workspace, error) {
	rows, err := s.Pool.Query(ctx, `SELECT w.user_id,w.resource_name,w.desired_state,w.restart_nonce,w.last_activity_at,w.created_at,w.updated_at,COALESCE(c.revision,0)
		FROM workspaces w LEFT JOIN workspace_configurations c ON c.user_id=w.user_id ORDER BY w.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.Workspace
	for rows.Next() {
		var w model.Workspace
		if err := rows.Scan(&w.UserID, &w.ResourceName, &w.DesiredState, &w.RestartNonce, &w.LastActivityAt, &w.CreatedAt, &w.UpdatedAt, &w.ConfigRevision); err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, rows.Err()
}

func (s *Store) SetWorkspaceConfiguration(ctx context.Context, actor, userID string, opencodeJSON []byte, apiKey string) error {
	var document map[string]any
	if len(opencodeJSON) == 0 || json.Unmarshal(opencodeJSON, &document) != nil || document == nil {
		return errors.New("OpenCode configuration must be a JSON object")
	}
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("workspace API key is required")
	}
	configCiphertext, err := s.encrypt(opencodeJSON)
	if err != nil {
		return err
	}
	keyCiphertext, err := s.encrypt([]byte(apiKey))
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_configurations(user_id,opencode_json_ciphertext,api_key_ciphertext)
			VALUES($1,$2,$3) ON CONFLICT(user_id) DO UPDATE SET opencode_json_ciphertext=EXCLUDED.opencode_json_ciphertext,
			api_key_ciphertext=EXCLUDED.api_key_ciphertext,revision=workspace_configurations.revision+1,updated_at=now()`, userID, configCiphertext, keyCiphertext); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "workspace.configuration.update", "workspace", userID, `{}`, "")
	})
}

func (s *Store) WorkspaceConfiguration(ctx context.Context, userID string) (model.WorkspaceConfiguration, error) {
	var result model.WorkspaceConfiguration
	var configCiphertext, keyCiphertext string
	err := s.Pool.QueryRow(ctx, `SELECT opencode_json_ciphertext,api_key_ciphertext,revision FROM workspace_configurations WHERE user_id=$1`, userID).
		Scan(&configCiphertext, &keyCiphertext, &result.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	result.OpenCodeJSON, err = s.decrypt(configCiphertext)
	if err != nil {
		return result, err
	}
	key, err := s.decrypt(keyCiphertext)
	if err != nil {
		return result, err
	}
	result.APIKey = string(key)
	return result, nil
}

func (s *Store) SetWorkspaceState(ctx context.Context, actor, userID, state, remote string, restart bool) error {
	action := "workspace." + state
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		query := "UPDATE workspaces SET desired_state=$2,last_activity_at=CASE WHEN $2='running' THEN now() ELSE last_activity_at END,updated_at=now() WHERE user_id=$1"
		if restart {
			query = "UPDATE workspaces SET desired_state='running',restart_nonce=restart_nonce+1,last_activity_at=now(),updated_at=now() WHERE user_id=$1"
			action = "workspace.restart"
		}
		var tag pgconn.CommandTag
		var err error
		if restart {
			tag, err = tx.Exec(ctx, query, userID)
		} else {
			tag, err = tx.Exec(ctx, query, userID, state)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, actor, action, "workspace", userID, `{}`, remote)
	})
}

func (s *Store) TouchWorkspace(ctx context.Context, userID string) {
	s.Pool.Exec(ctx, "UPDATE workspaces SET last_activity_at=now(),desired_state='running',updated_at=now() WHERE user_id=$1", userID) //nolint:errcheck
}

func (s *Store) IdleWorkspaces(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `UPDATE workspaces SET desired_state='stopped',updated_at=now()
		WHERE desired_state='running' AND last_activity_at<$1 RETURNING user_id`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) DeleteWorkspaceDataRecord(ctx context.Context, actor, userID, remote string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE workspaces SET desired_state='stopped',restart_nonce=restart_nonce+1,updated_at=now() WHERE user_id=$1", userID); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "workspace.purge", "workspace", userID, `{}`, remote)
	})
}

func (s *Store) Setting(ctx context.Context, key string, target any) (bool, error) {
	var data []byte
	var encrypted bool
	err := s.Pool.QueryRow(ctx, "SELECT value,encrypted FROM settings WHERE key=$1", key).Scan(&data, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if encrypted {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return false, err
		}
		data, err = s.decrypt(encoded)
		if err != nil {
			return false, err
		}
	}
	return true, json.Unmarshal(data, target)
}

func (s *Store) SetSetting(ctx context.Context, actor, key string, value any, encrypted bool, remote string) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if encrypted {
		encoded, err := s.encrypt(data)
		if err != nil {
			return err
		}
		data, _ = json.Marshal(encoded)
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO settings(key,value,encrypted,updated_by) VALUES($1,$2,$3,$4)
			ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,encrypted=EXCLUDED.encrypted,updated_by=EXCLUDED.updated_by,updated_at=now()`, key, data, encrypted, nullUUID(actor))
		if err != nil {
			return err
		}
		return audit(ctx, tx, actor, "setting.update", "setting", key, `{}`, remote)
	})
}

func (s *Store) AuditEvents(ctx context.Context, limit int) ([]model.AuditEvent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,COALESCE(actor_user_id::text,''),action,target_type,COALESCE(target_id,''),details::text,remote_addr,created_at FROM audit_events ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []model.AuditEvent
	for rows.Next() {
		var e model.AuditEvent
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.Action, &e.TargetType, &e.TargetID, &e.Details, &e.RemoteAddr, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) OIDCUser(ctx context.Context, issuer, subject string) (model.User, error) {
	user, err := scanUser(s.Pool.QueryRow(ctx, `SELECT `+userColumnsFor("u")+` FROM users u JOIN oidc_identities i ON i.user_id=u.id WHERE i.issuer=$1 AND i.subject=$2`, issuer, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return user, ErrNotFound
	}
	return user, err
}

func (s *Store) LinkOIDCUserByEmail(ctx context.Context, issuer, subject, email, remote string) (model.User, error) {
	var user model.User
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		user, err = scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE lower(email)=lower($1)`, strings.TrimSpace(email)))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if user.Disabled {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `INSERT INTO oidc_identities(issuer,subject,user_id,email) VALUES($1,$2,$3,$4)`, issuer, subject, user.ID, email); err != nil {
			if strings.Contains(err.Error(), "duplicate key") {
				return ErrConflict
			}
			return err
		}
		return audit(ctx, tx, user.ID, "oidc.identity.link", "user", user.ID, `{}`, remote)
	})
	return user, err
}

func (s *Store) ProvisionOIDCUser(ctx context.Context, issuer, subject, username, displayName, email, role, remote string) (model.User, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return model.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	user, err := insertUser(ctx, tx, username, displayName, email, role, "")
	if err != nil {
		return model.User{}, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO oidc_identities(issuer,subject,user_id,email) VALUES($1,$2,$3,NULLIF($4,''))", issuer, subject, user.ID, email); err != nil {
		return model.User{}, err
	}
	if err := audit(ctx, tx, user.ID, "oidc.provision", "user", user.ID, `{}`, remote); err != nil {
		return model.User{}, err
	}
	return user, tx.Commit(ctx)
}

func audit(ctx context.Context, tx pgx.Tx, actor, action, targetType, targetID, details, remote string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(actor_user_id,action,target_type,target_id,details,remote_addr) VALUES($1,$2,$3,NULLIF($4,''),$5,$6)`, nullUUID(actor), action, targetType, targetID, details, remote)
	return err
}

func nullUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func Hash(value string) []byte { sum := sha256.Sum256([]byte(value)); return sum[:] }

func (s *Store) encrypt(plain []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}
func (s *Store) decrypt(encoded string) ([]byte, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(data) < s.aead.NonceSize() {
		return nil, errors.New("encrypted setting is truncated")
	}
	nonce, ciphertext := data[:s.aead.NonceSize()], data[s.aead.NonceSize():]
	return s.aead.Open(nil, nonce, ciphertext, nil)
}
