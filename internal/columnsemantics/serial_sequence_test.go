package columnsemantics_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/testing/protocmp"
)

// sequenceRow is a desired id declared as typ with a start and an increment,
// against an Int64 Serial id whose sequence the read reports.
type sequenceRow struct {
	name                 string
	typ, start, inc      string
	heldStart, heldInc   string
	heldRestart          string
	want                 []*protocol.ColumnSequenceDifference
	heldAutoIncrementOff bool
}

func (r sequenceRow) changes(c *qt.C) []*protocol.ColumnSequenceDifference {
	c.Helper()
	desired := &protocol.DeclaredColumn{Name: "id", Type: r.typ, Primary: true, AutoIncrement: true, IdentityGeneration: "BY_DEFAULT",
		IdentityStart: r.start, IdentityIncrement: r.inc}
	current := &protocol.ObservedColumn{Name: "id", DataType: "Int64", IsNullable: "NO", PrimaryKey: true, AutoIncrement: !r.heldAutoIncrementOff,
		IdentityStart: r.heldStart, IdentityIncrement: r.heldInc, SequenceRestart: r.heldRestart}
	result, err := columnsemantics.Compare(c.Context(), requestFor(desired, current, protocol.ColumnAspect_COLUMN_SEQUENCE))
	c.Assert(err, qt.IsNil)
	return result.Tables[0].Columns[0].Aspects[0].GetSequence().GetChanges()
}

// The declaration and the sequence agree however each spells the settings:
// the read leaves a setting of 1 empty, and a declaration may write it, or
// write `0100` for 100.
func TestSerialSequenceComparisonAgrees(t *testing.T) {
	for _, row := range []sequenceRow{
		{name: "both at the defaults", typ: "BIGSERIAL"},
		{name: "the defaults written out", typ: "BIGSERIAL", start: "1", inc: "1"},
		{name: "a start and an increment", typ: "BIGINT", start: "0100", inc: "5", heldStart: "100", heldInc: "5"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(row.changes(c), qt.HasLen, 0)
		})
	}
}

// A changed start or increment is reported under the attribute it is declared
// with, each on its own, a restart the read reports included.
func TestSerialSequenceComparisonChanges(t *testing.T) {
	start := protocol.ColumnSequenceDifference_IDENTITY_START
	increment := protocol.ColumnSequenceDifference_IDENTITY_INCREMENT
	for _, row := range []sequenceRow{
		{name: "a start declared on a sequence nobody altered", typ: "BIGSERIAL", start: "100",
			want: []*protocol.ColumnSequenceDifference{{Attribute: start, Current: "1", Desired: "100"}}},
		{name: "an increment changed", typ: "BIGSERIAL", start: "100", inc: "10", heldStart: "100", heldInc: "5",
			want: []*protocol.ColumnSequenceDifference{{Attribute: increment, Current: "5", Desired: "10"}}},
		{name: "both moved back to the defaults on a restarted sequence", typ: "BIGSERIAL", heldStart: "100", heldInc: "5", heldRestart: "100",
			want: []*protocol.ColumnSequenceDifference{{Attribute: start, Current: "100", Desired: "1"}, {Attribute: increment, Current: "5", Desired: "1"}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(row.changes(c), qt.CmpEquals(protocmp.Transform()), row.want)
		})
	}
}

// A column that is a Serial on one side only has no sequence to compare: its
// type differs.
func TestSerialSequenceComparisonNeedsASerialOnBothSides(t *testing.T) {
	c := testcheck.New(t)
	row := sequenceRow{typ: "BIGSERIAL", start: "100", heldAutoIncrementOff: true}
	c.Assert(row.changes(c), qt.HasLen, 0)
	desired := &protocol.DeclaredColumn{Name: "id", Type: "BIGSERIAL", Primary: true, IdentityStart: "100"}
	current := &protocol.ObservedColumn{Name: "id", DataType: "Int64", IsNullable: "NO", PrimaryKey: true}
	result, err := columnsemantics.Compare(c.Context(), requestFor(desired, current, protocol.ColumnAspect_COLUMN_TYPE))
	c.Assert(err, qt.IsNil)
	difference := result.Tables[0].Columns[0].Aspects[0].GetDifference()
	c.Assert([]any{difference.GetRelation(), difference.GetCurrent(), difference.GetDesired()}, qt.DeepEquals,
		[]any{protocol.ColumnDifference_CHANGED, "Int64", "BigSerial"})
}
