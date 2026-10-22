package nativeapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// alterFixture serves the table read before the call and the one read back.
type alterFixture struct {
	events                                       []string
	before, after                                string
	missing, alterFailure, readFailure, closeErr bool
	described                                    int
	sent                                         *Ydb_Table.AlterTableRequest
}

func (f *alterFixture) Describe(_ context.Context, path string) (*native.DescribeTableResult, error) {
	f.events = append(f.events, "describe")
	f.described++
	if path != "/local/app/records" {
		return nil, errors.New("wrong describe path")
	}
	if (f.described == 1 && f.missing) || (f.described == 2 && f.readFailure) {
		return nil, errors.New("describe failed")
	}
	text := f.before
	if f.described > 1 {
		text = f.after
	}
	var result native.DescribeTableResult
	if err := protojson.Unmarshal([]byte(text), &result); err != nil {
		return nil, err
	}
	return &result, nil
}
func (f *alterFixture) Alter(_ context.Context, request *Ydb_Table.AlterTableRequest) error {
	f.events = append(f.events, "alter")
	f.sent = proto.CloneOf(request)
	if request.Path != "/local/app/records" || request.SessionId != "" {
		return errors.New("wrong dispatch binding")
	}
	if f.alterFailure {
		return errors.New("lost native reply")
	}
	return nil
}
func (f *alterFixture) Close() error {
	f.events = append(f.events, "close")
	if f.closeErr {
		return errors.New("unconfirmed session release")
	}
	return nil
}

const (
	tableHead = `{"self":{"name":"records","type":"TABLE"},"primaryKey":["id"],"storeType":"STORE_TYPE_ROW",`
	idColumn  = `{"name":"id","type":{"typeId":"UINT64"},"notNull":true}`
	// payload sits in the default family before the call.
	beforeTable = tableHead + `"columns":[` + idColumn + `,{"name":"payload","type":{"typeId":"UTF8"}}],"columnFamilies":[{"name":"default"}]}`
	// payload sits in the added archive family after the call.
	afterTable = tableHead + `"columns":[` + idColumn + `,{"name":"payload","type":{"typeId":"UTF8"},"family":"archive"}],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4"}]}`
	addAndMove = `{"request":{"addColumnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"}],"alterColumns":[{"name":"payload","family":"archive"}]},"held_columns":[{"name":"payload","family":""}]}`
	alterOnly  = `{"request":{"alterColumnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"}]},"held_families":[{"name":"archive"}]}`
)

func alterEnvelope(c *qt.C, database, payload string) *protocol.Envelope {
	c.Helper()
	definition := tablecreate.AlterModel()
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: tablecreate.AlterOperationKind, SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: []byte(payload),
		Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: "app", Name: "records"}}
}

func applyAlteration(c *qt.C, fixture *alterFixture, database, payload string, guardFailure bool) (bool, error) {
	c.Helper()
	api := &nativeapi.TableAlteration{Open: func(context.Context) (nativeapi.AlterationSession, error) {
		fixture.events = append(fixture.events, "open")
		return fixture, nil
	}}
	envelope := alterEnvelope(c, database, payload)
	original := proto.CloneOf(envelope)
	dispatched, err := api.Apply(c.Context(), "/local", envelope, func(context.Context) error {
		fixture.events = append(fixture.events, "guard")
		if guardFailure {
			return errors.New("lease lost")
		}
		return nil
	})
	c.Assert(proto.Equal(original, envelope), qt.IsTrue)
	return dispatched, err
}

// pooled is a table whose archive family keeps its data on the given media.
func pooled(media string) string {
	return tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default"},{"name":"archive","data":{"media":"` + media + `"}}]}`
}

func TestNativeTableAlteration_HappyPath(t *testing.T) {
	plain := tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`
	compressed := tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4"}]}`
	for _, test := range []struct {
		name            string
		before, after   string
		payload         string
		families, moves int
	}{
		{"adds a family and moves a column", beforeTable, afterTable, addAndMove, 1, 1},
		{"changes a family setting", plain, compressed, alterOnly, 0, 0},
		{"changes a storage pool", pooled("hdd"), pooled("ssd"), `{"request":{"alterColumnFamilies":[{"name":"archive","data":{"media":"ssd"}}]},"held_families":[{"name":"archive","data":{"media":"hdd"}}]}`, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := &alterFixture{before: test.before, after: test.after}
			dispatched, err := applyAlteration(c, fixture, "", test.payload, false)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.events, qt.DeepEquals, []string{"open", "describe", "guard", "alter", "describe", "close"})
			c.Assert(fixture.sent.AddColumnFamilies, qt.HasLen, test.families)
			c.Assert(fixture.sent.AlterColumns, qt.HasLen, test.moves)
		})
	}
}

func TestNativeTableAlteration_FailurePath(t *testing.T) {
	archived := tableHead + `"columns":[` + idColumn + `,{"name":"payload","type":{"typeId":"UTF8"},"family":"archive"}],"columnFamilies":[{"name":"default"},{"name":"archive"}]}`
	heldArchive := tableHead + `"columns":[` + idColumn + `,{"name":"payload","type":{"typeId":"UTF8"}}],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4"}]}`
	notRow := `{"self":{"name":"records","type":"TABLE"},"primaryKey":["id"],"storeType":"STORE_TYPE_COLUMN","columns":[` + idColumn + `]}`
	renamed := `{"self":{"name":"other","type":"TABLE"},"primaryKey":["id"],"storeType":"STORE_TYPE_ROW","columns":[` + idColumn + `]}`
	for _, test := range []struct {
		name       string
		fixture    alterFixture
		database   string
		payload    string
		guard      bool
		dispatched bool
		events     []string
		message    string
	}{
		{"missing table", alterFixture{missing: true, before: beforeTable}, "", addAndMove, false, false, []string{"open", "describe", "close"}, "describe failed"},
		{"family to add exists", alterFixture{before: archived, after: archived}, "", addAndMove, false, false, []string{"open", "describe", "close"}, "YDB column family to add already exists"},
		{"column family drifted", alterFixture{before: beforeTable, after: afterTable}, "", alterOnly, false, false, []string{"open", "describe", "close"}, "YDB column family differs from the plan"},
		{"column drifted", alterFixture{before: archived}, "", `{"request":{"alterColumns":[{"name":"payload","family":"archive"}]},"held_columns":[{"name":"payload","family":""}]}`, false, false, []string{"open", "describe", "close"}, "YDB column differs from the plan"},
		{"move into an unknown family", alterFixture{before: beforeTable}, "", `{"request":{"alterColumns":[{"name":"payload","family":"cold"}]},"held_columns":[{"name":"payload","family":""}]}`, false, false, []string{"open", "describe", "close"}, "YDB column moves into a family the table does not hold"},
		{"not a row table", alterFixture{before: notRow}, "", addAndMove, false, false, []string{"open", "describe", "close"}, "YDB table identity differs from the alteration"},
		{"another table", alterFixture{before: renamed}, "", addAndMove, false, false, []string{"open", "describe", "close"}, "YDB table identity differs from the alteration"},
		{"foreign database", alterFixture{before: beforeTable}, "/other", addAndMove, false, false, nil, "YDB table alteration targets another database"},
		{"lease lost", alterFixture{before: beforeTable}, "", addAndMove, true, false, []string{"open", "describe", "guard", "close"}, "lease lost"},
		{"lost reply", alterFixture{before: beforeTable, alterFailure: true}, "", addAndMove, false, true, []string{"open", "describe", "guard", "alter", "close"}, "lost native reply"},
		{"lost read", alterFixture{before: beforeTable, readFailure: true}, "", addAndMove, false, true, []string{"open", "describe", "guard", "alter", "describe", "close"}, "describe failed"},
		{"nothing changed", alterFixture{before: beforeTable, after: beforeTable}, "", addAndMove, false, true, []string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB added column family differs from the request"},
		{"column stayed", alterFixture{before: beforeTable, after: tableHead + `"columns":[` + idColumn + `,{"name":"payload","type":{"typeId":"UTF8"}}],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4"}]}`}, "", addAndMove, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered column differs from the request"},
		{"another column moved", alterFixture{before: beforeTable, after: tableHead + `"columns":[{"name":"id","type":{"typeId":"UINT64"},"notNull":true,"family":"archive"},{"name":"payload","type":{"typeId":"UTF8"},"family":"archive"}],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4"}]}`}, "", addAndMove, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered column differs from the request"},
		{"another family appeared", alterFixture{before: beforeTable, after: tableHead + `"columns":[` + idColumn + `,{"name":"payload","type":{"typeId":"UTF8"},"family":"archive"}],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4"},{"name":"extra"}]}`}, "", addAndMove, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered table holds unexpected column families"},
		{"setting not applied", alterFixture{before: heldArchive, after: heldArchive}, "", `{"request":{"alterColumnFamilies":[{"name":"archive","compression":"COMPRESSION_NONE"}]},"held_families":[{"name":"archive","compression":"COMPRESSION_LZ4"}]}`, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered column family differs from the request"},
		{"family settings drifted", alterFixture{before: heldArchive}, "", alterOnly, false, false, []string{"open", "describe", "close"}, "YDB column family differs from the plan"},
		{"key changed", alterFixture{before: beforeTable, after: strings.Replace(afterTable, `"primaryKey":["id"]`, `"primaryKey":["id","payload"]`, 1)}, "", addAndMove, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered table key or columns differ from the request"},
		{"column lost", alterFixture{before: beforeTable, after: tableHead + `"columns":[` + idColumn + `],"columnFamilies":[{"name":"default"},{"name":"archive","compression":"COMPRESSION_LZ4"}]}`}, "", addAndMove, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered table key or columns differ from the request"},
		{"column type changed", alterFixture{before: beforeTable, after: strings.Replace(afterTable, `{"typeId":"UTF8"}`, `{"typeId":"STRING"}`, 1)}, "", addAndMove, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered column differs from the request"},
		{"pool not applied", alterFixture{before: pooled("hdd"), after: pooled("hdd")}, "", `{"request":{"alterColumnFamilies":[{"name":"archive","data":{"media":"ssd"}}]},"held_families":[{"name":"archive","data":{"media":"hdd"}}]}`, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered column family differs from the request"},
		{"added pool not applied", alterFixture{before: beforeTable, after: afterTable}, "", `{"request":{"addColumnFamilies":[{"name":"archive","data":{"media":"ssd"},"compression":"COMPRESSION_LZ4"}],"alterColumns":[{"name":"payload","family":"archive"}]},"held_columns":[{"name":"payload","family":""}]}`, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB added column family differs from the request"},
		{"keep_in_memory appeared", alterFixture{before: heldArchive, after: strings.Replace(heldArchive, `"compression":"COMPRESSION_LZ4"`, `"compression":"COMPRESSION_NONE","keepInMemory":"ENABLED"`, 1)}, "", `{"request":{"alterColumnFamilies":[{"name":"archive","compression":"COMPRESSION_NONE"}]},"held_families":[{"name":"archive","compression":"COMPRESSION_LZ4"}]}`, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "YDB altered column family differs from the request"},
		{"unconfirmed cleanup", alterFixture{before: beforeTable, after: afterTable, closeErr: true}, "", addAndMove, false, true,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}, "unconfirmed session release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := test.fixture
			dispatched, err := applyAlteration(c, &fixture, test.database, test.payload, test.guard)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(fixture.events, qt.DeepEquals, test.events)
		})
	}
}

func TestNativeTableAlteration_RefusesUnsafeSessionDatabase(t *testing.T) {
	for _, database := range []string{"/local/../other", "local", "/"} {
		t.Run(database, func(t *testing.T) {
			c := qt.New(t)
			fixture := &alterFixture{before: beforeTable}
			api := &nativeapi.TableAlteration{Open: func(context.Context) (nativeapi.AlterationSession, error) {
				fixture.events = append(fixture.events, "open")
				return fixture, nil
			}}
			dispatched, err := api.Apply(c.Context(), database, alterEnvelope(c, "", addAndMove), func(context.Context) error { return nil })
			c.Assert(err, qt.IsNotNil)
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(fixture.events, qt.HasLen, 0)
		})
	}
}
