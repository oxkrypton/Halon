// Package identity implements local username/password authentication, user
// storage and refresh-token rotation for the gateway administration surface.
package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. m=64 MiB, t=3, p=2 with a 16-byte salt and a 32-byte
// output. The values are stored as part of every PHC string so old hashes can
// still be verified and upgraded after a parameter change.
const (
	argonTime       = 3
	argonMemory     = 64 * 1024 // KiB
	argonThreads    = 2
	argonSaltLength = 16
	argonKeyLength  = 32

	// maxHashConcurrency bounds in-flight argon2 computations. Each one uses
	// up to 64 MiB, so unbounded parallel logins could exhaust the process.
	maxHashConcurrency = 4

	// Sanity ceilings applied to params read from a stored PHC string. A
	// corrupt or malicious row must not be able to request an enormous hash.
	maxStoredArgonMemory = 512 * 1024 // KiB
	maxStoredArgonTime   = 100
	maxStoredArgonKey    = 1024

	// DefaultMinPasswordLength is the policy default; MinPasswordLength and
	// MaxPasswordLength bound the configurable lower limit.
	DefaultMinPasswordLength = 12
	MinPasswordLength        = 8
	MaxPasswordLength        = 128

	// A login attempt is verified against the stored hash, so it is not
	// subject to the creation policy. This hard cap only prevents a caller
	// from sending an arbitrarily large input into argon2.
	maxVerifyPasswordLength = 4096
)

var (
	ErrPasswordTooShort        = errors.New("password is shorter than the configured minimum")
	ErrPasswordTooLong         = errors.New("password exceeds 128 bytes")
	ErrPasswordMatchesUsername = errors.New("password must differ from username")
	ErrInvalidPasswordPolicy   = errors.New("invalid password policy configuration")
	ErrInvalidPasswordHash     = errors.New("invalid argon2id password hash")
)

// PasswordPolicy is the creation/reset policy. Only length is required; no
// character-class rules are imposed.
type PasswordPolicy struct {
	MinLength int
}

// DefaultPasswordPolicy returns the production policy: at least 12 bytes.
func DefaultPasswordPolicy() PasswordPolicy {
	return PasswordPolicy{MinLength: DefaultMinPasswordLength}
}

// Validate rejects a password that violates the policy. The configured lower
// limit itself must be within 8..128 bytes.
func (p PasswordPolicy) Validate(username, password string) error {
	if p.MinLength < MinPasswordLength || p.MinLength > MaxPasswordLength {
		return fmt.Errorf("%w: minimum %d outside %d..%d", ErrInvalidPasswordPolicy, p.MinLength, MinPasswordLength, MaxPasswordLength)
	}
	if len(password) < p.MinLength {
		return fmt.Errorf("%w: got %d bytes, need %d", ErrPasswordTooShort, len(password), p.MinLength)
	}
	if len(password) > MaxPasswordLength {
		return fmt.Errorf("%w: got %d bytes", ErrPasswordTooLong, len(password))
	}
	if username != "" && password == username {
		return ErrPasswordMatchesUsername
	}
	return nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
	keyLen  uint32
}

func currentParams() argonParams {
	return argonParams{
		memory:  argonMemory,
		time:    argonTime,
		threads: argonThreads,
		keyLen:  argonKeyLength,
	}
}

var hashSemaphore = make(chan struct{}, maxHashConcurrency)

// deriveKey is the only path into argon2.IDKey; both hashing and verification
// pass through the same semaphore.
func deriveKey(password, salt []byte, params argonParams) []byte {
	hashSemaphore <- struct{}{}
	defer func() { <-hashSemaphore }()
	return argon2.IDKey(password, salt, params.time, params.memory, params.threads, params.keyLen)
}

func encodeHash(params argonParams, salt, key []byte) string {
	encoding := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, params.memory, params.time, params.threads,
		encoding.EncodeToString(salt), encoding.EncodeToString(key))
}

func parseArgonParams(value string) (argonParams, error) {
	fields := strings.Split(value, ",")
	if len(fields) != 3 {
		return argonParams{}, ErrInvalidPasswordHash
	}
	values := [3]uint64{}
	for i, prefix := range [3]string{"m=", "t=", "p="} {
		if !strings.HasPrefix(fields[i], prefix) {
			return argonParams{}, ErrInvalidPasswordHash
		}
		parsed, err := strconv.ParseUint(strings.TrimPrefix(fields[i], prefix), 10, 32)
		if err != nil || parsed == 0 {
			return argonParams{}, ErrInvalidPasswordHash
		}
		values[i] = parsed
	}
	if values[0] > maxStoredArgonMemory || values[1] > maxStoredArgonTime || values[2] > 255 {
		return argonParams{}, ErrInvalidPasswordHash
	}
	return argonParams{
		// #nosec G115 -- ParseUint used a 32-bit size and the ceilings above bound these values.
		memory: uint32(values[0]),
		// #nosec G115 -- time is bounded to maxStoredArgonTime above.
		time: uint32(values[1]),
		// #nosec G115 -- threads is bounded to 255 above.
		threads: uint8(values[2]),
	}, nil
}

func parseHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" ||
		parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return argonParams{}, nil, nil, ErrInvalidPasswordHash
	}
	params, err := parseArgonParams(parts[3])
	if err != nil {
		return argonParams{}, nil, nil, err
	}
	encoding := base64.RawStdEncoding
	salt, err := encoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return argonParams{}, nil, nil, ErrInvalidPasswordHash
	}
	key, err := encoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 || len(key) > maxStoredArgonKey {
		return argonParams{}, nil, nil, ErrInvalidPasswordHash
	}
	// #nosec G115 -- key length is bounded to maxStoredArgonKey above.
	params.keyLen = uint32(len(key))
	return params, salt, key, nil
}

// HashPassword returns a PHC-encoded argon2id hash of password. It does not
// enforce the creation policy: HashPassword is also used to rehash existing
// passwords that may predate the current policy.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	params := currentParams()
	return encodeHash(params, salt, deriveKey([]byte(password), salt, params)), nil
}

// VerifyPassword checks password against a PHC-encoded hash using a
// constant-time comparison.
func VerifyPassword(encoded, password string) (bool, error) {
	if len(password) > maxVerifyPasswordLength {
		return false, fmt.Errorf("%w: verification input exceeds %d bytes", ErrPasswordTooLong, maxVerifyPasswordLength)
	}
	params, salt, want, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	got := deriveKey([]byte(password), salt, params)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether encoded was produced with older argon2id
// parameters. Callers rehash after a successful login.
func NeedsRehash(encoded string) (bool, error) {
	params, _, _, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	return params != currentParams(), nil
}

// RehashPassword recomputes a hash with the current parameters. It is called
// after a successful login when NeedsRehash reports an outdated hash.
func RehashPassword(password string) (string, error) {
	return HashPassword(password)
}

// fakePasswordHash is a real argon2id hash with a fixed, non-secret salt. A
// login attempt for a nonexistent user is verified against it so the timing
// does not reveal whether the username exists.
var fakePasswordHash = func() string {
	salt := make([]byte, argonSaltLength)
	for i := range salt {
		salt[i] = byte(i*7 + 1)
	}
	params := currentParams()
	return encodeHash(params, salt, deriveKey([]byte("halon-timing-equalization-placeholder"), salt, params))
}()

// FakePasswordHash returns the placeholder hash used for nonexistent users.
// It is never stored and contains no real credential.
func FakePasswordHash() string { return fakePasswordHash }
