package models

// Connection scopes. A project connection lives in the project namespace; a
// platform connection is cluster-wide and shared by every project.
const (
	ConnectionScopeProject  = "project"
	ConnectionScopePlatform = "platform"
)

// Reasons returned by a connectivity test, so the console can tell a network
// problem from a credential problem instead of showing a raw driver error.
const (
	TestReasonUnreachable   = "unreachable"
	TestReasonAuthFailed    = "auth-failed"
	TestReasonNotFound      = "not-found"
	TestReasonTimeout       = "timeout"
	TestReasonInvalidConfig = "invalid-config"
	TestReasonUnknown       = "unknown"
)

// Field types accepted in a connection type descriptor.
const (
	FieldTypeString  = "string"
	FieldTypeNumber  = "number"
	FieldTypeBoolean = "boolean"
	FieldTypeEnum    = "enum"
)

// ConnectionField describes a single input of a connection type.
type ConnectionField struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Secret      bool     `json:"secret,omitempty"`
	Default     any      `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
}

// ConnectionProviders tells how a service deployed on the platform is
// recognized as providing this kind of connection, and which of the ports it
// publishes carries it. Used to derive the internal connections of a project.
type ConnectionProviders struct {
	// Services matches the `okdp.io/service` label of a deployed Release.
	Services []string `json:"services"`
	// PortNames lists, in priority order, the Service port names that carry the
	// connection. A port matching none of them is used only as a fallback.
	PortNames []string `json:"portNames"`
	// DefaultPort is used when no Service port could be resolved.
	DefaultPort int32 `json:"defaultPort"`
}

// ConnectionUsageSpec describes, in the type descriptor, how a connection of
// this kind is consumed. The server renders it against a connection's values;
// the console only displays the result, so the formats live in one place.
type ConnectionUsageSpec struct {
	// URI is a template of the paste-ready connection string. `{field}` is
	// replaced by that field's value. Anything else — notably `${VAR}` shell
	// expansions standing in for a credential — is left verbatim, so a password
	// is never interpolated into a string the console displays.
	URI string `json:"uri,omitempty"`
	// URILabel names the string for the reader ("JDBC URL", "Connection URI").
	URILabel string `json:"uriLabel,omitempty"`
	// Env lists the environment variables the usual clients of this type read.
	Env []ConnectionEnvSpec `json:"env,omitempty"`
}

// ConnectionEnvSpec is one environment variable of a type's usage. Exactly one
// of From or SecretField is set: From names a plain value field, SecretField a
// credential that resolves to a Secret reference rather than to a value.
type ConnectionEnvSpec struct {
	Name        string `json:"name"`
	From        string `json:"from,omitempty"`
	SecretField string `json:"secretField,omitempty"`
}

// ConnectionType is the descriptor of one kind of connection. It drives the
// form rendered by the console, the server-side validation of submitted
// values, the recognition of deployed services as connection providers, and
// the ready-to-use snippets shown when a connection is opened.
type ConnectionType struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Category    string `json:"category"`
	// External reports whether a user may declare a connection of this type by
	// hand. Types that only ever come from a service deployed on the platform
	// (Trino, ...) are listed for the internal view but offer no creation form.
	External  bool                `json:"external"`
	Fields    []ConnectionField   `json:"fields"`
	Providers ConnectionProviders `json:"providers"`
	Usage     ConnectionUsageSpec `json:"usage"`
}

// Field returns the named field of the type.
func (t *ConnectionType) Field(name string) (*ConnectionField, bool) {
	for i := range t.Fields {
		if t.Fields[i].Name == name {
			return &t.Fields[i], true
		}
	}
	return nil, false
}

// SecretFields lists the fields of the type that hold credentials. Their values
// live in a Kubernetes Secret and never in the Connection spec.
func (t *ConnectionType) SecretFields() []string {
	var names []string
	for i := range t.Fields {
		if t.Fields[i].Secret {
			names = append(names, t.Fields[i].Name)
		}
	}
	return names
}

// ConnectionCatalogResponse is the payload of GET /api/connection-types.
type ConnectionCatalogResponse struct {
	Types []ConnectionType `json:"types"`
	// CRDAvailable reports whether the KuboCD connection CRDs are installed.
	// While they are not, external connections cannot be persisted and the
	// console says so instead of failing on save; internal connections are
	// derived from deployed services and stay available either way.
	CRDAvailable bool `json:"crdAvailable"`
}

// ConnectionRequest is the body for creating or updating an external connection.
type ConnectionRequest struct {
	Name        string         `json:"name" binding:"required"`
	Type        string         `json:"type" binding:"required"`
	Description string         `json:"description,omitempty"`
	Values      map[string]any `json:"values" binding:"required"`
}

// ConnectionTestRequest is the body of a connectivity test. It carries no name:
// a connection is tested before it is created.
type ConnectionTestRequest struct {
	Type   string         `json:"type" binding:"required"`
	Values map[string]any `json:"values" binding:"required"`
}

// ConnectionTestResult is the outcome of a connectivity test.
type ConnectionTestResult struct {
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	Reason     string `json:"reason,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// SecretRef points at the key of a Kubernetes Secret holding one credential —
// what a consumer needs to mount it, never the value itself.
type SecretRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Key       string `json:"key"`
}

// CredentialsSecretRef is the Secret holding a connection's credentials, with
// the keys it carries. It names no single key — a connection may hold several.
type CredentialsSecretRef struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	Keys      []string `json:"keys,omitempty"`
}

// ConnectionEnvVar is one rendered environment variable. Value carries a plain
// value; SecretRef points at the Secret key instead, for a credential.
type ConnectionEnvVar struct {
	Name      string     `json:"name"`
	Value     string     `json:"value,omitempty"`
	SecretRef *SecretRef `json:"secretRef,omitempty"`
}

// ConnectionUsage is what a consumer needs to actually use a connection: the
// paste-ready string and the environment variables its usual clients read.
// Credentials appear only as Secret references.
type ConnectionUsage struct {
	URI      string             `json:"uri,omitempty"`
	URILabel string             `json:"uriLabel,omitempty"`
	Env      []ConnectionEnvVar `json:"env,omitempty"`
}

// ConnectionResponse is an external connection as returned by the API. The
// values of secret fields are never included; only their names are, so that
// the console can show a credential as set without being able to read it.
type ConnectionResponse struct {
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Scope        string         `json:"scope"`
	Namespace    string         `json:"namespace,omitempty"`
	Description  string         `json:"description,omitempty"`
	Status       string         `json:"status"`
	Message      string         `json:"message,omitempty"`
	Values       map[string]any `json:"values"`
	SecretFields []string       `json:"secretFields,omitempty"`
	// CredentialsSecret is the Secret holding the secret fields, so a consumer
	// can reference it from a Deployment without the console ever reading it.
	CredentialsSecret *CredentialsSecretRef `json:"credentialsSecret,omitempty"`
	Usage             ConnectionUsage       `json:"usage"`
	CreatedAt         string                `json:"createdAt,omitempty"`
}

// InternalConnection is a connection provided by a service already deployed in
// the project, that the project's other services can consume.
type InternalConnection struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	TypeDisplay string `json:"typeDisplay"`
	Icon        string `json:"icon,omitempty"`
	Category    string `json:"category,omitempty"`
	// Service and ReleaseName identify the deployed instance providing it.
	Service     string `json:"service"`
	ReleaseName string `json:"releaseName"`
	Namespace   string `json:"namespace"`
	Status      string `json:"status"`
	// Endpoint is the in-cluster address another service uses to reach it.
	Endpoint string         `json:"endpoint"`
	Host     string         `json:"host"`
	Port     int32          `json:"port"`
	Values   map[string]any `json:"values"`
	// Managed reports that the entry comes from a Connection the KuboCD release
	// controller owns, rather than being derived from the deployed service.
	Managed bool `json:"managed"`
	// Usage is rendered the same way as for an external connection, minus the
	// credentials: an internal connection carries none.
	Usage     ConnectionUsage `json:"usage"`
	CreatedAt string          `json:"createdAt,omitempty"`
}

// PackageInput is one connection a package declares it needs. The console
// offers a choice for each: an existing connection, a new one, or nothing at
// all when the package marks it optional.
type PackageInput struct {
	// Alias names the input in the package's templates, and is the key the
	// console sends back when the user picks a connection.
	Alias string `json:"alias"`
	// Interface is the contract the chosen connection must satisfy. It is what
	// makes the choice safe: only connections of that type are offered.
	Interface string `json:"interface"`
	// Parameter is the package parameter carrying the chosen connection name,
	// derived from the input's namedConnection template. Empty when the input
	// binds some other way, in which case the console offers no choice.
	Parameter string `json:"parameter,omitempty"`
	// Optional reports that the package tolerates no connection at all.
	Optional bool `json:"optional"`
	// Description is shown next to the field.
	Description string `json:"description,omitempty"`
}
