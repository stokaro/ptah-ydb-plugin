//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func TestCompleteObservedColumnsSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	table := path.Join(database, directory, "records")
	c.Assert(execFixtureSchema(ctx, opened.DB, "CREATE TABLE `"+table+"` (id BigSerial NOT NULL, amount Decimal(22,9), label Utf8 DEFAULT ''u, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil)
	})
	c.Assert(execFixtureSchema(ctx, opened.DB, "ALTER SEQUENCE `"+table+"/_serial_column_id` START WITH 7 INCREMENT BY 3 RESTART WITH 11"), qt.IsNil)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 1)
	columns := snapshot.Tables[0].Columns
	c.Assert(columns, qt.HasLen, 3)
	id := columns[0].Observed
	c.Assert(id.AutoIncrement, qt.IsTrue)
	c.Assert(id.PrimaryKey, qt.IsTrue)
	c.Assert([]string{id.IdentityStart, id.IdentityIncrement, id.SequenceRestart}, qt.DeepEquals, []string{"7", "3", "11"})
	c.Assert(columns[1].Observed.GetNumericPrecision(), qt.Equals, int64(22))
	c.Assert(columns[1].Observed.GetNumericScale(), qt.Equals, int64(9))
	c.Assert(columns[2].Observed.DefaultExpression, qt.DeepEquals, new("''u"))
	for i, column := range columns {
		c.Assert(column.Observed.Name, qt.Equals, column.Name)
		c.Assert(column.Observed.OrdinalPosition, qt.Equals, int64(i+1))
		c.Assert(column.Type, qt.Equals, "")
		c.Assert(column.DefaultExpression, qt.IsNil)
	}
	values, err := provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	normalized, err := client.NormalizeValues(ctx, values)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateNormalizedInspection(client.Description(), request, snapshot, normalized), qt.IsNil)
}
