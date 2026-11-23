package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/authz"
)

func TestKeygenIssueInspectRoundTrip(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if err := run([]string{"keygen"}, &output); err != nil {
		t.Fatal(err)
	}
	var keys keygenOutput
	if err := json.Unmarshal(output.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if keys.Kid == "" || keys.PrivateKey == "" || keys.PublicKey == "" {
		t.Fatalf("incomplete keygen output: %+v", keys)
	}

	token, err := issueToken(issueRequest{
		PrivateKey: keys.PrivateKey,
		Subject:    "alice",
		Type:       "human",
		Role:       "operator",
		Audience:   string(authz.AudienceGatewayAdmin),
		TTL:        time.Hour,
		Issuer:     authz.DefaultIssuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(token, ".")) != 3 {
		t.Fatalf("unexpected token: %q", token)
	}

	verified, err := inspectToken(token, keys.PublicKeysEnv, string(authz.AudienceGatewayAdmin), authz.DefaultIssuer)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Verified || verified.Claims["role"] != "operator" || verified.Claims["sub"] != "alice" {
		t.Fatalf("unexpected inspect result: %+v", verified)
	}

	unverified, err := inspectToken(token, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if unverified.Verified || unverified.Claims["sub"] != "alice" {
		t.Fatalf("unexpected unverified inspect result: %+v", unverified)
	}

	if _, err := inspectToken(token, keys.PublicKeysEnv, string(authz.AudienceAgent), authz.DefaultIssuer); err == nil {
		t.Error("expected audience mismatch to fail")
	}
}

func TestRunDispatch(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := run([]string{"keygen"}, &output); err != nil {
		t.Fatal(err)
	}
	var keys keygenOutput
	if err := json.Unmarshal(output.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	err := run([]string{
		"token", "issue", "--private-key", keys.PrivateKey,
		"--sub", "agent-1", "--type", "agent", "--role", "readonly",
		"--aud", string(authz.AudienceGatewayAdmin), "--ttl", "1h",
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	result, err := inspectToken(strings.TrimSpace(output.String()), keys.PublicKeysEnv, string(authz.AudienceGatewayAdmin), authz.DefaultIssuer)
	if err != nil || !result.Verified || result.Claims["typ"] != "agent" || result.Claims["role"] != "readonly" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if err := run(nil, &output); err == nil {
		t.Error("expected usage error")
	}
}
