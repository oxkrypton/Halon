package authz

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Mode controls whether static tokens, JWTs, or both are accepted.
type Mode string

const (
	ModeLegacy Mode = "legacy"
	ModeHybrid Mode = "hybrid"
	ModeJWT    Mode = "jwt"
)

const DefaultIssuer = "halon"

// Config is parsed once at process startup by both gateway and agent.
type Config struct {
	Mode        Mode
	PublicKeys  map[string]ed25519.PublicKey
	Issuer      string
	RevokedJTIs map[string]struct{}
}

func ParseMode(value string) (Mode, error) {
	if strings.TrimSpace(value) == "" {
		return ModeLegacy, nil
	}
	mode := Mode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case ModeLegacy, ModeHybrid, ModeJWT:
		return mode, nil
	default:
		return "", fmt.Errorf("AUTH_MODE must be legacy, hybrid, or jwt, got %q", value)
	}
}

// LoadConfig reads the AUTH_* environment variables shared by both services.
func LoadConfig() (Config, error) {
	return LoadConfigWithExtraPublicKeys(nil)
}

// LoadConfigWithExtraPublicKeys behaves like LoadConfig but merges extra
// keys into AUTH_JWT_PUBLIC_KEYS before validating. The gateway uses it to
// add the public half of its own login signing key without requiring the
// operator to duplicate it in configuration. A kid that already exists with
// a different key is rejected rather than silently shadowed.
func LoadConfigWithExtraPublicKeys(extra map[string]ed25519.PublicKey) (Config, error) {
	mode, err := ParseMode(os.Getenv("AUTH_MODE"))
	if err != nil {
		return Config{}, err
	}
	keys, err := ParsePublicKeys(os.Getenv("AUTH_JWT_PUBLIC_KEYS"))
	if err != nil {
		return Config{}, err
	}
	for kid, key := range extra {
		if existing, found := keys[kid]; found {
			if !bytes.Equal(existing, key) {
				return Config{}, fmt.Errorf("JWT kid %q already configured with a different public key", kid)
			}
			continue
		}
		keys[kid] = key
	}
	issuer := strings.TrimSpace(os.Getenv("AUTH_JWT_ISSUER"))
	if issuer == "" {
		issuer = DefaultIssuer
	}
	cfg := Config{
		Mode:        mode,
		PublicKeys:  keys,
		Issuer:      issuer,
		RevokedJTIs: ParseRevokedJTIs(os.Getenv("AUTH_REVOKED_JTIS")),
	}
	if cfg.Mode != ModeLegacy && len(cfg.PublicKeys) == 0 {
		return Config{}, errors.New("AUTH_JWT_PUBLIC_KEYS is required in hybrid and jwt modes")
	}
	// Validate verifier construction during config parsing so a bad key or
	// issuer fails at startup rather than on the first request.
	if _, err := cfg.Verifier(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Verifier returns nil in legacy mode, where no JWT is ever accepted.
func (c Config) Verifier() (*Verifier, error) {
	if c.Mode == ModeLegacy {
		return nil, nil
	}
	return NewVerifier(c.PublicKeys, c.Issuer, c.RevokedJTIs)
}

// ParsePublicKeys decodes a comma-separated kid=base64(public key) list.
func ParsePublicKeys(raw string) (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		kid, encoded, found := strings.Cut(item, "=")
		kid, encoded = strings.TrimSpace(kid), strings.TrimSpace(encoded)
		if !found || kid == "" || encoded == "" {
			return nil, fmt.Errorf("invalid AUTH_JWT_PUBLIC_KEYS entry %q: expected kid=base64", item)
		}
		decoded, err := decodeBase64(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode public key %q: %w", kid, err)
		}
		if len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("public key %q must be %d bytes, got %d", kid, ed25519.PublicKeySize, len(decoded))
		}
		if _, exists := keys[kid]; exists {
			return nil, fmt.Errorf("duplicate key id %q", kid)
		}
		keys[kid] = ed25519.PublicKey(decoded)
	}
	return keys, nil
}

func decodeBase64(value string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

// ParseRevokedJTIs builds the emergency single-token revocation set.
func ParseRevokedJTIs(raw string) map[string]struct{} {
	revoked := make(map[string]struct{})
	for _, jti := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(jti); trimmed != "" {
			revoked[trimmed] = struct{}{}
		}
	}
	return revoked
}
