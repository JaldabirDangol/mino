package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mino/internal/testutil"
)

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=12288,t=3,p=1$") {
		t.Fatalf("unexpected format: %s", h)
	}
	if err := CheckPassword("correct horse battery", h); err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
	if err := CheckPassword("wrong", h); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong password: %v", err)
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("hashes must be salted")
	}
	if err := CheckPassword("x", "$2a$10$bcrypt"); err == nil {
		t.Fatal("foreign hash format accepted")
	}
}

func TestValidatePassword(t *testing.T) {
	if ValidatePassword("short") == nil || ValidatePassword(strings.Repeat("a", 300)) == nil {
		t.Fatal("policy not enforced")
	}
	if err := ValidatePassword("long enough pw"); err != nil {
		t.Fatal(err)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("1st")
	}
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("2nd")
	}
	if ok, retry := l.Allow("ip"); ok || retry <= 0 {
		t.Fatal("3rd should be limited")
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("keys are independent")
	}
	now = now.Add(61 * time.Second)
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("window should reset")
	}
	l.Reset("ip")
	l.Allow("ip")
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("reset should clear the count")
	}
}

func TestSessions(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO admin_users (email, name, password_hash) VALUES ('a@b.co', 'A', 'x') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	s := NewSessions(pool, time.Hour, 24*time.Hour)
	now := time.Now()
	s.now = func() time.Time { return now }

	token, sess, err := s.Create(ctx, uid, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Lookup(ctx, token)
	if err != nil || got.UserID != uid || got.CSRFToken != sess.CSRFToken {
		t.Fatalf("lookup: %v %+v", err, got)
	}
	var stored int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash = $1`, []byte(token)).Scan(&stored)
	if stored != 0 {
		t.Fatal("raw token must not be stored")
	}
	if _, err := s.Lookup(ctx, token+"x"); !errors.Is(err, ErrNoSession) {
		t.Fatal("tampered token accepted")
	}

	// Absolute lifetime is enforced even with activity.
	now = now.Add(25 * time.Hour)
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session past max age: %v", err)
	}

	// Deactivated users lose access immediately.
	now = time.Now()
	token, _, _ = s.Create(ctx, uid, "", "")
	_, _ = pool.Exec(ctx, `UPDATE admin_users SET active = false WHERE id = $1`, uid)
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatal("inactive user's session accepted")
	}
	_, _ = pool.Exec(ctx, `UPDATE admin_users SET active = true WHERE id = $1`, uid)

	keep, _, _ := s.Create(ctx, uid, "", "")
	if err := s.DeleteForUser(ctx, uid, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatal("other sessions should be revoked")
	}
	if _, err := s.Lookup(ctx, keep); err != nil {
		t.Fatal("kept session revoked")
	}
	if err := s.Delete(ctx, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, keep); !errors.Is(err, ErrNoSession) {
		t.Fatal("logout did not end session")
	}
}

func TestIdleExpiry(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var uid int64
	_ = pool.QueryRow(ctx, `INSERT INTO admin_users (email, name, password_hash) VALUES ('a@b.co', 'A', 'x') RETURNING id`).Scan(&uid)
	s := NewSessions(pool, time.Hour, 24*time.Hour)
	token, _, _ := s.Create(ctx, uid, "", "")
	// Expiry is evaluated by the database clock; simulate an idle session.
	_, _ = pool.Exec(ctx, `UPDATE sessions SET expires_at = now() - interval '1 second'`)
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatal("idle-expired session accepted")
	}
}
