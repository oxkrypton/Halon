// halonctl is the offline Ed25519 key and JWT tool for Halon. It never needs
// access to the cluster and never writes credentials to disk unless the
// operator redirects its output.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/oxkrypton/Halon/internal/authz"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "halonctl:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: halonctl keygen | token issue | token inspect")
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:], stdout)
	case "token":
		if len(args) < 2 {
			return errors.New("usage: halonctl token issue|inspect")
		}
		switch args[1] {
		case "issue":
			return tokenIssue(args[2:], stdout)
		case "inspect":
			return tokenInspect(args[2:], stdout)
		default:
			return fmt.Errorf("unknown token command %q", args[1])
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type keygenOutput struct {
	Kid           string `json:"kid"`
	PrivateKey    string `json:"private_key"`
	PublicKey     string `json:"public_key"`
	PublicKeysEnv string `json:"public_keys_env"`
}

func keygen(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	kid := flags.String("kid", "", "optional key id; derived from the public key when empty")
	if err := flags.Parse(args); err != nil {
		return err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key pair: %w", err)
	}
	id := *kid
	if id == "" {
		id = authz.KeyID(public)
	}
	encodedPublic := authz.PublicKeyBase64(public)
	return encodeJSON(stdout, keygenOutput{
		Kid:           id,
		PrivateKey:    authz.PrivateKeyBase64(private),
		PublicKey:     encodedPublic,
		PublicKeysEnv: id + "=" + encodedPublic,
	})
}

func tokenIssue(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("token issue", flag.ContinueOnError)
	privateKey := flags.String("private-key", "", "base64-encoded Ed25519 private key")
	kid := flags.String("kid", "", "key id; derived from the private key when empty")
	subject := flags.String("sub", "", "subject (person or agent identity)")
	typ := flags.String("type", string(authz.TypeHuman), "human or agent")
	role := flags.String("role", string(authz.RoleReadOnly), "readonly, operator or full")
	audience := flags.String("aud", string(authz.AudienceGatewayAdmin), "comma-separated audiences: halon-gateway-admin, halon-agent")
	ttl := flags.Duration("ttl", 8*time.Hour, "token lifetime, for example 8h or 2160h")
	issuer := flags.String("issuer", authz.DefaultIssuer, "JWT issuer")
	jti := flags.String("jti", "", "optional explicit jti; random when empty")
	if err := flags.Parse(args); err != nil {
		return err
	}
	token, err := issueToken(issueRequest{
		PrivateKey: *privateKey,
		Kid:        *kid,
		Subject:    *subject,
		Type:       *typ,
		Role:       *role,
		Audience:   *audience,
		TTL:        *ttl,
		Issuer:     *issuer,
		JTI:        *jti,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, token)
	return err
}

type issueRequest struct {
	PrivateKey string
	Kid        string
	Subject    string
	Type       string
	Role       string
	Audience   string
	TTL        time.Duration
	Issuer     string
	JTI        string
}

func issueToken(request issueRequest) (string, error) {
	private, err := authz.ParsePrivateKeyBase64(request.PrivateKey)
	if err != nil {
		return "", err
	}
	principalType, err := authz.ParsePrincipalType(request.Type)
	if err != nil {
		return "", err
	}
	role, err := authz.ParseRole(request.Role)
	if err != nil {
		return "", err
	}
	audiences, err := parseAudiences(request.Audience)
	if err != nil {
		return "", err
	}
	kid := strings.TrimSpace(request.Kid)
	if kid == "" {
		kid = authz.KeyID(private.Public().(ed25519.PublicKey))
	}
	signer, err := authz.NewSigner(private, kid, request.Issuer)
	if err != nil {
		return "", err
	}
	return signer.Issue(authz.IssueOptions{
		Subject:   request.Subject,
		Type:      principalType,
		Role:      role,
		Audiences: audiences,
		TTL:       request.TTL,
		Issuer:    request.Issuer,
		JTI:       request.JTI,
	})
}

type inspectOutput struct {
	Verified bool           `json:"verified"`
	Claims   map[string]any `json:"claims"`
}

func tokenInspect(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("token inspect", flag.ContinueOnError)
	token := flags.String("token", "", "JWT to inspect")
	publicKeys := flags.String("public-keys", "", "optional kid=base64 public key list; enables signature verification")
	audience := flags.String("aud", string(authz.AudienceGatewayAdmin), "comma-separated expected audiences when verifying")
	issuer := flags.String("issuer", authz.DefaultIssuer, "expected issuer when verifying")
	if err := flags.Parse(args); err != nil {
		return err
	}
	result, err := inspectToken(*token, *publicKeys, *audience, *issuer)
	if err != nil {
		return err
	}
	return encodeJSON(stdout, result)
}

func inspectToken(raw, publicKeys, audience, issuer string) (inspectOutput, error) {
	if strings.TrimSpace(raw) == "" {
		return inspectOutput{}, errors.New("--token is required")
	}
	if strings.TrimSpace(publicKeys) == "" {
		claims := jwt.MapClaims{}
		parser := jwt.NewParser(jwt.WithoutClaimsValidation())
		if _, _, err := parser.ParseUnverified(raw, claims); err != nil {
			return inspectOutput{}, fmt.Errorf("parse JWT: %w", err)
		}
		return inspectOutput{Verified: false, Claims: claims}, nil
	}
	keys, err := authz.ParsePublicKeys(publicKeys)
	if err != nil {
		return inspectOutput{}, err
	}
	verifier, err := authz.NewVerifier(keys, issuer, nil)
	if err != nil {
		return inspectOutput{}, err
	}
	audiences, err := parseAudiences(audience)
	if err != nil {
		return inspectOutput{}, err
	}
	principal, err := verifier.Verify(raw, audiences...)
	if err != nil {
		return inspectOutput{}, err
	}
	claims := map[string]any{
		"sub":  principal.Subject,
		"typ":  string(principal.Type),
		"role": string(principal.Role),
		"aud":  string(principal.Audience),
		"iss":  principal.Issuer,
		"jti":  principal.JTI,
	}
	if principal.UserID != "" {
		claims["uid"] = principal.UserID
	}
	if principal.PasswordChangeRequired {
		claims["pcr"] = true
	}
	if len(principal.Audiences) > 1 {
		audStrings := make([]string, 0, len(principal.Audiences))
		for _, aud := range principal.Audiences {
			audStrings = append(audStrings, string(aud))
		}
		claims["audiences"] = audStrings
	}
	return inspectOutput{
		Verified: true,
		Claims:   claims,
	}, nil
}

func parseAudiences(raw string) ([]authz.Audience, error) {
	var audiences []authz.Audience
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			audiences = append(audiences, authz.Audience(trimmed))
		}
	}
	if len(audiences) == 0 {
		return nil, errors.New("at least one audience is required")
	}
	return audiences, nil
}

func encodeJSON(stdout io.Writer, value any) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
