package service

import (
	"strings"
	"testing"

	"github.com/okdp/okdp-server-new/internal/models"
)

func postgresType() *models.ConnectionType {
	return &models.ConnectionType{
		Name: "postgresql",
		Fields: []models.ConnectionField{
			{Name: "host"}, {Name: "port"}, {Name: "database"},
			{Name: "user"}, {Name: "password", Secret: true}, {Name: "sslMode"},
		},
		Usage: models.ConnectionUsageSpec{
			URI:      "postgresql://{user}:${PGPASSWORD}@{host}:{port}/{database}?sslmode={sslMode}",
			URILabel: "Connection URI",
			Env: []models.ConnectionEnvSpec{
				{Name: "PGHOST", From: "host"},
				{Name: "PGPORT", From: "port"},
				{Name: "PGSSLMODE", From: "sslMode"},
				{Name: "PGPASSWORD", SecretField: "password"},
			},
		},
	}
}

func envByName(usage models.ConnectionUsage, name string) *models.ConnectionEnvVar {
	for i := range usage.Env {
		if usage.Env[i].Name == name {
			return &usage.Env[i]
		}
	}
	return nil
}

func TestRenderUsageBuildsTheURIFromTheValues(t *testing.T) {
	values := map[string]any{
		"host": "db.example.com", "port": float64(5432),
		"database": "crm", "user": "reader", "sslMode": "require",
	}

	usage := renderUsage(postgresType(), values, &models.SecretRef{Name: "c-credentials", Namespace: "demo"})

	want := "postgresql://reader:${PGPASSWORD}@db.example.com:5432/crm?sslmode=require"
	if usage.URI != want {
		t.Errorf("URI = %q, want %q", usage.URI, want)
	}
	if usage.URILabel != "Connection URI" {
		t.Errorf("URILabel = %q", usage.URILabel)
	}
}

// A port decoded from JSON is a float64; printing it naively yields "5432.0",
// which no client accepts.
func TestRenderUsageFormatsAPortWithoutADecimalPart(t *testing.T) {
	usage := renderUsage(postgresType(), map[string]any{"host": "h", "port": float64(5432)}, nil)

	if got := envByName(usage, "PGPORT"); got == nil || got.Value != "5432" {
		t.Errorf("PGPORT = %+v, want value 5432", got)
	}
}

func TestRenderUsageNeverPutsACredentialInAValue(t *testing.T) {
	values := map[string]any{"host": "h", "port": float64(5432), "password": "s3cret"}
	secret := &models.SecretRef{Name: "c-credentials", Namespace: "demo"}

	usage := renderUsage(postgresType(), values, secret)

	if got := envByName(usage, "PGPASSWORD"); got == nil {
		t.Fatal("PGPASSWORD missing")
	} else if got.Value != "" {
		t.Errorf("PGPASSWORD carries the value %q", got.Value)
	} else if got.SecretRef == nil || got.SecretRef.Name != "c-credentials" || got.SecretRef.Key != "password" {
		t.Errorf("PGPASSWORD secretRef = %+v", got.SecretRef)
	}
	// The password must not leak through the URI either.
	if strings.Contains(usage.URI, "s3cret") {
		t.Errorf("the URI discloses the password: %q", usage.URI)
	}
}

// An internal connection has no Secret, so a credential variable would point
// nowhere; it is dropped rather than emitted broken.
func TestRenderUsageDropsCredentialsWhenThereIsNoSecret(t *testing.T) {
	usage := renderUsage(postgresType(), map[string]any{"host": "h"}, nil)

	if got := envByName(usage, "PGPASSWORD"); got != nil {
		t.Errorf("PGPASSWORD emitted without a secret: %+v", got)
	}
}

// Emitting PGSSLMODE="" would override the client's own default.
func TestRenderUsageSkipsAnUnsetOptionalField(t *testing.T) {
	usage := renderUsage(postgresType(), map[string]any{"host": "h"}, nil)

	if got := envByName(usage, "PGSSLMODE"); got != nil {
		t.Errorf("PGSSLMODE emitted while unset: %+v", got)
	}
	if got := envByName(usage, "PGHOST"); got == nil || got.Value != "h" {
		t.Errorf("PGHOST = %+v, want value h", got)
	}
}

func TestExpandTemplateLeavesShellExpansionsAlone(t *testing.T) {
	got := expandTemplate("${PGPASSWORD}@{host}", map[string]any{"host": "h"})

	if got != "${PGPASSWORD}@h" {
		t.Errorf("got %q, want ${PGPASSWORD}@h", got)
	}
}

// A missing field must not leave the braces in the string.
func TestExpandTemplateDropsAnUnknownField(t *testing.T) {
	got := expandTemplate("s3://{bucket}/{prefix}", map[string]any{"bucket": "data"})

	if got != "s3://data/" {
		t.Errorf("got %q, want s3://data/", got)
	}
}

// A Trino connection derived from a deployed service knows no catalog and no
// schema, and "jdbc:trino://host:8080//" is not something anyone can paste.
func TestRenderUsageTrimsTheSegmentsOfUnsetOptionalFields(t *testing.T) {
	trino := &models.ConnectionType{
		Name: "trino",
		Usage: models.ConnectionUsageSpec{
			URI: "jdbc:trino://{host}:{port}/{catalog}/{schema}",
		},
	}

	usage := renderUsage(trino, map[string]any{"host": "trino.demo", "port": float64(8080)}, nil)

	if usage.URI != "jdbc:trino://trino.demo:8080" {
		t.Errorf("URI = %q, want jdbc:trino://trino.demo:8080", usage.URI)
	}
}

func TestRenderUsageKeepsASetSegmentAndTrimsOnlyWhatFollows(t *testing.T) {
	trino := &models.ConnectionType{
		Name: "trino",
		Usage: models.ConnectionUsageSpec{
			URI: "jdbc:trino://{host}:{port}/{catalog}/{schema}",
		},
	}

	usage := renderUsage(trino, map[string]any{
		"host": "trino.demo", "port": float64(8080), "catalog": "hive",
	}, nil)

	if usage.URI != "jdbc:trino://trino.demo:8080/hive" {
		t.Errorf("URI = %q, want jdbc:trino://trino.demo:8080/hive", usage.URI)
	}
}

// The scheme's own "//" must survive the trimming.
func TestTrimEmptyPathSegmentsLeavesASchemeAlone(t *testing.T) {
	if got := trimEmptyPathSegments("s3://"); got != "s3://" {
		t.Errorf("got %q, want s3://", got)
	}
}

func TestFormatValueRendersBooleansAndIntegers(t *testing.T) {
	if got := formatValue(true); got != "true" {
		t.Errorf("bool -> %q", got)
	}
	if got := formatValue(float64(8080)); got != "8080" {
		t.Errorf("float64 -> %q", got)
	}
	if got := formatValue(nil); got != "" {
		t.Errorf("nil -> %q", got)
	}
}
