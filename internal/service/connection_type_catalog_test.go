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
	assert.NotContains(t, s3.SecretFields(), "accessKey", "the key ID is not a credential on its own")
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
		"host":     "db.example.com",
		"port":     float64(5432),
		"database": "analytics",
		"user":     "reader",
		"password": "s3cret",
		"sslMode":  "require",
	})

	assert.NoError(t, err)
}

func TestValidateRejectsBadPayloads(t *testing.T) {
	catalog := newTestCatalog(t)

	base := func() map[string]any {
		return map[string]any{
			"host":     "db.example.com",
			"port":     float64(5432),
			"database": "analytics",
			"user":     "reader",
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
		"endpoint":  "https://s3.example.com",
		"bucket":    "warehouse",
		"accessKey": "AKIA",
		"secretKey": "secret",
	})

	assert.NoError(t, err)
}
