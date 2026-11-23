package authz

// PrincipalType distinguishes human-issued tokens from agent credentials.
type PrincipalType string

const (
	TypeHuman PrincipalType = "human"
	TypeAgent PrincipalType = "agent"
)

// Audience pins a token to one component so it cannot be replayed elsewhere.
type Audience string

const (
	AudienceGatewayAdmin Audience = "halon-gateway-admin"
	AudienceAgent        Audience = "halon-agent"
)

// Principal is the authenticated identity carried through one request.
type Principal struct {
	Subject  string
	Type     PrincipalType
	Role     Role
	Audience Audience
	Issuer   string
	JTI      string
	// Static is true for legacy ADMIN_TOKEN / INGEST_TOKEN credentials.
	Static bool
}
