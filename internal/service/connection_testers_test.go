package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A probe reading a field the descriptor does not write is invisible: the value
// comes out empty, the server refuses the connection, and the user is told their
// credentials are wrong. That is exactly what happened when the descriptor
// renamed database/user to dbName/username and these two functions kept the old
// names. The probe therefore gets tested against the descriptor itself, not
// against a hand-written fixture that could drift the same way.
func TestProbesReadTheFieldsTheDescriptorDeclares(t *testing.T) {
	catalog := newTestCatalog(t)
	database, ok := catalog.Get("database-server")
	require.True(t, ok)

	// One distinctive value per declared field, so a probe reading the wrong
	// name gets the empty string and the assertions below catch it.
	values := connectionValues{}
	for i := range database.Fields {
		values[database.Fields[i].Name] = "probe-" + database.Fields[i].Name
	}
	values["port"] = float64(5432)
	values["engine"] = "postgresql"
	values["sslMode"] = "disable"

	pg, err := postgresConfig(values)
	require.NoError(t, err)
	assert.Equal(t, "probe-host", pg.Host)
	assert.Equal(t, "probe-dbName", pg.Database, "the probe must read the database name the form writes")
	assert.Equal(t, "probe-username", pg.User, "the probe must read the user the form writes")
	assert.Equal(t, "probe-password", pg.Password)

	values["engine"] = "mysql"
	values["tls"] = "preferred"
	my, err := mysqlConfig(values)
	require.NoError(t, err)
	assert.Equal(t, "probe-dbName", my.DBName, "the probe must read the database name the form writes")
	assert.Equal(t, "probe-username", my.User, "the probe must read the user the form writes")
	assert.Equal(t, "probe-password", my.Passwd)
}
