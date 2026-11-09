//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	providerruntime "ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A host that lists sessions.typed-arguments sends each statement argument
// with the type its caller bound it at, and the provider binds it at that YDB
// type. The expected types are what YDB reports for the parameter, and the
// expected rows what YDB stored, read back past the provider.

func typedArgument(scalar protocol.ArgumentType_Scalar, optional bool, value *protocol.DataValue) *protocol.StatementArgument {
	return &protocol.StatementArgument{Type: &protocol.ArgumentType{Scalar: scalar, Optional: optional}, Value: value}
}

func typedList(scalar protocol.ArgumentType_Scalar, optional bool, elements ...*protocol.DataValue) *protocol.StatementArgument {
	return &protocol.StatementArgument{Type: &protocol.ArgumentType{Scalar: scalar, Optional: optional, List: true}, Elements: elements}
}

func signedValue(v int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: v}}
}

func unsignedValue(v uint64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Uint64Value{Uint64Value: v}}
}

func textValue(v string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: v}}
}

func doubleValue(v float64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: v}}
}

var nullValue = &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}

// typedSession opens a session of a provider whose host lists typed
// arguments.
func typedSession(c *qt.C) (*providerruntime.Client, string) {
	c.Helper()
	client := startProviderFor(c, provider.TypedArgumentsFeature)
	c.Assert(client.Description().GetFeatures(), qt.Contains, provider.TypedArgumentsFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return client, session.Id
}

// runTyped runs one read-write statement with typed arguments.
func runTyped(c *qt.C, client *providerruntime.Client, session, text string, arguments ...*protocol.StatementArgument) *protocol.StatementResult {
	c.Helper()
	result, err := client.RunStatement(c.Context(), &protocol.RunStatementRequest{SessionId: session, RequestId: text, Sql: text, TypedArguments: arguments,
		Access: protocol.RunStatementRequest_READ_WRITE, MaxRows: 10})
	c.Assert(err, qt.IsNil)
	return result
}

// Each scalar binds at its own YDB type, an optional type as Optional<T> with
// a typed null, a list as List<T> of its elements, and an untyped null as
// Void.
func TestTypedStatementArgumentsBindAtTheirTypeSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client, session := typedSession(c)
	for _, test := range []struct {
		name     string
		argument *protocol.StatementArgument
		value    string
		wantType string
		want     *protocol.DataValue
	}{
		{name: "an int8", argument: typedArgument(protocol.ArgumentType_INT8, false, signedValue(math.MinInt8)), value: "CAST($p1 AS Utf8)",
			wantType: "Int8", want: textValue("-128")},
		{name: "an int16", argument: typedArgument(protocol.ArgumentType_INT16, false, signedValue(math.MaxInt16)), value: "CAST($p1 AS Utf8)",
			wantType: "Int16", want: textValue("32767")},
		{name: "an int32", argument: typedArgument(protocol.ArgumentType_INT32, false, signedValue(-5)), value: "CAST($p1 AS Utf8)",
			wantType: "Int32", want: textValue("-5")},
		{name: "an int64", argument: typedArgument(protocol.ArgumentType_INT64, false, signedValue(math.MinInt64)), value: "CAST($p1 AS Utf8)",
			wantType: "Int64", want: textValue("-9223372036854775808")},
		{name: "a uint8", argument: typedArgument(protocol.ArgumentType_UINT8, false, unsignedValue(math.MaxUint8)), value: "CAST($p1 AS Utf8)",
			wantType: "Uint8", want: textValue("255")},
		{name: "a uint16", argument: typedArgument(protocol.ArgumentType_UINT16, false, unsignedValue(math.MaxUint16)), value: "CAST($p1 AS Utf8)",
			wantType: "Uint16", want: textValue("65535")},
		{name: "a uint32", argument: typedArgument(protocol.ArgumentType_UINT32, false, unsignedValue(math.MaxUint32)), value: "CAST($p1 AS Utf8)",
			wantType: "Uint32", want: textValue("4294967295")},
		{name: "a uint64", argument: typedArgument(protocol.ArgumentType_UINT64, false, unsignedValue(math.MaxUint64)), value: "CAST($p1 AS Utf8)",
			wantType: "Uint64", want: textValue("18446744073709551615")},
		{name: "a float32", argument: typedArgument(protocol.ArgumentType_FLOAT32, false, doubleValue(1.5)), value: "CAST($p1 AS Utf8)",
			wantType: "Float", want: textValue("1.5")},
		{name: "a float64", argument: typedArgument(protocol.ArgumentType_FLOAT64, false, doubleValue(0.25)), value: "CAST($p1 AS Utf8)",
			wantType: "Double", want: textValue("0.25")},
		{name: "a bool", argument: typedArgument(protocol.ArgumentType_BOOL, false, &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: true}}),
			value: "CAST($p1 AS Utf8)", wantType: "Bool", want: textValue("true")},
		{name: "a string", argument: typedArgument(protocol.ArgumentType_STRING, false, textValue("CZ")), value: "$p1", wantType: "Utf8", want: textValue("CZ")},
		{name: "bytes", argument: typedArgument(protocol.ArgumentType_BYTES, false, &protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: []byte("CZ")}}),
			value: "CAST($p1 AS Utf8)", wantType: "String", want: textValue("CZ")},
		{name: "a timestamp", argument: typedArgument(protocol.ArgumentType_TIMESTAMP, false, &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{
			TimestampValue: &protocol.TimestampValue{Seconds: 1, Nanos: 2000}}}), value: "CAST($p1 AS Utf8)", wantType: "Timestamp",
			want: textValue("1970-01-01T00:00:01.000002Z")},
		{name: "an interval", argument: typedArgument(protocol.ArgumentType_INTERVAL, false, &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{
			IntervalValue: &protocol.IntervalValue{Nanoseconds: 90000000000}}}), value: "CAST($p1 AS Int64)", wantType: "Interval", want: signedValue(90000000)},
		{name: "an optional uint64", argument: typedArgument(protocol.ArgumentType_UINT64, true, unsignedValue(7)), value: "CAST($p1 AS Utf8)",
			wantType: "Optional<Uint64>", want: textValue("7")},
		{name: "a null of an optional utf8", argument: typedArgument(protocol.ArgumentType_STRING, true, nullValue), value: "$p1",
			wantType: "Optional<Utf8>", want: nullValue},
		{name: "a null of an optional int32", argument: typedArgument(protocol.ArgumentType_INT32, true, nullValue), value: "CAST($p1 AS Utf8)",
			wantType: "Optional<Int32>", want: nullValue},
		{name: "an untyped null", argument: typedArgument(protocol.ArgumentType_UNTYPED, false, nullValue), value: "CAST(NULL AS Utf8)",
			wantType: "Void", want: nullValue},
		{name: "a list of uint64", argument: typedList(protocol.ArgumentType_UINT64, false, unsignedValue(1), unsignedValue(2)),
			value: "CAST(ListHead($p1) AS Utf8)", wantType: "List<Uint64>", want: textValue("1")},
		{name: "a list of optional int16", argument: typedList(protocol.ArgumentType_INT16, true, nullValue, signedValue(3)),
			value: "CAST(ListLength($p1) AS Utf8)", wantType: "List<Int16?>", want: textValue("2")},
		{name: "an empty list of utf8", argument: typedList(protocol.ArgumentType_STRING, false),
			value: "CAST(ListLength($p1) AS Utf8)", wantType: "List<Utf8>", want: textValue("0")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result := runTyped(c, client, session, "SELECT CAST(FormatType(TypeOf($p1)) AS Utf8) AS bound, "+test.value+" AS value", test.argument)
			c.Assert(result.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED, qt.Commentf("%s", result.Error))
			c.Assert(result.Rows, qt.HasLen, 1)
			c.Assert(result.Rows[0].Values[0].GetStringValue(), qt.Equals, test.wantType)
			c.Assert(result.Rows[0].Values[1], qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

// Typed arguments write the narrow and nullable columns of a table at their
// own types, a List<Uint64> selects and deletes rows, and an Int64 written
// into an Int32 column is refused by YDB, which the typed form exists to
// avoid.
func TestTypedStatementArgumentsWriteNarrowColumnsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "typed_arguments")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, small Int32, tiny Uint8, label Utf8, ratio Float, PRIMARY KEY(id))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+table+"`")
		c.Check(err, qt.IsNil)
	})
	client, session := typedSession(c)
	upsert := "UPSERT INTO `" + table + "` (id, small, tiny, label, ratio) VALUES ($p1, $p2, $p3, $p4, $p5)"

	written := runTyped(c, client, session, upsert, typedArgument(protocol.ArgumentType_UINT64, false, unsignedValue(1)),
		typedArgument(protocol.ArgumentType_INT32, false, signedValue(-5)), typedArgument(protocol.ArgumentType_UINT8, true, unsignedValue(7)),
		typedArgument(protocol.ArgumentType_STRING, true, textValue("CZ")), typedArgument(protocol.ArgumentType_FLOAT32, false, doubleValue(1.5)))
	nulls := runTyped(c, client, session, upsert, typedArgument(protocol.ArgumentType_UINT64, false, unsignedValue(2)),
		typedArgument(protocol.ArgumentType_INT32, true, nullValue), typedArgument(protocol.ArgumentType_UINT8, true, nullValue),
		typedArgument(protocol.ArgumentType_STRING, true, nullValue), typedArgument(protocol.ArgumentType_FLOAT32, true, nullValue))
	wide := runTyped(c, client, session, "UPSERT INTO `"+table+"` (id, small) VALUES ($p1, $p2)",
		typedArgument(protocol.ArgumentType_UINT64, false, unsignedValue(3)), typedArgument(protocol.ArgumentType_INT64, false, signedValue(5)))
	selected := runTyped(c, client, session, "SELECT id FROM `"+table+"` WHERE id IN $p1 ORDER BY id",
		typedList(protocol.ArgumentType_UINT64, false, unsignedValue(1), unsignedValue(2), unsignedValue(9)))

	c.Assert(written.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED, qt.Commentf("%s", written.Error))
	c.Assert(nulls.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED, qt.Commentf("%s", nulls.Error))
	c.Assert(wide.Outcome, qt.Equals, protocol.StatementResult_FAILED)
	c.Assert(wide.Error, qt.Matches, `(?s).*Int64 to Optional<Int32>.*`)
	c.Assert(selected.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED, qt.Commentf("%s", selected.Error))
	c.Assert(selected.Rows, qt.HasLen, 2)
	c.Assert(selected.Rows[0].Values[0].GetUint64Value(), qt.Equals, uint64(1))
	c.Assert(selected.Rows[1].Values[0].GetUint64Value(), qt.Equals, uint64(2))
	var (
		small sql.NullInt32
		tiny  sql.Null[uint8]
		label sql.NullString
		ratio sql.Null[float32]
	)
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT small, tiny, label, ratio FROM `"+table+"` WHERE id = 1").Scan(&small, &tiny, &label, &ratio), qt.IsNil)
	c.Assert([]any{small, tiny, label, ratio}, qt.DeepEquals, []any{sql.NullInt32{Int32: -5, Valid: true}, sql.Null[uint8]{V: 7, Valid: true},
		sql.NullString{String: "CZ", Valid: true}, sql.Null[float32]{V: 1.5, Valid: true}})
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT small, tiny, label, ratio FROM `"+table+"` WHERE id = 2").Scan(&small, &tiny, &label, &ratio), qt.IsNil)
	c.Assert([]any{small, tiny, label, ratio}, qt.DeepEquals, []any{sql.NullInt32{}, sql.Null[uint8]{}, sql.NullString{}, sql.Null[float32]{}})
	assertRowCount(c, opened, table, 2)

	deleted := runTyped(c, client, session, "DELETE FROM `"+table+"` WHERE id IN $p1", typedList(protocol.ArgumentType_UINT64, false, unsignedValue(1)))
	c.Assert(deleted.Outcome, qt.Equals, protocol.StatementResult_SUCCEEDED, qt.Commentf("%s", deleted.Error))
	assertRowCount(c, opened, table, 1)
}
