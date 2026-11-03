package nativeapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	embColumn = `{"name":"emb","type":{"optionalType":{"item":{"typeId":"STRING"}}}}`
	// vectorSettings is a vector index's settings, as declared and as read back.
	vectorSettings = `"vectorSettings":{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"levels":1,"clusters":2}`
	// vectorDeclared is the vector index an addition adds.
	vectorDeclared = `{"name":"by_emb","indexColumns":["emb"],"globalVectorKmeansTreeIndex":{` + vectorSettings + `}}`
	// vectorRead is that index as the server reports it: ready, with the
	// partitioning of each table that implements it.
	vectorRead = `{"name":"by_emb","indexColumns":["emb"],"status":"STATUS_READY","globalVectorKmeansTreeIndex":{` + vectorSettings +
		`,"levelTableSettings":{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}}}}`
	vectorBuilding = `{"name":"by_emb","indexColumns":["emb"],"status":"STATUS_BUILDING","globalVectorKmeansTreeIndex":{` + vectorSettings + `}}`
	// vectorOther is by_emb built with other clusters than the addition states.
	vectorOther = `{"name":"by_emb","indexColumns":["emb"],"status":"STATUS_READY","globalVectorKmeansTreeIndex":{"vectorSettings":` +
		`{"settings":{"metric":"DISTANCE_COSINE","vectorType":"VECTOR_TYPE_FLOAT","vectorDimension":3},"levels":1,"clusters":4}}}`
	addVectorSQL = "ALTER TABLE `/local/app/records` ADD INDEX `by_emb` GLOBAL USING vector_kmeans_tree ON (`emb`) " +
		"WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2);"
)

// vectorTable is app.records with an id, a label and a vector column, and
// the indexes given.
func vectorTable(indexes ...string) string {
	text := ""
	for i, index := range indexes {
		if i > 0 {
			text += ","
		}
		text += index
	}
	return withColumns(`,"indexes":[`+text+`]`, labelCol, embColumn)
}

// indexFixture serves the table read before the statement and the reads
// after it, in order, the last one repeating, and records each call.
type indexFixture struct {
	events   []string
	before   string
	after    []string
	reads    int
	executed []string
	failure  error
}

func (f *indexFixture) Describe(_ context.Context, path string) (*native.DescribeTableResult, error) {
	f.events = append(f.events, "describe")
	if path != "/local/app/records" {
		return nil, errors.New("wrong describe path")
	}
	text := f.before
	if f.reads > 0 {
		text = f.after[min(f.reads-1, len(f.after)-1)]
	}
	f.reads++
	var result native.DescribeTableResult
	if err := protojson.Unmarshal([]byte(text), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (f *indexFixture) Close() error {
	f.events = append(f.events, "close")
	return nil
}

// applyNativeIndex adds the index spec states to the fixture's table.
func applyNativeIndex(c *qt.C, fixture *indexFixture, spec string) (bool, error) {
	c.Helper()
	var index native.IndexIntent
	c.Assert(protojson.Unmarshal([]byte(spec), &index), qt.IsNil)
	captured, err := tablecreate.CaptureNativeIndex("add", &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}, &index)
	c.Assert(err, qt.IsNil)
	api := &nativeapi.NativeIndexAddition{
		Open: func(context.Context) (nativeapi.NativeIndexReader, error) {
			fixture.events = append(fixture.events, "open")
			return fixture, nil
		},
		Execute: func(_ context.Context, statement string) error {
			fixture.events = append(fixture.events, "execute")
			fixture.executed = append(fixture.executed, statement)
			return fixture.failure
		},
		Poll: time.Millisecond, Timeout: time.Second,
	}
	original := proto.CloneOf(captured.Value)
	dispatched, err := api.Apply(c.Context(), "/local", captured.Value, func(context.Context) error {
		fixture.events = append(fixture.events, "guard")
		return nil
	})
	c.Assert(proto.Equal(original, captured.Value), qt.IsTrue)
	return dispatched, err
}

// An addition runs its statement once and confirms the index the server
// reports, whatever it reports for the tables that implement it, after
// waiting while the index is being built.
func TestNativeIndexAddition_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		before string
		after  []string
		events []string
	}{
		{"adds a vector index", vectorTable(), []string{vectorTable(vectorRead)},
			[]string{"open", "describe", "guard", "execute", "describe", "close"}},
		{"keeps the indexes the table holds", vectorTable(byLabelIndex), []string{vectorTable(byLabelIndex, vectorRead)},
			[]string{"open", "describe", "guard", "execute", "describe", "close"}},
		{"waits while the index is being built", vectorTable(), []string{vectorTable(vectorBuilding), vectorTable(vectorRead)},
			[]string{"open", "describe", "guard", "execute", "describe", "describe", "close"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := &indexFixture{before: test.before, after: test.after}
			dispatched, err := applyNativeIndex(c, fixture, vectorDeclared)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.events, qt.DeepEquals, test.events)
			c.Assert(fixture.executed, qt.DeepEquals, []string{addVectorSQL})
		})
	}
}

// A table that does not hold what the plan captured refuses the statement
// before it is sent.
func TestNativeIndexAddition_PreconditionFailurePath(t *testing.T) {
	for _, test := range []struct {
		name, before, message string
	}{
		{"the index exists", vectorTable(vectorRead), `YDB index to add already exists`},
		{"the vector column is gone", withColumns("", labelCol), `YDB index to add reads the absent column "emb"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := &indexFixture{before: test.before}
			dispatched, err := applyNativeIndex(c, fixture, vectorDeclared)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(fixture.executed, qt.HasLen, 0)
		})
	}
}

// A table read back without the index as requested, or with something else
// changed, leaves the outcome as a failure after dispatch.
func TestNativeIndexAddition_ConfirmationFailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		before  string
		after   string
		failure error
		message string
	}{
		{"the index has other settings", vectorTable(), vectorTable(vectorOther), nil, `YDB added index differs from the request`},
		{"the index is absent", vectorTable(), vectorTable(), nil, `YDB altered table holds unexpected indexes`},
		{"another index changed", vectorTable(byLabelIndex), vectorTable(asyncByLabel, vectorRead), nil, `YDB altered table changed the index "by_label"`},
		{"the statement failed", vectorTable(), vectorTable(), errors.New("lost reply"), `lost reply`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := &indexFixture{before: test.before, after: []string{test.after}, failure: test.failure}
			dispatched, err := applyNativeIndex(c, fixture, vectorDeclared)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.executed, qt.HasLen, 1)
		})
	}
}
