package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoSession is returned for missing, expired or revoked sessions.
var ErrNoSession = errors.New("no valid session")

// Session is an authenticated admin session.
type Session struct {
	UserID    int64
	CSRFToken string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Sessions stores admin sessions. The cookie carries a random token; the database stores
// only its SHA-256, so a leaked database cannot be replayed as cookies.
type Sessions struct {
	pool   *pgxpool.Pool
	idle   time.Duration // extended on activity
	maxAge time.Duration // absolute lifetime
	now    func() time.Time
}

// NewSessions creates a session store.
func NewSessions(pool *pgxpool.Pool, idle, maxAge time.Duration) *Sessions {
	return &Sessions{pool: pool, idle: idle, maxAge: maxAge, now: time.Now}
}

// RandomToken returns a URL-safe random string with n bytes of entropy.
func RandomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken returns the stored form of a secret token.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Create starts a session and returns the cookie token. Expired sessions are pruned here,
// which avoids a background cleanup worker.
func (s *Sessions) Create(ctx context.Context, userID int64, ip, userAgent string) (string, *Session, error) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		return "", nil, err
	}
	token := RandomToken(32)
	now := s.now()
	sess := &Session{UserID: userID, CSRFToken: RandomToken(32), CreatedAt: now, ExpiresAt: now.Add(s.idle)}
	if len(userAgent) > 400 {
		userAgent = userAgent[:400]
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at, ip, user_agent)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7)`, HashToken(token), userID, sess.CSRFToken, now, sess.ExpiresAt, ip, userAgent)
	if err != nil {
		return "", nil, err
	}
	return token, sess, nil
}

// Lookup validates a cookie token and slides the idle expiry (at most once a minute).
// Sessions of inactive users are rejected.
func (s *Sessions) Lookup(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	hash := HashToken(token)
	sess := &Session{}
	var lastSeen time.Time
	err := s.pool.QueryRow(ctx, `SELECT s.user_id, s.csrf_token, s.expires_at, s.created_at, s.last_seen_at
		FROM sessions s JOIN admin_users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND u.active`, hash).
		Scan(&sess.UserID, &sess.CSRFToken, &sess.ExpiresAt, &sess.CreatedAt, &lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if now.Sub(sess.CreatedAt) > s.maxAge {
		_ = s.Delete(ctx, token)
		return nil, ErrNoSession
	}
	if now.Sub(lastSeen) > time.Minute {
		exp := now.Add(s.idle)
		if hard := sess.CreatedAt.Add(s.maxAge); exp.After(hard) {
			exp = hard
		}
		sess.ExpiresAt = exp
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2, expires_at = $3 WHERE token_hash = $1`, hash, now, exp); err != nil {
			return nil, err
		}
	}
	return sess, nil
}

// Delete ends one session.
func (s *Sessions) Delete(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, HashToken(token))
	return err
}

// DeleteForUser ends every session of a user, optionally keeping one (the current) token.
func (s *Sessions) DeleteForUser(ctx context.Context, userID int64, keepToken string) error {
	keep := []byte{}
	if keepToken != "" {
		keep = HashToken(keepToken)
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2`, userID, keep)
	return err
}
