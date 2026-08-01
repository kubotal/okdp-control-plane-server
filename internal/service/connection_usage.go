package service

import (
	"fmt"
	"strings"

	"github.com/okdp/okdp-server-new/internal/models"
)

// renderUsage turns a type's usage spec into what a consumer can actually
// paste: the connection string with the values substituted, and the
// environment variables its usual clients read.
//
// secret is the Secret holding the credentials, or nil for an internal
// connection, which has none. Credentials never appear as values — in the URI
// they stay the literal `${VAR}` the template wrote, and in the environment
// they become a reference to the Secret key.
func renderUsage(t *models.ConnectionType, values map[string]any, secret *models.SecretRef) models.ConnectionUsage {
	usage := models.ConnectionUsage{URILabel: t.Usage.URILabel}

	if t.Usage.URI != "" {
		usage.URI = trimEmptyPathSegments(expandTemplate(t.Usage.URI, values))
	}

	for _, spec := range t.Usage.Env {
		switch {
		case spec.SecretField != "":
			// A credential the consumer must mount. With no Secret — an internal
			// connection — there is nothing to point at, so the variable is
			// dropped rather than emitted with a dangling reference.
			if secret == nil {
				continue
			}
			usage.Env = append(usage.Env, models.ConnectionEnvVar{
				Name: spec.Name,
				SecretRef: &models.SecretRef{
					Name:      secret.Name,
					Namespace: secret.Namespace,
					Key:       spec.SecretField,
				},
			})
		case spec.From != "":
			v, ok := values[spec.From]
			// An unset optional field would otherwise yield `PGSSLMODE=`, which
			// overrides the client's own default with an empty string.
			if !ok || v == nil {
				continue
			}
			s := formatValue(v)
			if s == "" {
				continue
			}
			usage.Env = append(usage.Env, models.ConnectionEnvVar{Name: spec.Name, Value: s})
		}
	}

	return usage
}

// trimEmptyPathSegments removes the trailing slashes an unset optional segment
// leaves behind — a derived Trino connection knows no catalog or schema, and
// "jdbc:trino://host:8080//" is not something anyone can paste. The "://" of
// the scheme is never touched.
func trimEmptyPathSegments(uri string) string {
	for strings.HasSuffix(uri, "/") && !strings.HasSuffix(uri, "://") {
		uri = strings.TrimSuffix(uri, "/")
	}
	return uri
}

// expandTemplate replaces every `{field}` with that field's value. Anything
// else is copied verbatim, so `${PGPASSWORD}` survives as the placeholder the
// reader is meant to fill in themselves.
func expandTemplate(tmpl string, values map[string]any) string {
	var b strings.Builder
	for {
		open := strings.IndexByte(tmpl, '{')
		if open < 0 {
			b.WriteString(tmpl)
			return b.String()
		}
		// `${` introduces a shell expansion, not a field: skip past it so the
		// placeholder reaches the reader intact.
		if open > 0 && tmpl[open-1] == '$' {
			close := strings.IndexByte(tmpl[open:], '}')
			if close < 0 {
				b.WriteString(tmpl)
				return b.String()
			}
			b.WriteString(tmpl[:open+close+1])
			tmpl = tmpl[open+close+1:]
			continue
		}
		close := strings.IndexByte(tmpl[open:], '}')
		if close < 0 {
			b.WriteString(tmpl)
			return b.String()
		}
		b.WriteString(tmpl[:open])
		name := tmpl[open+1 : open+close]
		if v, ok := values[name]; ok && v != nil {
			b.WriteString(formatValue(v))
		}
		tmpl = tmpl[open+close+1:]
	}
}

// formatValue renders a value the way a connection string or an environment
// variable expects it — notably without the trailing ".0" a float64 decoded
// from JSON would otherwise print for a port.
func formatValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return fmt.Sprintf("%t", x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
	case int, int32, int64:
		return fmt.Sprintf("%d", x)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}
