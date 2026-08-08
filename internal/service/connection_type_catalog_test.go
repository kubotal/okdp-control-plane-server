package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCatalog(t *testing.T) ConnectionTypeCatalog {
	t.Helper()
	catalog, err := NewEmbeddedConnectionTypeCatalog()
	require.NoError(t, err, "the embedded connection types must all parse")
	return catalog
}

func TestEmbeddedCatalogLoadsBuiltInTypes(t *testing.T) {
	catalog := newTestCatalog(t)

	names := map[string]bool{}
	for _, ct := range catalog.List() {
		names[ct.Name] = true
	}

	// The three types the console offers for creation, plus Trino which is
	// only ever discovered from a deployed service.
	assert.True(t, names["postgresql"])
	assert.True(t, names["mysql"])
	assert.True(t, names["s3"])
	assert.True(t, names["trino"])
}

func TestExternalFlagMatchesCreationSupport(t *testing.T) {
	catalog := newTestCatalog(t)

	postgres, ok := catalog.Get("postgresql")
	require.True(t, ok)
	assert.True(t, postgres.External, "PostgreSQL is declarable by hand")

	trino, ok := catalog.Get("trino")
	require.True(t, ok)
	assert.False(t, trino.External, "Trino only comes from a deployed release")
}

func TestEveryTypeWithCredentialsMarksThemSecret(t *testing.T) {
	catalog := newTestCatalog(t)

	// A credential that is not marked secret would be written to the Connection
	// spec in clear, so assert the marking rather than trusting the JSON.
	for _, name := range []string{"postgresql", "mysql"} {
		ct, ok := catalog.Get(name)
		require.True(t, ok, name)
		assert.Contains(t, ct.SecretFields(), "password", name)
	}

	s3, ok := catalog.Get("s3")
	require.True(t, ok)
	assert.Contains(t, s3.SecretFields(), "secretKey")
	// Both halves of an S3 identity go to the Secret. Not because a key ID is a
	// secret on its own, it is not, but because the s3 Interface declares a Secret
	// "with keys: accessKey, secretKey" and consuming packages read both from it.
	assert.Contains(t, s3.SecretFields(), "accessKey", "the s3 contract expects the key ID in the Secret")
}

// A database outside the cluster is often reachable only in clear text —
// public datasets, older corporate instances. Without "prefer" the only
// working choice is "disable", which never even attempts TLS, so a server
// that does support it would be talked to unencrypted.
func TestPostgreSQLOffersThePreferSSLMode(t *testing.T) {
	catalog := newTestCatalog(t)

	postgres, ok := catalog.Get("postgresql")
	if !ok {
		t.Fatal("postgresql type missing")
	}
	sslMode, ok := postgres.Field("sslMode")
	if !ok {
		t.Fatal("sslMode field missing")
	}

	var found bool
	for _, option := range sslMode.Options {
		if option == "prefer" {
			found = true
		}
	}
	if !found {
		t.Errorf("sslMode options = %v, want one of them to be \"prefer\"", sslMode.Options)
	}
	if sslMode.Default != "prefer" {
		t.Errorf("sslMode default = %v, want prefer", sslMode.Default)
	}
}

func TestForServiceResolvesDeployedProviders(t *testing.T) {
	catalog := newTestCatalog(t)

	ct, ok := catalog.ForService("trino")
	require.True(t, ok, "a deployed Trino must be recognized as a connection provider")
	assert.Equal(t, "trino", ct.Name)

	// SeaweedFS is the object storage shipped by the platform packages.
	ct, ok = catalog.ForService("seaweedfs")
	require.True(t, ok)
	assert.Equal(t, "s3", ct.Name)

	_, ok = catalog.ForService("jupyterhub")
	assert.False(t, ok, "a service that provides no connection must not be listed")
}

func TestValidateAcceptsAWellFormedPayload(t *testing.T) {
	catalog := newTestCatalog(t)

	err := catalog.Validate("postgresql", map[string]any{
		"engine":   "postgresql",
		"driver":   "org.postgresql.Driver",
		"host":     "db.example.com",
		"port":     float64(5432),
		"dbName":   "analytics",
		"username": "reader",
		"password": "s3cret",
		"sslMode":  "require",
	})

	assert.NoError(t, err)
}

func TestValidateRejectsBadPayloads(t *testing.T) {
	catalog := newTestCatalog(t)

	base := func() map[string]any {
		return map[string]any{
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "db.example.com",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "require",
		}
	}

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		message string
	}{
		{
			name:    "missing required field",
			mutate:  func(v map[string]any) { delete(v, "host") },
			message: "Host is required",
		},
		{
			name:    "empty required field",
			mutate:  func(v map[string]any) { v["host"] = "" },
			message: "Host is required",
		},
		{
			name:    "port out of range",
			mutate:  func(v map[string]any) { v["port"] = float64(70000) },
			message: "Port must be at most 65535",
		},
		{
			name:    "port not a number",
			mutate:  func(v map[string]any) { v["port"] = "5432" },
			message: "Port must be a number",
		},
		{
			name:    "value outside the enum",
			mutate:  func(v map[string]any) { v["sslMode"] = "maybe" },
			message: "SSL mode must be one of",
		},
		{
			name:    "unknown field",
			mutate:  func(v map[string]any) { v["hostname"] = "db.example.com" },
			message: `unknown field "hostname"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := base()
			tt.mutate(values)

			err := catalog.Validate("postgresql", values)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	catalog := newTestCatalog(t)

	err := catalog.Validate("oracle", map[string]any{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown connection type "oracle"`)
}

func TestValidateAllowsOmittingOptionalFields(t *testing.T) {
	catalog := newTestCatalog(t)

	// region and pathStyle carry defaults and must not be demanded.
	err := catalog.Validate("s3", map[string]any{
		"apiUrl":    "https://s3.example.com",
		"bucket":    "warehouse",
		"accessKey": "AKIA",
		"secretKey": "secret",
	})

	assert.NoError(t, err)
}
