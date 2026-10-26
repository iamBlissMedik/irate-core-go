package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: OWASP's recommended baseline (19 MiB, 2 passes, 1
// lane). Raising them later is safe: NeedsRehash upgrades hashes on login.
type argonParams struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
	saltLen     int
	keyLen      uint32
}

var defaultParams = argonParams{memoryKiB: 19 * 1024, iterations: 2, parallelism: 1, saltLen: 16, keyLen: 32}

var errMalformedHash = errors.New("malformed password hash")

// Hasher hashes and verifies passwords with argon2id.
//
// argon2id is deliberately memory-hard, so each hash costs ~19 MiB of RAM. A
// semaphore caps how many run at once; without it a burst of login requests
// could exhaust the server's memory (a cheap denial-of-service).
type Hasher struct {
	params argonParams
	sem    chan struct{}
	// dummy is verified against when a login names an unknown email, so
	// "no such user" takes as long as "wrong password" (no timing oracle for
	// account enumeration).
	dummy string
}

func NewHasher(maxConcurrent int) *Hasher {
	h := &Hasher{params: defaultParams, sem: make(chan struct{}, maxConcurrent)}
	dummy, err := h.hash("dummy-password-for-timing-equalisation")
	if err != nil {
		panic(err) // crypto/rand failure at startup
	}
	h.dummy = dummy
	return h
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns a PHC-formatted argon2id hash:
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return h.hash(password)
}

func (h *Hasher) hash(password string) (string, error) {
	p := h.params
	salt := make([]byte, p.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.iterations, p.memoryKiB, p.parallelism, p.keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memoryKiB, p.iterations, p.parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches encoded, in constant time.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()

	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	other := argon2.IDKey([]byte(password), salt, p.iterations, p.memoryKiB, p.parallelism, uint32(len(key)))
	return subtle.ConstantTimeCompare(key, other) == 1, nil
}

// VerifyDummy burns the same time as a real Verify. Used for unknown emails.
func (h *Hasher) VerifyDummy(ctx context.Context, password string) {
	_, _ = h.Verify(ctx, password, h.dummy)
}

// NeedsRehash reports whether encoded was produced with outdated parameters.
func (h *Hasher) NeedsRehash(encoded string) bool {
	p, _, _, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return p.memoryKiB != h.params.memoryKiB || p.iterations != h.params.iterations || p.parallelism != h.params.parallelism
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, errMalformedHash
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.iterations, &p.parallelism); err != nil {
		return argonParams{}, nil, nil, errMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, errMalformedHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return argonParams{}, nil, nil, errMalformedHash
	}
	return p, salt, key, nil
}
