//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

func inspectionFixture(c *qt.C) (*connection.Connection, string, string) {
	c.Helper()
	address, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	directory := "ptah4204_" + strings.ToLower(rand.Text())
	absolute := path.Join(address.Database(), directory)
	c.Assert(opened.Driver.Scheme().MakeDirectory(ctx, absolute), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(opened.Driver.Scheme().RemoveDirectory(cleanup, absolute), qt.IsNil)
	})
	return opened, directory, address.Database()
}

func createFixtureTable(c *qt.C, opened *connection.Connection, database, directory string) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	table := path.Join(database, directory, "records")
	statement := fmt.Sprintf("CREATE TABLE `%s` (a Utf8 NOT NULL, b Uint64 NOT NULL, amount Decimal(22,9), expires Timestamp, PRIMARY KEY (b,a)) WITH (TTL = Interval(\"P1D\") ON expires)", table)
	_, err := opened.DB.ExecContext(ctx, statement)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
}

// execFixtureSchema waits only after an explicit rejection of a schema change
// while an earlier change is still running. A successful changefeed creation
// can return before YDB releases the table's schema operation. Transport errors
// and uncertain outcomes must not replay DDL, even during fixture setup.
func execFixtureSchema(ctx context.Context, db *sql.DB, statement string) error {
	for {
		_, err := db.ExecContext(ctx, statement)
		if !ydb.IsOperationErrorOverloaded(err) || !strings.Contains(err.Error(), "path is under operation") {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestRowTableInspectionSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	createFixtureTable(c, opened, database, directory)
	child := path.Join(directory, "child")
	setup, cancelSetup := context.WithTimeout(context.Background(), time.Minute)
	c.Assert(opened.Driver.Scheme().MakeDirectory(setup, path.Join(database, child)), qt.IsNil)
	cancelSetup()
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(opened.Driver.Scheme().RemoveDirectory(cleanup, path.Join(database, child)), qt.IsNil)
	})
	createFixtureTable(c, opened, database, child)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The same native reader is checked first to retain actionable diagnostics
	// if the subprocess's intentionally redacted RPC reports a refusal.
	direct, err := inspect.Read(ctx, opened.Driver, database, []string{directory})
	c.Assert(err, qt.IsNil)
	c.Assert(direct.GetTables(), qt.HasLen, 1)
	all, err := inspect.Read(ctx, opened.Driver, database, nil)
	c.Assert(err, qt.IsNil)
	var selected []string
	for _, table := range all.Tables {
		if table.Identity.Schema == directory || table.Identity.Schema == child {
			selected = append(selected, table.Identity.Schema)
		}
	}
	c.Assert(selected, qt.DeepEquals, []string{directory, child})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.TargetFeatures, qt.DeepEquals, client.Description().Engines[0].Features)
	c.Assert(session.Features, qt.DeepEquals, client.Description().Engines[0].Features)
	c.Assert(snapshot.Tables, qt.HasLen, 1)
	table := snapshot.Tables[0]
	c.Assert(table.Identity.Schema, qt.Equals, directory)
	c.Assert(table.Identity.Database, qt.Equals, database)
	c.Assert(table.Identity.Name, qt.Equals, "records")
	var columns []string
	for _, column := range table.Columns {
		columns = append(columns, fmt.Sprintf("%s:%s:%t:%t", column.Name, column.Type, column.Nullable, column.PrimaryKey))
	}
	c.Assert(columns, qt.DeepEquals, []string{"a:Utf8:false:true", "b:Uint64:false:true", "amount:Decimal(22,9):true:false", "expires:Timestamp:true:false"})
	c.Assert(table.Facets[0].Kind, qt.Equals, schema.TableProperties)
	var native nativeprotocol.DescribeTableResult
	c.Assert(protojson.Unmarshal(table.Facets[0].Payload, &native), qt.IsNil)
	c.Assert(native.GetPrimaryKey(), qt.DeepEquals, []string{"b", "a"})
	c.Assert(native.GetTtlSettings().GetDateTypeColumn().GetColumnName(), qt.Equals, "expires")
	c.Assert(native.GetTtlSettings().GetDateTypeColumn().GetExpireAfterSeconds(), qt.Equals, uint32(86400))
	c.Assert(native.GetColumns(), qt.HasLen, 0)
	values, err := provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	normalized, err := client.NormalizeValues(ctx, values)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateNormalizedInspection(client.Description(), request, snapshot, normalized), qt.IsNil)
	c.Assert(normalized.Values, qt.HasLen, 7)
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	// Captured observations remain usable after their database session closes.
	again, err := client.NormalizeValues(ctx, values)
	c.Assert(err, qt.IsNil)
	c.Assert(string(again.Values[0].CanonicalProjection), qt.Equals, string(normalized.Values[0].CanonicalProjection))
}

func TestInspectionRefusesEscapingAndMissingScopes(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	empty, err := inspect.Read(ctx, opened.Driver, database, []string{directory})
	c.Assert(err, qt.IsNil)
	c.Assert(empty.Tables, qt.HasLen, 0)
	c.Assert(empty.Coverage.Subjects, qt.HasLen, 0)
	createFixtureTable(c, opened, database, directory)
	for _, selectors := range [][]string{{"../other"}, {directory, "/" + directory}, {""}, {".sys"}, {directory + "/../other"}, {directory + "/missing"}} {
		snapshot, err := inspect.Read(ctx, opened.Driver, database, selectors)
		c.Assert(snapshot, qt.IsNil)
		c.Assert(err, qt.IsNotNil)
	}
}

func TestInspectionRefusesUnmodeledObjectWithoutPartialSnapshot(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	createFixtureTable(c, opened, database, directory)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	unsupported := path.Join(database, directory, "z_unmodeled")
	_, err := opened.DB.ExecContext(ctx, "CREATE TABLE `"+unsupported+"` (id Int64 NOT NULL, PRIMARY KEY(id)) PARTITION BY HASH(id) WITH (STORE=COLUMN)")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := opened.DB.ExecContext(cleanup, "DROP TABLE `"+unsupported+"`")
		c.Check(err, qt.IsNil)
	})
	// The supported records table sorts first. The later column table must invalidate
	// the whole snapshot, including the already captured table and its facets.
	snapshot, err := inspect.Read(ctx, opened.Driver, database, []string{directory})
	c.Assert(snapshot, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "YDB object kind COLUMN_TABLE needs an extracted reader")
}
