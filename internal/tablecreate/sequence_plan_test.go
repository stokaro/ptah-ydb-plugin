package tablecreate_test

import (
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
)

// serialChange is an existing app.records whose key id is held and declared
// as given. heldStart and heldIncrement are the native read of the sequence,
// empty for no sequence; maximum is where it ends.
type serialChange struct {
	desired                  *protocol.DeclaredColumn
	held                     *protocol.ObservedColumn
	heldStart, heldIncrement string
	maximum                  int64
	caps                     map[string]bool
	policy                   *protocol.PlanningPolicy
	// added is a further declared column the table does not hold.
	added *protocol.DeclaredColumn
}

// heldSerial is the read of an Int64 Serial key that starts at start and steps
// by increment, as inspection reports it.
func heldSerial(start, increment string) *protocol.ObservedColumn {
	return &protocol.ObservedColumn{DataType: "Int64", IsNullable: "NO", PrimaryKey: true, AutoIncrement: true,
		IdentityStart: start, IdentityIncrement: increment}
}

func serialChangeRequest(c *qt.C, in serialChange) *protocol.PlanFeaturesRequest {
	c.Helper()
	heldColumn := "{}"
	if in.heldStart != "" {
		heldColumn = `{"fromSequence":{"name":"_serial_column_id","minValue":"1","maxValue":"` + strconv.FormatInt(in.maximum, 10) + `","startValue":"` + in.heldStart +
			`","cache":"1","increment":"` + in.heldIncrement + `","cycle":false,"dataType":{"typeId":"INT64"}}}`
	}
	request := alterRequest(c, alterCase{heldTable: plainTable, heldColumns: map[string]string{"id": heldColumn}, capabilities: in.caps})
	request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
	table := request.Tables[0]
	desired, held := proto.CloneOf(in.desired), proto.CloneOf(in.held)
	desired.Name, desired.Primary, held.Name = "id", true, "id"
	table.Desired.Columns, table.Current.Columns = []*protocol.DeclaredColumn{desired}, []*protocol.ObservedColumn{held}
	if in.added != nil {
		table.Desired.Columns = append(table.Desired.Columns, in.added)
	}
	request.Policy = in.policy
	if in.policy != nil {
		request.TargetFeatures = append(request.TargetFeatures, provider.PlanningPolicyFeature)
	}
	return request
}

// A changed start or increment of an existing Serial is one step that alters
// the column's sequence without a restart, as the host's embedded engine
// writes it; a sequence moved back to the defaults is written out too.
func TestSerialSequenceChange_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		desired *protocol.DeclaredColumn
		held    *protocol.ObservedColumn
		start   string
		incr    string
		want    ydbsequence.Settings
		text    string
	}{
		{name: "another increment", desired: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "7"},
			held: heldSerial("100", "5"), start: "100", incr: "5", want: ydbsequence.Settings{Start: 100, Increment: 7},
			text: "ALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 100 INCREMENT BY 7;"},
		{name: "a start on an auto-increment BIGINT", desired: &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true, IdentityStart: "50"},
			held: heldSerial("", ""), start: "1", incr: "1", want: ydbsequence.Settings{Start: 50, Increment: 1},
			text: "ALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 50 INCREMENT BY 1;"},
		{name: "back to the defaults", desired: &protocol.DeclaredColumn{Type: "BIGSERIAL"},
			held: heldSerial("100", "5"), start: "100", incr: "5", want: ydbsequence.Default,
			text: "ALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 1 INCREMENT BY 1;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := serialChangeRequest(c, serialChange{desired: test.desired, held: test.held, heldStart: test.start, heldIncrement: test.incr,
				maximum: 9223372036854775807, caps: serialCapabilities()})
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			step := plan.Contributions[0].Steps[0]
			c.Assert(step.Id.Name, qt.Equals, "serial-sequence/000000")
			c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.SequenceOperationKind)
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(step.Metadata.Effects, qt.HasLen, 2)
			c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_READ)
			c.Assert(step.Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_ALTER)
			c.Assert(step.Metadata.Effects[1].Subject.Name.Source, qt.Equals, "id")
			for _, parent := range plan.Parents {
				c.Assert(parent.Receipt.Steps, qt.HasLen, 1)
				c.Assert(proto.Equal(parent.Receipt.Steps[0], step.Id), qt.IsTrue)
			}
			change, err := tablecreate.DecodeSequence(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(change.Settings, qt.Equals, test.want)
			c.Assert(change.Held.GetFromSequence().GetIncrement(), qt.Equals, mustInt(c, test.incr))
			text, err := tablecreate.SequenceAlterationStatement("/local", "app/records", change)
			c.Assert(err, qt.IsNil)
			c.Assert(text, qt.Equals, test.text)
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

func mustInt(c *qt.C, text string) int64 {
	c.Helper()
	value, err := strconv.ParseInt(text, 10, 64)
	c.Assert(err, qt.IsNil)
	return value
}

// heldNarrowSerial is heldSerial for an Int32 Serial key.
func heldNarrowSerial(start, increment string) *protocol.ObservedColumn {
	held := heldSerial(start, increment)
	held.DataType = "Int32"
	return held
}

// A sequence that holds what the declaration states, however it spells it,
// plans nothing. So does a 32-bit Serial whose sequence holds a start and an
// increment, which no target that raises its range on ALTER SEQUENCE could
// give it: the table needs no statement, as the host's embedded engine plans
// it (G113 of stokaro/ptah#4204).
func TestSerialSequenceUnchanged_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		desired *protocol.DeclaredColumn
		held    *protocol.ObservedColumn
		maximum int64
	}{
		{name: "the same settings", desired: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "5"}, held: heldSerial("100", "5"),
			maximum: 9223372036854775807},
		{name: "another spelling", desired: &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true, IdentityStart: "0100", IdentityIncrement: " 5"}, held: heldSerial("100", "5"),
			maximum: 9223372036854775807},
		{name: "the settings a 32-bit Serial holds", desired: &protocol.DeclaredColumn{Type: "INTEGER", AutoIncrement: true, IdentityStart: "100", IdentityIncrement: "5"},
			held: heldNarrowSerial("100", "5"), maximum: 2147483647},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := serialChangeRequest(c, serialChange{desired: test.desired, held: test.held, heldStart: "100", heldIncrement: "5",
				maximum: test.maximum, caps: serialCapabilities()})
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// What the server would break is refused in the host's embedded engine's
// words: a sequence some ALTER SEQUENCE restarted, the sequence of a 32-bit
// Serial, a target without sequence settings, a column that becomes or stops
// being a Serial, a Serial added to a table that exists, and the rebuild of a
// table with a Serial.
func TestSerialSequenceChange_FailurePath(t *testing.T) {
	restarted := heldSerial("100", "5")
	restarted.SequenceRestart = "110"
	narrow := heldSerial("", "")
	narrow.DataType = "Int32"
	plain := &protocol.ObservedColumn{DataType: "Int64", IsNullable: "NO", PrimaryKey: true}
	for _, test := range []struct {
		name string
		in   serialChange
		want string
	}{
		{name: "a restarted sequence", in: serialChange{desired: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "10"},
			held: restarted, heldStart: "100", heldIncrement: "5", maximum: 9223372036854775807, caps: serialCapabilities()},
			want: "the sequence of column \"id\" of table \"app/records\": the sequence was restarted at 110, and YDB replays that restart on every later ALTER SEQUENCE, " +
				"so the next insert would take that value again and fail on a key a row may hold (measured: `Conflict with existing key`)"},
		{name: "a start on a 32-bit Serial", in: serialChange{desired: &protocol.DeclaredColumn{Type: "SERIAL", IdentityIncrement: "2"},
			held: narrow, heldStart: "1", heldIncrement: "1", maximum: 2147483647, caps: serialCapabilities()},
			want: `giving the sequence of column "id" of table "app/records" a start or an increment (an ALTER SEQUENCE raises the maximum of a Serial's sequence ` +
				`from 2147483647 to the Int64 maximum, and the column then stores the value after 2147483647 as a negative number without an error; declare the column ` +
				`as a 64-bit Serial (BIGSERIAL, or BIGINT with auto_increment) to give its sequence a start or an increment), which requires target capability ` +
				`serial_sequence_keeps_range, unavailable on this ydb target`},
		{name: "the defaults written back on a 32-bit Serial", in: serialChange{desired: &protocol.DeclaredColumn{Type: "SERIAL"},
			held: heldNarrowSerial("100", "5"), heldStart: "100", heldIncrement: "5", maximum: 2147483647, caps: serialCapabilities()},
			want: `giving the sequence of column "id" of table "app/records" a start or an increment (an ALTER SEQUENCE raises the maximum of a Serial's sequence ` +
				`from 2147483647 to the Int64 maximum, and the column then stores the value after 2147483647 as a negative number without an error; declare the column ` +
				`as a 64-bit Serial (BIGSERIAL, or BIGINT with auto_increment) to give its sequence a start or an increment), which requires target capability ` +
				`serial_sequence_keeps_range, unavailable on this ydb target`},
		{name: "a target without sequence settings", in: serialChange{desired: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "10"},
			held: heldSerial("100", "5"), heldStart: "100", heldIncrement: "5", maximum: 9223372036854775807, caps: map[string]bool{"serial_columns": true}},
			want: `giving the sequence of column "id" of table "app/records" a start or an increment, which requires target capability serial_sequence_options, unavailable on this ydb target`},
		{name: "the defaults written back on a target without sequence settings", in: serialChange{desired: &protocol.DeclaredColumn{Type: "BIGSERIAL"},
			held: heldSerial("100", "5"), heldStart: "100", heldIncrement: "5", maximum: 9223372036854775807, caps: map[string]bool{"serial_columns": true}},
			want: `giving the sequence of column "id" of table "app/records" a start or an increment, which requires target capability serial_sequence_options, unavailable on this ydb target`},
		{name: "a column that becomes a Serial", in: serialChange{desired: &protocol.DeclaredColumn{Type: "BIGSERIAL"}, held: plain, caps: serialCapabilities()},
			want: `changing the type of column "id" of table "app/records" (Int64 -> BigSerial), which requires target capability alter_column_type, unavailable on this ydb target; ` +
				`YDB makes it by rebuilding the table, which Ptah plans when asked with a table rebuild request in the host's planning policy (contract 25.3)`},
		{name: "a Serial that stops being one", in: serialChange{desired: &protocol.DeclaredColumn{Type: "Int64"}, held: heldSerial("", ""), heldStart: "1", heldIncrement: "1",
			maximum: 9223372036854775807, caps: serialCapabilities()},
			want: `changing the type of column "id" of table "app/records" (BigSerial -> Int64), which requires target capability alter_column_type, unavailable on this ydb target; ` +
				`YDB makes it by rebuilding the table, which Ptah plans when asked with a table rebuild request in the host's planning policy (contract 25.3)`},
		{name: "native and common reads that disagree", in: serialChange{desired: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "10"},
			held: heldSerial("100", "5"), heldStart: "99", heldIncrement: "5", maximum: 9223372036854775807, caps: serialCapabilities()},
			want: `the sequence of column "id" of table "app/records": the common and native reads of the sequence disagree`},
		{name: "an added Serial", in: serialChange{desired: &protocol.DeclaredColumn{Type: "Int64"}, held: plain, caps: serialCapabilities(),
			added: &protocol.DeclaredColumn{Name: "seq", Type: "BIGSERIAL"}},
			want: "adding column \"seq\" to table \"app/records\": YDB adds no Serial column to an existing table (`Column addition with serial data type is unsupported`)"},
		{name: "a rebuild of a table with a Serial", in: serialChange{desired: &protocol.DeclaredColumn{Type: "BIGSERIAL"}, held: plain, caps: serialCapabilities(),
			policy: &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"}},
			want: "rebuilding table \"app/records\": column \"id\" takes its values from a sequence. The new table's sequence would start at 1 while the copied rows keep " +
				"theirs, so the next insert would collide (`Conflict with existing key`), and YDB's ALTER SEQUENCE ... RESTART WITH takes only a literal, so no statement " +
				"can move it past the copied rows"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), serialChangeRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Code, qt.Equals, protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, test.want)
		})
	}
}

// heldSequenceColumn is the native read of an Int64 Serial id whose sequence
// starts at 100 and steps by 5.
func heldSequenceColumn() *native.ColumnMeta {
	return &native.ColumnMeta{Name: "id", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT64}}, NotNull: new(true),
		DefaultValue: &native.ColumnMeta_FromSequence{FromSequence: &native.SequenceDescription{Name: new("_serial_column_id"), MinValue: proto.Int64(1),
			MaxValue: proto.Int64(9223372036854775807), StartValue: proto.Int64(100), Cache: proto.Uint64(1), Increment: proto.Int64(5), Cycle: new(false)}}}
}

// The operation keeps the column it read and the settings the sequence takes,
// and its TEXT form needs the database that names the sequence.
func TestSequenceOperation_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	change := &tablecreate.SequenceAlteration{Held: heldSequenceColumn(), Settings: ydbsequence.Settings{Start: 100, Increment: 7}}
	operation, err := tablecreate.CaptureSequence("sequence", owner(), change)
	c.Assert(err, qt.IsNil)
	decoded, err := tablecreate.DecodeSequence(operation.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(decoded.Held, change.Held), qt.IsTrue)
	c.Assert(decoded.Settings, qt.Equals, change.Settings)
	text, err := tablecreate.SequenceAlterationStatement("", "app/records", decoded)
	c.Assert(err, qt.ErrorIs, tablecreate.ErrNoTextForm)
	c.Assert(text, qt.Equals, "")
	execution, err := backend.New().Execute(c.Context(), &protocol.ExecuteRequest{SessionId: "not-open", TransactionId: "forbidden", RequestId: "request",
		Units: []*protocol.Unit{{ChangeId: "sequence", Body: &protocol.Unit_Native{Native: operation.Value}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(execution.DiagnosticCode, qt.Equals, "ydb.native-transaction-forbidden")
}

// A capture that does not change a sequence, or changes one a restart would
// replay, is refused.
func TestSequenceOperation_FailurePath(t *testing.T) {
	restarted := heldSequenceColumn()
	restarted.GetFromSequence().SetVal = &native.SequenceDescription_SetVal{NextValue: proto.Int64(110)}
	noSequence := heldSequenceColumn()
	noSequence.DefaultValue = nil
	for _, test := range []struct {
		name     string
		held     *native.ColumnMeta
		settings ydbsequence.Settings
		want     string
	}{
		{name: "the settings it holds", held: heldSequenceColumn(), settings: ydbsequence.Settings{Start: 100, Increment: 5},
			want: `a sequence operation must change the sequence`},
		{name: "a restarted sequence", held: restarted, settings: ydbsequence.Settings{Start: 100, Increment: 7},
			want: `a sequence operation does not change a sequence some ALTER SEQUENCE restarted`},
		{name: "a column without a sequence", held: noSequence, settings: ydbsequence.Settings{Start: 100, Increment: 7},
			want: `a sequence operation requires a named Serial column`},
		{name: "an increment of 0", held: heldSequenceColumn(), settings: ydbsequence.Settings{Start: 100, Increment: 0},
			want: `a sequence operation states its start and increment as whole numbers of at least 1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			operation, err := tablecreate.CaptureSequence("sequence", owner(), &tablecreate.SequenceAlteration{Held: test.held, Settings: test.settings})
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(operation, qt.IsNil)
		})
	}
}
