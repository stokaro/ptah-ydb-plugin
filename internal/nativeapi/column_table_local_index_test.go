package nativeapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// indexedPage is the monitoring page of col_index_probe as local-ydb 26.2.1.14
// described it (internal/columnstore/testdata/26-2-indexes), with the local
// indexes edit leaves: it is given the page's index list and returns the one
// the page holds instead.
func indexedPage(c *qt.C, edit func([]any) []any) []byte {
	c.Helper()
	page, err := os.ReadFile("../columnstore/testdata/26-2-indexes.json")
	c.Assert(err, qt.IsNil)
	decoder := json.NewDecoder(bytes.NewReader(page))
	decoder.UseNumber()
	var decoded map[string]any
	c.Assert(decoder.Decode(&decoded), qt.IsNil)
	described := decoded["PathDescription"].(map[string]any)["ColumnTableDescription"].(map[string]any)["Schema"].(map[string]any)
	described["Indexes"] = edit(described["Indexes"].([]any))
	encoded, err := json.Marshal(decoded)
	c.Assert(err, qt.IsNil)
	return encoded
}

// withoutIndex leaves out the indexes named names.
func withoutIndex(names ...string) func([]any) []any {
	return func(indexes []any) []any {
		return slices.DeleteFunc(indexes, func(index any) bool { return slices.Contains(names, index.(map[string]any)["Name"].(string)) })
	}
}

// withIndex adds an index the page describes as given.
func withIndex(c *qt.C, entry string) func([]any) []any {
	return func(indexes []any) []any {
		decoder := json.NewDecoder(bytes.NewReader([]byte(entry)))
		decoder.UseNumber()
		var decoded any
		c.Assert(decoder.Decode(&decoded), qt.IsNil)
		return append(indexes, decoded)
	}
}

// unchanged leaves the page as it is.
func unchanged(indexes []any) []any { return indexes }

// asRead is the page as 26.2.1.14 described it.
func asRead(*qt.C) func([]any) []any { return unchanged }

// noChange leaves a fixture as it is.
func noChange(*localIndexFixture) {}

// The bloom index bf2 on body as 26.2.1.14 describes one ALTER TABLE ... ADD
// INDEX added with a probability of 0.05.
const addedBloomEntry = `{"BloomFilter":{"DataExtractor":{"ClassName":"DEFAULT"},"FalsePositiveProbability":0.05,"BitsStorage":{"ClassName":"SIMPLE_STRING"},"ColumnIds":[2]},` +
	`"Id":9,"StorageId":"__DEFAULT","ClassName":"BLOOM_FILTER","InheritPortionStorage":true,"Name":"bf2"}`

// localIndexFixture answers the reads of col_index_probe with before until the
// statement runs and with after from then on.
type localIndexFixture struct {
	before, after     []byte
	executed          bool
	events            []string
	statement         string
	executeFailure    bool
	monitoringUnnamed bool
}

func (f *localIndexFixture) DescribeColumnTable(_ context.Context, path string) (*native.ColumnTableDescribeResult, error) {
	f.events = append(f.events, "describe "+path)
	optional := &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}}}}
	return &native.ColumnTableDescribeResult{Self: &Ydb_Scheme.Entry{Name: "col_index_probe", Type: Ydb_Scheme.Entry_COLUMN_TABLE},
		StoreType: Ydb_Table.StoreType_STORE_TYPE_COLUMN, PrimaryKey: []string{"id"}, PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitionBy: []string{"id"}},
		Columns: []*native.ColumnMeta{{Name: "id", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UINT64}}}, {Name: "body", Type: optional}}}, nil
}

func (f *localIndexFixture) Close() error {
	f.events = append(f.events, "close")
	return nil
}

func (f *localIndexFixture) api() *nativeapi.ColumnTableLocalIndex {
	api := &nativeapi.ColumnTableLocalIndex{
		Open: func(context.Context) (nativeapi.ColumnDescriber, error) {
			f.events = append(f.events, "open")
			return f, nil
		},
		Execute: func(_ context.Context, statement string) error {
			f.events = append(f.events, "execute")
			f.statement, f.executed = statement, true
			return map[bool]error{true: errors.New("lost reply")}[f.executeFailure]
		},
		Monitoring: func(_ context.Context, path string) ([]byte, error) {
			f.events = append(f.events, "monitoring "+path)
			return map[bool][]byte{false: f.before, true: f.after}[f.executed], nil
		},
	}
	if f.monitoringUnnamed {
		api.Monitoring = nil
	}
	return api
}

// localIndexStep captures a local-index step of /local/col_index_probe with
// the given payload.
func localIndexStep(c *qt.C, payload string) *protocol.Envelope {
	c.Helper()
	model := tablecreate.ColumnLocalIndexModel()
	normalized, err := tablecreate.NormalizeColumnLocalIndex(&protocol.ValueInput{Id: "step", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: tablecreate.ColumnLocalIndexOperationKind, Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Name: "col_index_probe"},
		SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(payload)}})
	c.Assert(err, qt.IsNil)
	return normalized.Value
}

// The bloom index bf on body as the page describes it.
const heldBloomStep = `{"drop":{"name":"bf","columns":["body"],"storageId":"__DEFAULT","inheritPortionStorage":true,` +
	`"bloom":{"dataExtractor":"DEFAULT","bitsStorage":"SIMPLE_STRING","falsePositiveProbability":0.01}}}`

// TestColumnTableLocalIndexStep_HappyPath reads the table, runs the step's
// statement once, and confirms the local indexes the table reads back with.
func TestColumnTableLocalIndexStep_HappyPath(t *testing.T) {
	read := []string{"describe /local/col_index_probe", "monitoring /local/col_index_probe"}
	for _, test := range []struct {
		name, payload string
		after         func(*qt.C) func([]any) []any
		statement     string
	}{
		{"a removal", heldBloomStep, func(*qt.C) func([]any) []any { return withoutIndex("bf") },
			"ALTER TABLE `/local/col_index_probe` DROP INDEX `bf`;"},
		{"an addition", `{"add":{"name":"bf2","columns":["body"],"bloom":{"falsePositiveProbability":0.05}}}`,
			func(c *qt.C) func([]any) []any { return withIndex(c, addedBloomEntry) },
			"ALTER TABLE `/local/col_index_probe` ADD INDEX `bf2` LOCAL USING bloom_filter ON (`body`) WITH (false_positive_probability = 0.05);"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := &localIndexFixture{before: indexedPage(c, unchanged), after: indexedPage(c, test.after(c))}
			dispatched, err := fixture.api().Apply(t.Context(), "/local", localIndexStep(c, test.payload), func(context.Context) error {
				fixture.events = append(fixture.events, "locks")
				return nil
			})
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.statement, qt.Equals, test.statement)
			c.Assert(fixture.events, qt.DeepEquals, slices.Concat([]string{"open"}, read, []string{"locks", "execute"}, read, []string{"close"}))
		})
	}
}

func TestColumnTableLocalIndexStep_FailurePath(t *testing.T) {
	addBloom := `{"add":{"name":"bf2","columns":["body"],"bloom":{"falsePositiveProbability":0.05}}}`
	for _, test := range []struct {
		name, payload string
		before, after func(*qt.C) func([]any) []any
		modify        func(*localIndexFixture)
		wantErr       string
		dispatched    bool
	}{
		{"a removal of an index the table holds otherwise", `{"drop":{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.02}}}`,
			asRead, asRead, noChange, `YDB column table local index "bf" differs from the plan`, false},
		{"a removal of an index the table no longer holds", heldBloomStep,
			func(*qt.C) func([]any) []any { return withoutIndex("bf") }, asRead, noChange, `YDB column table local index "bf" differs from the plan`, false},
		{"an addition under a name the table holds", `{"add":{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.05}}}`,
			asRead, asRead, noChange, `YDB column table already holds local index "bf"`, false},
		{"an addition over a column the table does not hold", `{"add":{"name":"bf2","columns":["gone"],"bloom":{"falsePositiveProbability":0.05}}}`,
			asRead, asRead, noChange, `YDB local index column "gone" is absent`, false},
		{"an n-gram index over a number", `{"add":{"name":"ng2","columns":["id"],"ngram":{"falsePositiveProbability":0.1,"ngramSize":"3","caseSensitive":true}}}`,
			asRead, asRead, noChange, `YDB n-gram index column "id" must be Utf8`, false},
		{"no monitoring endpoint", heldBloomStep, asRead, asRead, func(f *localIndexFixture) { f.monitoringUnnamed = true },
			`creating a column table or changing its TTL or local indexes requires monitoring=.*`, false},
		{"a failed call", heldBloomStep, asRead, asRead, func(f *localIndexFixture) { f.executeFailure = true }, `lost reply`, true},
		{"an addition that does not read back", addBloom, asRead, asRead, noChange,
			`YDB column table local index "bf2" differs from the request after the change`, true},
		{"an addition that reads back otherwise", addBloom, asRead,
			func(c *qt.C) func([]any) []any {
				return withIndex(c, strings.Replace(addedBloomEntry, "0.05", "0.1", 1))
			}, noChange,
			`YDB column table local index "bf2" differs from the request after the change`, true},
		{"a removal that does not read back", heldBloomStep, asRead, asRead, noChange, `YDB column table still holds local index "bf" after the change`, true},
		{"a removal that takes another index along", heldBloomStep, asRead, func(*qt.C) func([]any) []any { return withoutIndex("bf", "ng") }, noChange,
			`YDB column table changed beyond local index "bf"`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := &localIndexFixture{before: indexedPage(c, test.before(c)), after: indexedPage(c, test.after(c))}
			test.modify(fixture)
			dispatched, err := fixture.api().Apply(t.Context(), "/local", localIndexStep(c, test.payload), func(context.Context) error { return nil })
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(fixture.executed, qt.Equals, test.dispatched)
		})
	}
}

// TestColumnTableLocalIndexStep_RefusesAnotherDatabase refuses a step whose
// owner names another database than the session's before it reads anything.
func TestColumnTableLocalIndexStep_RefusesAnotherDatabase(t *testing.T) {
	c := testcheck.New(t)
	fixture := &localIndexFixture{before: indexedPage(c, unchanged), after: indexedPage(c, unchanged)}
	step := localIndexStep(c, heldBloomStep)
	step = proto.CloneOf(step)
	step.Owner.Database = "/other"
	dispatched, err := fixture.api().Apply(t.Context(), "/local", step, func(context.Context) error { return nil })
	c.Assert(err, qt.ErrorMatches, `YDB column table local-index change targets another database`)
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(fixture.events, qt.HasLen, 0)
}
