package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Token scopes. A read token may call the documented read endpoints and MCP
// tools; a write token may also queue, cancel, requeue, and change settings.
const (
	TokenScopeRead  = "read"
	TokenScopeWrite = "write"
)

// tokenPrefix keeps generated credentials recognizable in env files and shell
// histories without leaking anything about the secret itself.
const tokenPrefix = "mmt_"

// ErrTokenRevoked reports that a token existed but has been revoked.
var ErrTokenRevoked = errors.New("token is revoked")

// APIToken is one bearer credential. The plaintext secret is returned exactly
// once at creation and only the SHA-256 hash is stored.
type APIToken struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// TokenIdentity is the authenticated principal for one request.
type TokenIdentity struct {
	ID   string
	Name string
}

// CreateToken mints a bearer token with the requested scope and returns the
// plaintext secret once. Only its hash is persisted.
func (s *Store) CreateToken(ctx context.Context, name, scope string) (APIToken, string, error) {
	if scope != TokenScopeRead && scope != TokenScopeWrite {
		return APIToken{}, "", fmt.Errorf("scope must be %q or %q", TokenScopeRead, TokenScopeWrite)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return APIToken{}, "", errors.New("token name is required")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return APIToken{}, "", fmt.Errorf("generate token: %w", err)
	}
	plaintext := tokenPrefix + hex.EncodeToString(secret)
	id, err := randomID("token", 12)
	if err != nil {
		return APIToken{}, "", err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens(id,name,token_hash,scope,created_at) VALUES(?,?,?,?,?)`,
		id, name, hashToken(plaintext), scope, now); err != nil {
		return APIToken{}, "", fmt.Errorf("store token: %w", err)
	}
	token := APIToken{ID: id, Name: name, Scope: scope}
	if token.CreatedAt, err = time.Parse(time.RFC3339Nano, now); err != nil {
		token.CreatedAt = s.now().UTC()
	}
	return token, plaintext, nil
}

// RevokeToken marks a token revoked. Revoked tokens stay listed with their
// revoked timestamp so the audit trail keeps naming them.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`,
		s.now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if changed, err := result.RowsAffected(); err == nil && changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListTokens returns every token, most recent first, including revoked ones.
func (s *Store) ListTokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,scope,created_at,COALESCE(revoked_at,'') FROM api_tokens ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	tokens := []APIToken{}
	for rows.Next() {
		var token APIToken
		var created, revoked string
		if err := rows.Scan(&token.ID, &token.Name, &token.Scope, &created, &revoked); err != nil {
			return nil, fmt.Errorf("list tokens: %w", err)
		}
		if token.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("list tokens: %w", err)
		}
		if revoked != "" {
			if revokedAt, err := time.Parse(time.RFC3339Nano, revoked); err == nil {
				token.RevokedAt = &revokedAt
			}
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// AuthenticateToken resolves a plaintext bearer token to its stored identity
// and scope. Unknown, revoked, and malformed tokens are all rejected, and the
// comparison runs against a hash so the database never holds usable secrets.
func (s *Store) AuthenticateToken(ctx context.Context, plaintext string) (TokenIdentity, string, bool) {
	if !strings.HasPrefix(plaintext, tokenPrefix) {
		return TokenIdentity{}, "", false
	}
	hash := hashToken(plaintext)
	var identity TokenIdentity
	var scope, revoked string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,name,scope,COALESCE(revoked_at,'') FROM api_tokens WHERE token_hash=?`, hash).
		Scan(&identity.ID, &identity.Name, &scope, &revoked)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return TokenIdentity{}, "", false
	}
	if revoked != "" {
		return TokenIdentity{}, "", false
	}
	return identity, scope, true
}

// RecordAudit writes one audit entry naming the token behind a change.
// Audit writes are best effort for advisory callers: a failed write is logged
// by the scheduler error hook, never fails the change itself. Skills changes
// are the exception — they report a failed write to the caller so a change
// never silently succeeds without its audit entry.
func (s *Store) RecordAudit(ctx context.Context, identity TokenIdentity, action, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log(at,token_id,token_name,action,detail) VALUES(?,?,?,?,?)`,
		s.now().UTC().Format(time.RFC3339Nano), identity.ID, identity.Name, action, detail)
	return err
}

// AuditLog returns recent audit entries, newest first, capped at limit.
func (s *Store) AuditLog(ctx context.Context, limit int) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at,token_id,token_name,action,detail FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	defer rows.Close()
	entries := []map[string]any{}
	for rows.Next() {
		var at, tokenID, tokenName, action, detail string
		if err := rows.Scan(&at, &tokenID, &tokenName, &action, &detail); err != nil {
			return nil, fmt.Errorf("read audit log: %w", err)
		}
		entries = append(entries, map[string]any{
			"at": at, "token_id": tokenID, "token_name": tokenName, "action": action, "detail": detail,
		})
	}
	return entries, rows.Err()
}

// hashToken derives the stored digest for a plaintext token.
func hashToken(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(digest[:])
}

// constantTimeEquals keeps the existing worker-token comparison in one place.
func constantTimeEquals(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
