package authz

import (
	"crypto/subtle"
	"errors"
)

// Authenticator resolves a bearer credential to a Principal. Static tokens
// exist only for legacy/hybrid compatibility and always map to the full role.
type Authenticator struct {
	mode        Mode
	audience    Audience
	staticToken string
	staticRole  Role
	staticSubj  string
	verifier    *Verifier
}

func NewAuthenticator(cfg Config, audience Audience, staticToken string) (*Authenticator, error) {
	verifier, err := cfg.Verifier()
	if err != nil {
		return nil, err
	}
	subject := "ingest-token"
	if audience == AudienceGatewayAdmin {
		subject = "admin-token"
	}
	return &Authenticator{
		mode:        cfg.Mode,
		audience:    audience,
		staticToken: staticToken,
		staticRole:  RoleFull,
		staticSubj:  subject,
		verifier:    verifier,
	}, nil
}

// Authenticate checks the static credential first, then the JWT verifier in
// hybrid mode; jwt mode never falls back to a static token.
func (a *Authenticator) Authenticate(raw string) (*Principal, error) {
	if raw == "" {
		return nil, errors.New("missing credential")
	}
	if a.mode != ModeJWT && a.staticToken != "" &&
		subtle.ConstantTimeCompare([]byte(raw), []byte(a.staticToken)) == 1 {
		return &Principal{
			Subject:  a.staticSubj,
			Type:     TypeHuman,
			Role:     a.staticRole,
			Audience: a.audience,
			Issuer:   "static",
			JTI:      "static",
			Static:   true,
		}, nil
	}
	if a.mode == ModeLegacy || a.verifier == nil {
		return nil, errors.New("invalid credential")
	}
	return a.verifier.Verify(raw, a.audience)
}
