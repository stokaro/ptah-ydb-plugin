package rowdata_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
)

// The corpus holds row cases and what the host's embedded YDB engine
// answered for them, recorded by testdata/oracle at one host commit:
//
//	type-*   internal/ydbdata.Rows.TypeRows: the column types and the
//	         canonical declared and live rows, or the refusal
//	write-*  migration/datadiff with the YDB data services: the statement of
//	         each write, or the refusal
//
// A value is a JSON object with one key naming its case, as the contract's
// DataValue does; see testdata/README.md.

const corpusDirectory = "testdata/corpus"

// tagged is one value of the corpus.
type tagged map[string]json.RawMessage

type corpusColumn struct {
	Name         string `json:"name"`
	DeclaredType string `json:"declared_type"`
	Type         string `json:"type"`
}

type typeCase struct {
	Capabilities map[string]bool `json:"capabilities"`
	LiveTable    []corpusColumn  `json:"live_table"`
	Columns      []corpusColumn  `json:"columns"`
	Declared     [][]tagged      `json:"declared"`
	Live         [][]tagged      `json:"live"`
}

type typeGolden struct {
	HostCommit string         `json:"host_commit"`
	Accepted   bool           `json:"accepted"`
	Error      string         `json:"error"`
	Columns    []corpusColumn `json:"columns"`
	Declared   [][]tagged     `json:"declared"`
	Live       [][]tagged     `json:"live"`
}

type typeRow struct {
	name   string
	input  typeCase
	golden typeGolden
}

// typeCorpus lists the type cases the host accepted, or the ones it refused,
// in name order.
func typeCorpus(c *qt.C, accepted bool) []typeRow {
	c.Helper()
	var rows []typeRow
	for _, name := range corpusNames(c, "type-") {
		var row typeRow
		row.name = name
		readJSON(c, filepath.Join(corpusDirectory, name+".json"), &row.input)
		readJSON(c, filepath.Join(corpusDirectory, name+".golden.json"), &row.golden)
		if row.golden.Accepted == accepted {
			rows = append(rows, row)
		}
	}
	return rows
}

func corpusNames(c *qt.C, prefix string) []string {
	c.Helper()
	paths, err := filepath.Glob(filepath.Join(corpusDirectory, prefix+"*.golden.json"))
	c.Assert(err, qt.IsNil)
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(path), ".golden.json"))
	}
	slices.Sort(names)
	return names
}

func readJSON(c *qt.C, path string, target any) {
	c.Helper()
	data, err := os.ReadFile(path)
	c.Assert(err, qt.IsNil)
	c.Assert(json.Unmarshal(data, target), qt.IsNil)
}

// value is the neutral value a corpus value stands for.
func value(c *qt.C, tag tagged) *protocol.DataValue {
	c.Helper()
	if len(tag) == 0 {
		return &protocol.DataValue{}
	}
	c.Assert(tag, qt.HasLen, 1)
	encoded, err := json.Marshal(map[string]json.RawMessage(protojsonForm(c, tag)))
	c.Assert(err, qt.IsNil)
	var result protocol.DataValue
	c.Assert(protojson.Unmarshal(encoded, &result), qt.IsNil)
	return &result
}

// protojsonForm renames the corpus cases to the JSON names of DataValue.
func protojsonForm(c *qt.C, tag tagged) tagged {
	c.Helper()
	names := map[string]string{
		"null": "null", "bool": "boolValue", "int64": "int64Value", "uint64": "uint64Value", "double": "doubleValue",
		"string": "stringValue", "bytes": "bytesValue", "decimal": "decimalValue", "timestamp": "timestampValue",
		"date": "dateValue", "interval": "intervalValue", "json": "jsonValue", "native": "nativeValue",
	}
	out := tagged{}
	for name, raw := range tag {
		renamed, known := names[name]
		c.Assert(known, qt.IsTrue, qt.Commentf("corpus case %q", name))
		out[renamed] = raw
		if name == "null" {
			out[renamed] = json.RawMessage("{}")
		}
	}
	return out
}

// corpusValue writes a neutral value in the corpus form.
func corpusValue(c *qt.C, value *protocol.DataValue) tagged {
	c.Helper()
	marshal := func(v any) json.RawMessage {
		out, err := json.Marshal(v)
		c.Assert(err, qt.IsNil)
		return out
	}
	switch v := value.GetValue().(type) {
	case nil:
		return tagged{}
	case *protocol.DataValue_Null:
		return tagged{"null": marshal(true)}
	case *protocol.DataValue_BoolValue:
		return tagged{"bool": marshal(v.BoolValue)}
	case *protocol.DataValue_Int64Value:
		return tagged{"int64": marshal(strconv.FormatInt(v.Int64Value, 10))}
	case *protocol.DataValue_Uint64Value:
		return tagged{"uint64": marshal(strconv.FormatUint(v.Uint64Value, 10))}
	case *protocol.DataValue_DoubleValue:
		return tagged{"double": marshal(v.DoubleValue)}
	case *protocol.DataValue_StringValue:
		return tagged{"string": marshal(v.StringValue)}
	case *protocol.DataValue_BytesValue:
		return tagged{"bytes": marshal(base64.StdEncoding.EncodeToString(v.BytesValue))}
	case *protocol.DataValue_JsonValue:
		return tagged{"json": marshal(v.JsonValue)}
	case *protocol.DataValue_DecimalValue:
		return tagged{"decimal": marshal(map[string]any{"digits": v.DecimalValue.GetDigits(),
			"precision": v.DecimalValue.GetPrecision(), "scale": v.DecimalValue.GetScale()})}
	case *protocol.DataValue_TimestampValue:
		return tagged{"timestamp": marshal(map[string]int64{"seconds": v.TimestampValue.GetSeconds(), "nanos": int64(v.TimestampValue.GetNanos())})}
	case *protocol.DataValue_DateValue:
		return tagged{"date": marshal(v.DateValue)}
	case *protocol.DataValue_IntervalValue:
		return tagged{"interval": marshal(map[string]string{"nanoseconds": strconv.FormatInt(v.IntervalValue.GetNanoseconds(), 10)})}
	default:
		c.Fatalf("no corpus form for %T", v)
		return nil
	}
}

// canonicalTags reads tagged values back through JSON, so two spellings of
// one JSON object compare equal.
func canonicalTags(c *qt.C, rows [][]tagged) [][]any {
	c.Helper()
	if len(rows) == 0 {
		// The golden files leave out an empty row set.
		return nil
	}
	encoded, err := json.Marshal(rows)
	c.Assert(err, qt.IsNil)
	var out [][]any
	c.Assert(json.Unmarshal(encoded, &out), qt.IsNil)
	return out
}

func typeRequest(c *qt.C, input typeCase) *protocol.TypeRowsRequest {
	c.Helper()
	request := &protocol.TypeRowsRequest{Engine: "ydb", Capabilities: input.Capabilities, TargetFeatures: backend.Description().GetFeatures(),
		Table: &protocol.Reference{Kind: "table", Name: "t"}}
	if input.LiveTable != nil {
		request.LiveTable = &protocol.LiveTable{}
	}
	for _, column := range input.LiveTable {
		request.LiveTable.Columns = append(request.LiveTable.Columns, &protocol.Column{Name: column.Name, Type: column.Type})
	}
	for _, column := range input.Columns {
		request.Columns = append(request.Columns, &protocol.RowColumn{Name: column.Name, DeclaredType: column.DeclaredType})
	}
	rows := func(input [][]tagged) []*protocol.Row {
		out := make([]*protocol.Row, 0, len(input))
		for _, row := range input {
			values := make([]*protocol.DataValue, len(row))
			for i, tag := range row {
				values[i] = value(c, tag)
			}
			out = append(out, &protocol.Row{Values: values})
		}
		return out
	}
	request.Declared, request.Live = rows(input.Declared), rows(input.Live)
	return request
}

func corpusRows(c *qt.C, rows []*protocol.Row) [][]tagged {
	c.Helper()
	out := make([][]tagged, len(rows))
	for i, row := range rows {
		out[i] = make([]tagged, len(row.GetValues()))
		for j, value := range row.GetValues() {
			out[i][j] = corpusValue(c, value)
		}
	}
	return out
}

func TestTypeRowsMatchesTheHost_HappyPath(t *testing.T) {
	c := qt.New(t)
	rows := typeCorpus(c, true)
	c.Assert(len(rows) >= 25, qt.IsTrue, qt.Commentf("%d accepted type cases", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			request := typeRequest(c, row.input)

			result, err := rowdata.TypeRows(request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateTypedRows(backend.Description(), request, result), qt.IsNil)
			columns := make([]corpusColumn, len(result.GetColumns()))
			for i, column := range result.GetColumns() {
				columns[i] = corpusColumn{Name: column.GetName(), Type: column.GetType()}
			}
			c.Assert(columns, qt.DeepEquals, row.golden.Columns)
			c.Assert(canonicalTags(c, corpusRows(c, result.GetDeclared())), qt.DeepEquals, canonicalTags(c, row.golden.Declared))
			c.Assert(canonicalTags(c, corpusRows(c, result.GetLive())), qt.DeepEquals, canonicalTags(c, row.golden.Live))
		})
	}
}

func TestTypeRowsMatchesTheHost_FailurePath(t *testing.T) {
	c := qt.New(t)
	rows := typeCorpus(c, false)
	c.Assert(len(rows) >= 32, qt.IsTrue, qt.Commentf("%d refused type cases", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)

			result, err := rowdata.TypeRows(typeRequest(c, row.input))

			c.Assert(err, qt.ErrorMatches, regexp.QuoteMeta(row.golden.Error))
			c.Assert(result, qt.IsNil)
		})
	}
}

type corpusWrite struct {
	ChangeID    string            `json:"change_id"`
	Mode        string            `json:"mode"`
	Schema      string            `json:"schema"`
	Table       string            `json:"table"`
	Columns     []string          `json:"columns"`
	KeyColumns  []string          `json:"key_columns"`
	Values      []tagged          `json:"values"`
	ColumnTypes map[string]string `json:"column_types"`
}

type writeAnswer struct {
	ChangeID   string `json:"change_id"`
	Statement  string `json:"statement"`
	Error      string `json:"error"`
	Unrendered bool   `json:"unrendered"`
}

type writeRow struct {
	name    string
	writes  []corpusWrite
	answers []writeAnswer
}

// writeCorpus lists the write cases the host rendered whole, or the ones in
// which it refused a write, in name order.
func writeCorpus(c *qt.C, rendered bool) []writeRow {
	c.Helper()
	var rows []writeRow
	for _, name := range corpusNames(c, "write-") {
		var input struct {
			Writes []corpusWrite `json:"writes"`
		}
		var golden struct {
			Writes []writeAnswer `json:"writes"`
		}
		readJSON(c, filepath.Join(corpusDirectory, name+".json"), &input)
		readJSON(c, filepath.Join(corpusDirectory, name+".golden.json"), &golden)
		refused := slices.ContainsFunc(golden.Writes, func(answer writeAnswer) bool { return answer.Error != "" })
		if refused != rendered {
			rows = append(rows, writeRow{name: name, writes: input.Writes, answers: golden.Writes})
		}
	}
	return rows
}

func writeRequest(c *qt.C, writes []corpusWrite) *protocol.RenderRowWritesRequest {
	c.Helper()
	request := &protocol.RenderRowWritesRequest{Engine: "ydb", TargetFeatures: backend.Description().GetFeatures()}
	for _, write := range writes {
		values := make([]*protocol.DataValue, len(write.Values))
		for i, tag := range write.Values {
			values[i] = value(c, tag)
		}
		request.Writes = append(request.Writes, &protocol.RowWrite{
			ChangeId: write.ChangeID, Mode: protocol.RowWrite_Mode(protocol.RowWrite_Mode_value[write.Mode]),
			Table:   &protocol.Reference{Kind: "table", Schema: write.Schema, Name: write.Table},
			Columns: write.Columns, KeyColumns: write.KeyColumns, Values: values, ColumnTypes: write.ColumnTypes,
		})
	}
	return request
}

// expectedStatement is the host's statement for a write, and for an UPSERT,
// which the host's row diff does not write, the host's INSERT of the same row
// with YQL's UPSERT INTO in its place.
func expectedStatement(answer writeAnswer) string {
	if answer.Unrendered {
		return "UPSERT" + strings.TrimPrefix(answer.Statement, "INSERT")
	}
	return answer.Statement
}

func TestRenderRowWritesMatchesTheHost_HappyPath(t *testing.T) {
	c := qt.New(t)
	rows := writeCorpus(c, true)
	c.Assert(len(rows) >= 10, qt.IsTrue, qt.Commentf("%d rendered write cases", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			request := writeRequest(c, row.writes)

			result, err := rowdata.RenderRowWrites(request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateRenderedRowWrites(backend.Description(), request, result), qt.IsNil)
			statements := make([]string, len(result.GetUnits()))
			for i, unit := range result.GetUnits() {
				statements[i] = unit.GetSql()
			}
			expected := make([]string, len(row.answers))
			for i, answer := range row.answers {
				expected[i] = expectedStatement(answer)
			}
			c.Assert(statements, qt.DeepEquals, expected)
		})
	}
}

// valueText is the value a host refusal names, which a provider refusal
// never does.
var valueText = regexp.MustCompile(`cannot hold ("(?:[^"\\]|\\.)*"|\S+): `)

// refusalMessage is the host's refusal as this provider words it: without
// the value it names, and about a write rather than a diff.
func refusalMessage(host string) string {
	message := strings.TrimPrefix(host, "datadiff: ")
	message = valueText.ReplaceAllString(message, "cannot hold the value: ")
	return strings.Replace(message, "the diff names no type", "the write names no type", 1)
}

func TestRenderRowWritesMatchesTheHost_FailurePath(t *testing.T) {
	c := qt.New(t)
	rows := writeCorpus(c, false)
	c.Assert(len(rows) >= 6, qt.IsTrue, qt.Commentf("%d refused write cases", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			request := writeRequest(c, row.writes)
			first := row.answers[slices.IndexFunc(row.answers, func(answer writeAnswer) bool { return answer.Error != "" })]

			result, err := rowdata.RenderRowWrites(request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateRenderedRowWrites(backend.Description(), request, result), qt.IsNil)
			c.Assert(result.GetUnits(), qt.HasLen, 0)
			c.Assert(result.GetRefusal().GetChangeId(), qt.Equals, first.ChangeID)
			c.Assert(result.GetRefusal().GetReason(), qt.Equals, protocol.RenderRefusal_UNSUPPORTED_VALUE)
			c.Assert(result.GetRefusal().GetMessage(), qt.Equals, refusalMessage(first.Error))
		})
	}
}

// TestCorpusNamesOneHostCommit pins that every answer was recorded at one host
// commit, so the corpus is one measurement.
func TestCorpusNamesOneHostCommit(t *testing.T) {
	c := qt.New(t)
	paths, err := filepath.Glob(filepath.Join(corpusDirectory, "*.golden.json"))
	c.Assert(err, qt.IsNil)
	paths = append(paths, "testdata/query-dialect.golden.json")
	commits := map[string]bool{}
	for _, path := range paths {
		var golden struct {
			HostCommit string `json:"host_commit"`
		}
		readJSON(c, path, &golden)
		commits[golden.HostCommit] = true
	}
	c.Assert(len(paths) >= 70, qt.IsTrue, qt.Commentf("%d golden files", len(paths)))
	c.Assert(commits, qt.HasLen, 1)
}

// TestQueryDialectMatchesTheHost pins the query-builder spelling the provider
// declares to the host's YDB descriptor and the default capabilities the host
// assumes for YDB.
func TestQueryDialectMatchesTheHost(t *testing.T) {
	c := qt.New(t)
	var golden struct {
		Placeholder string `json:"placeholder"`
		Quoting     struct {
			Style    string   `json:"style"`
			Bare     string   `json:"bare"`
			Keywords []string `json:"keywords"`
			Paths    bool     `json:"paths"`
		} `json:"quoting"`
		Query struct {
			ChecksCapabilities bool     `json:"checks_capabilities"`
			UnsupportedJoins   []string `json:"unsupported_joins"`
			OffsetFetch        bool     `json:"offset_fetch"`
			FetchOrderBy       string   `json:"fetch_order_by"`
			OffsetOnlyLimit    string   `json:"offset_only_limit"`
			UnsignedBounds     bool     `json:"unsigned_bounds"`
			Upsert             string   `json:"upsert"`
			UpsertRefusal      string   `json:"upsert_refusal"`
			UpdateRefusal      string   `json:"update_refusal"`
		} `json:"query"`
		DefaultCapabilities map[string]bool `json:"default_capabilities"`
	}
	readJSON(c, "testdata/query-dialect.golden.json", &golden)

	declared := backend.Description().GetEngines()[0].GetQueryDialect()

	c.Assert(declared.GetPlaceholder(), qt.Equals, golden.Placeholder)
	c.Assert(declared.GetQuoting(), qt.CmpEquals(protocmp.Transform()), &protocol.QueryQuoting{
		Style: golden.Quoting.Style, Bare: golden.Quoting.Bare, Keywords: golden.Quoting.Keywords, Paths: golden.Quoting.Paths,
	})
	c.Assert(declared.GetQuery(), qt.CmpEquals(protocmp.Transform()), &protocol.QuerySpelling{
		ChecksCapabilities: golden.Query.ChecksCapabilities, UnsupportedJoins: golden.Query.UnsupportedJoins,
		OffsetFetch: golden.Query.OffsetFetch, FetchOrderBy: golden.Query.FetchOrderBy, OffsetOnlyLimit: golden.Query.OffsetOnlyLimit,
		UnsignedBounds: golden.Query.UnsignedBounds, Upsert: golden.Query.Upsert, UpsertRefusal: golden.Query.UpsertRefusal,
		UpdateRefusal: golden.Query.UpdateRefusal,
	})
	c.Assert(declared.GetDefaultCapabilities(), qt.DeepEquals, golden.DefaultCapabilities)
	c.Assert(provider.ValidateQueryDialect(declared), qt.IsNil)
}
