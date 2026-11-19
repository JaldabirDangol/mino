// Package auth provides password hashing, admin sessions and login rate limiting.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters: OWASP's 12 MiB / 3 iterations profile keeps per-login memory modest
// for a small server while remaining a strong, memory-hard KDF.
const (
	argonMemory  = 12 * 1024
	argonTime    = 3
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// ErrMismatch is returned when a password does not match its hash.
var ErrMismatch = errors.New("password does not match")

// ValidatePassword enforces the password policy.
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < 10:
		return errors.New("must be at least 10 characters")
	case len(pw) > 256:
		return errors.New("must be at most 256 bytes")
	}
	return nil
}

// HashPassword returns a PHC-formatted Argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// CheckPassword verifies pw against a hash produced by HashPassword (with any parameters).
func CheckPassword(pw, hash string) error {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("unsupported hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return errors.New("unsupported argon2 version")
	}
	var mem, iter uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &iter, &threads); err != nil {
		return errors.New("invalid argon2 parameters")
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return errors.New("invalid salt")
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return errors.New("invalid hash")
	}
	got := argon2.IDKey([]byte(pw), salt, iter, mem, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// dummyHash is checked when a login names an unknown user, so response time does not
// reveal which email addresses exist.
// It is computed on first use to keep startup fast.
var dummyHash = sync.OnceValue(func() string {
	h, _ := HashPassword("timing-equalizer-password")
	return h
})

// CheckDummy spends the same effort as a real password check.
func CheckDummy(pw string) { _ = CheckPassword(pw, dummyHash()) }
