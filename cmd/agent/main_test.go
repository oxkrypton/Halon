package main

import (
	"crypto/ed25519"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/agent"
	"github.com/oxkrypton/Halon/internal/authz"
)

const (
	configuredGatewayURL = "http://gateway-admin:9090"
	staticGatewayToken   = "static-admin-token"
)

func issueGatewayToken(t *testing.T, signer *authz.Signer, typ authz.PrincipalType, role authz.Role) string {
	t.Helper()
	token, err := signer.Issue(authz.IssueOptions{
		Subject:  "agent-1",
		Type:     typ,
		Role:     role,
		Audience: authz.AudienceGatewayAdmin,
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func gatewayToken(t *testing.T, signer *authz.Signer, role authz.Role) string {
	return issueGatewayToken(t, signer, authz.TypeAgent, role)
}

type credentialFixture struct {
	config authz.Config
	signer *authz.Signer
	logger *slog.Logger
}

func newCredentialFixture(t *testing.T) credentialFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid := authz.KeyID(public)
	signer, err := authz.NewSigner(private, kid, authz.DefaultIssuer)
	if err != nil {
		t.Fatal(err)
	}
	return credentialFixture{
		config: authz.Config{
			PublicKeys:  map[string]ed25519.PublicKey{kid: public},
			Issuer:      authz.DefaultIssuer,
			RevokedJTIs: map[string]struct{}{},
		},
		signer: signer,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestCheckGatewayCredential covers all three AUTH_MODEs crossed with all
// three operation levels, both with and without a token, while a gateway
// admin URL is configured. This is the startup self-check matrix for F2.
func TestCheckGatewayCredential(t *testing.T) {
	t.Parallel()
	fixture := newCredentialFixture(t)
	tokens := map[agent.OperationLevel]string{
		agent.LevelReadOnly: gatewayToken(t, fixture.signer, authz.RoleReadOnly),
		agent.LevelOperator: gatewayToken(t, fixture.signer, authz.RoleOperator),
		agent.LevelFull:     gatewayToken(t, fixture.signer, authz.RoleFull),
	}

	cases := []struct {
		name  string
		mode  authz.Mode
		token string
		level agent.OperationLevel
		want  bool
	}{
		{"jwt/readonly/no-token", authz.ModeJWT, "", agent.LevelReadOnly, false},
		{"jwt/operator/no-token", authz.ModeJWT, "", agent.LevelOperator, true},
		{"jwt/full/no-token", authz.ModeJWT, "", agent.LevelFull, true},
		{"jwt/readonly/token", authz.ModeJWT, tokens[agent.LevelReadOnly], agent.LevelReadOnly, false},
		{"jwt/operator/token", authz.ModeJWT, tokens[agent.LevelOperator], agent.LevelOperator, false},
		{"jwt/full/token", authz.ModeJWT, tokens[agent.LevelFull], agent.LevelFull, false},
		{"jwt/readonly/operator-token", authz.ModeJWT, tokens[agent.LevelOperator], agent.LevelReadOnly, true},

		{"hybrid/readonly/no-token", authz.ModeHybrid, "", agent.LevelReadOnly, false},
		{"hybrid/operator/no-token", authz.ModeHybrid, "", agent.LevelOperator, true},
		{"hybrid/full/no-token", authz.ModeHybrid, "", agent.LevelFull, true},
		{"hybrid/readonly/static", authz.ModeHybrid, staticGatewayToken, agent.LevelReadOnly, false},
		{"hybrid/operator/static", authz.ModeHybrid, staticGatewayToken, agent.LevelOperator, false},
		{"hybrid/full/static", authz.ModeHybrid, staticGatewayToken, agent.LevelFull, false},
		{"hybrid/operator/readonly-token", authz.ModeHybrid, tokens[agent.LevelReadOnly], agent.LevelOperator, true},

		{"legacy/readonly/no-token", authz.ModeLegacy, "", agent.LevelReadOnly, false},
		{"legacy/operator/no-token", authz.ModeLegacy, "", agent.LevelOperator, true},
		{"legacy/full/no-token", authz.ModeLegacy, "", agent.LevelFull, true},
		{"legacy/readonly/static", authz.ModeLegacy, staticGatewayToken, agent.LevelReadOnly, false},
		{"legacy/operator/static", authz.ModeLegacy, staticGatewayToken, agent.LevelOperator, false},
		{"legacy/full/static", authz.ModeLegacy, staticGatewayToken, agent.LevelFull, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := fixture.config
			config.Mode = test.mode
			err := checkGatewayCredential(fixture.logger, config, configuredGatewayURL, test.token, test.level)
			if test.want && err == nil {
				t.Fatal("expected an error")
			}
			if !test.want && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// With no gateway admin URL the historical behaviour is preserved: a
// read-only agent may run without a token, and legacy/hybrid operator agents
// are not forced to supply one. jwt mode still requires a signed JWT.
func TestCheckGatewayCredentialWithoutGatewayURL(t *testing.T) {
	t.Parallel()
	fixture := newCredentialFixture(t)
	cases := []struct {
		name  string
		mode  authz.Mode
		token string
		level agent.OperationLevel
		want  bool
	}{
		{"legacy/operator/no-token", authz.ModeLegacy, "", agent.LevelOperator, false},
		{"hybrid/full/no-token", authz.ModeHybrid, "", agent.LevelFull, false},
		{"jwt/readonly/no-token", authz.ModeJWT, "", agent.LevelReadOnly, false},
		{"jwt/operator/no-token", authz.ModeJWT, "", agent.LevelOperator, true},
		{"jwt/full/no-token", authz.ModeJWT, "", agent.LevelFull, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := fixture.config
			config.Mode = test.mode
			err := checkGatewayCredential(fixture.logger, config, "", test.token, test.level)
			if test.want && err == nil {
				t.Fatal("expected an error")
			}
			if !test.want && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCheckGatewayCredentialTokenTypes(t *testing.T) {
	t.Parallel()
	fixture := newCredentialFixture(t)
	human := issueGatewayToken(t, fixture.signer, authz.TypeHuman, authz.RoleOperator)

	// A human token is never a valid gateway identity for an agent, even if
	// its role matches AGENT_OPERATION_LEVEL.
	for _, mode := range []authz.Mode{authz.ModeJWT, authz.ModeHybrid} {
		t.Run("human-token/"+string(mode), func(t *testing.T) {
			config := fixture.config
			config.Mode = mode
			err := checkGatewayCredential(fixture.logger, config, configuredGatewayURL, human, agent.LevelOperator)
			if err == nil || !strings.Contains(err.Error(), "typ") {
				t.Fatalf("err = %v, want a typ error", err)
			}
		})
	}

	// Two dots alone do not make a JWT. An unparseable string is handled as
	// a static token and must not be rejected as a malformed JWT.
	opaque := "%%%.%%%.%%%"
	for _, mode := range []authz.Mode{authz.ModeLegacy, authz.ModeHybrid} {
		t.Run("two-dot-static/"+string(mode), func(t *testing.T) {
			config := fixture.config
			config.Mode = mode
			if err := checkGatewayCredential(fixture.logger, config, configuredGatewayURL, opaque, agent.LevelFull); err != nil {
				t.Fatalf("two-dot static token rejected: %v", err)
			}
		})
	}
}
