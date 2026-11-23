package authz

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the JWT payload shared by the CLI, gateway and agent.
type Claims struct {
	jwt.RegisteredClaims
	Type PrincipalType `json:"typ"`
	Role Role          `json:"role"`
}

// Verifier verifies EdDSA/JWTs against a set of public keys.
type Verifier struct {
	keys    map[string]ed25519.PublicKey
	issuer  string
	revoked map[string]struct{}
}

func NewVerifier(keys map[string]ed25519.PublicKey, issuer string, revoked map[string]struct{}) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one JWT public key is required")
	}
	if strings.TrimSpace(issuer) == "" {
		return nil, errors.New("JWT issuer is required")
	}
	for kid, key := range keys {
		if kid == "" {
			return nil, errors.New("JWT key id must not be empty")
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("JWT public key %q has invalid size %d", kid, len(key))
		}
	}
	if revoked == nil {
		revoked = make(map[string]struct{})
	}
	return &Verifier{keys: keys, issuer: issuer, revoked: revoked}, nil
}

// Verify validates signature, issuer, audience, expiry, role/type claims and
// the emergency revocation list. Only EdDSA is accepted.
func (v *Verifier) Verify(raw string, audience Audience) (*Principal, error) {
	if v == nil {
		return nil, errors.New("JWT verifier is not configured")
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodEdDSA.Alg() {
			return nil, fmt.Errorf("unsupported JWT algorithm %q", token.Method.Alg())
		}
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("JWT kid header is required")
		}
		key, found := v.keys[kid]
		if !found {
			return nil, fmt.Errorf("unknown JWT kid %q", kid)
		}
		return key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("verify JWT: %w", err)
	}
	if !token.Valid {
		return nil, errors.New("verify JWT: invalid token")
	}
	if claims.ExpiresAt == nil {
		return nil, errors.New("verify JWT: exp is required")
	}
	if claims.Subject == "" {
		return nil, errors.New("verify JWT: sub is required")
	}
	if claims.ID == "" {
		return nil, errors.New("verify JWT: jti is required")
	}
	if _, revoked := v.revoked[claims.ID]; revoked {
		return nil, errors.New("verify JWT: jti is revoked")
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != string(audience) {
		return nil, fmt.Errorf("verify JWT: audience must be exactly %q", audience)
	}
	typ, err := ParsePrincipalType(string(claims.Type))
	if err != nil {
		return nil, fmt.Errorf("verify JWT: %w", err)
	}
	role, err := ParseRole(string(claims.Role))
	if err != nil {
		return nil, fmt.Errorf("verify JWT: %w", err)
	}
	return &Principal{
		Subject:  claims.Subject,
		Type:     typ,
		Role:     role,
		Audience: audience,
		Issuer:   claims.Issuer,
		JTI:      claims.ID,
	}, nil
}

// LooksLikeJWT distinguishes a signed JWT from an opaque static token. It
// only reports true when the compact serialization can actually be parsed;
// a string that merely contains two dots is treated as a static token.
func LooksLikeJWT(raw string) bool {
	_, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	return err == nil
}

func ParsePrincipalType(value string) (PrincipalType, error) {
	typ := PrincipalType(strings.ToLower(strings.TrimSpace(value)))
	switch typ {
	case TypeHuman, TypeAgent:
		return typ, nil
	default:
		return "", fmt.Errorf("unsupported principal type %q", value)
	}
}

// IssueOptions describes one token to sign.
type IssueOptions struct {
	Subject  string
	Type     PrincipalType
	Role     Role
	Audience Audience
	TTL      time.Duration
	Issuer   string
	JTI      string
}

// Signer holds the signing key. Only the offline CLI should ever hold it.
type Signer struct {
	privateKey ed25519.PrivateKey
	kid        string
	issuer     string
}

func NewSigner(privateKey ed25519.PrivateKey, kid, issuer string) (*Signer, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(privateKey))
	}
	if strings.TrimSpace(kid) == "" {
		return nil, errors.New("kid is required")
	}
	if strings.TrimSpace(issuer) == "" {
		issuer = DefaultIssuer
	}
	return &Signer{privateKey: privateKey, kid: kid, issuer: issuer}, nil
}

func (s *Signer) Issue(options IssueOptions) (string, error) {
	if options.Subject == "" {
		return "", errors.New("subject is required")
	}
	if _, err := ParsePrincipalType(string(options.Type)); err != nil {
		return "", err
	}
	if _, err := ParseRole(string(options.Role)); err != nil {
		return "", err
	}
	if options.Audience != AudienceGatewayAdmin && options.Audience != AudienceAgent {
		return "", fmt.Errorf("unsupported audience %q", options.Audience)
	}
	if options.TTL <= 0 {
		return "", errors.New("ttl must be positive")
	}
	jti := options.JTI
	if jti == "" {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("generate jti: %w", err)
		}
		jti = hex.EncodeToString(random)
	}
	issuer := options.Issuer
	if issuer == "" {
		issuer = s.issuer
	}
	now := time.Now()
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   options.Subject,
			Audience:  jwt.ClaimStrings{string(options.Audience)},
			ExpiresAt: jwt.NewNumericDate(now.Add(options.TTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jti,
		},
		Type: options.Type,
		Role: options.Role,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = s.kid
	signed, err := token.SignedString(s.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return signed, nil
}

// KeyID derives a stable kid from a public key.
func KeyID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:8])
}

func PublicKeyBase64(publicKey ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(publicKey)
}

func PrivateKeyBase64(privateKey ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(privateKey)
}

func ParsePrivateKeyBase64(encoded string) (ed25519.PrivateKey, error) {
	decoded, err := decodeBase64(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(decoded))
	}
	return ed25519.PrivateKey(decoded), nil
}
