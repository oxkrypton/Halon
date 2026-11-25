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
	UserID   string
	Type     PrincipalType
	Role     Role
	Audience Audience
	Issuer   string
	JTI      string
	// PasswordChangeRequired is true when the user must set a new password
	// before using any permission-guarded endpoint. Static credentials and
	// agent tokens never carry it.
	PasswordChangeRequired bool
	// Static is true for legacy ADMIN_TOKEN / INGEST_TOKEN credentials.
	Static bool
}
