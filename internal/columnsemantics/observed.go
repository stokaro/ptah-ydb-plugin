package columnsemantics

import (
	"fmt"
	"math"
	"strconv"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// Projected derives common facts from the exact column native creation writes.
// The complete native binding preserves fields the vendor binding leaves as
// unknown bytes, and Observed validates and maps the same facts as inspection.
func Projected(column *Ydb_Table.ColumnMeta, position int, primary bool) (*protocol.ObservedColumn, error) {
	encoded, err := proto.Marshal(column)
	if err != nil {
		return nil, err
	}
	var nativeColumn native.ColumnMeta
	if err := proto.Unmarshal(encoded, &nativeColumn); err != nil {
		return nil, err
	}
	return Observed(&nativeColumn, position, primary)
}

// Observed projects the common facts of a complete native column. The caller
// retains its native facet independently for settings and execution evidence.
func Observed(column *native.ColumnMeta, position int, primary bool) (*protocol.ObservedColumn, error) {
	if err := schema.Known(column); err != nil {
		return nil, err
	}
	if column.Name == "" || position < 1 {
		return nil, fmt.Errorf("observed column requires its name and positive ordinal")
	}
	spelling, nullable, err := schema.ColumnType(column.Type)
	if err != nil {
		return nil, err
	}
	result := &protocol.ObservedColumn{Name: column.Name, OrdinalPosition: int64(position), DataType: spelling, ColumnType: spelling,
		IsNullable: "NO", PrimaryKey: primary}
	if nullable && !column.GetNotNull() {
		result.IsNullable = "YES"
	}
	if precision, scale, ok := ydbtype.DecimalArguments(spelling); ok {
		result.NumericPrecision, result.NumericScale = new(int64(precision)), new(int64(scale))
	}
	if literal := column.GetFromLiteral(); literal != nil {
		literalType, _, err := schema.ColumnType(literal.Type)
		if err != nil || literalType != spelling {
			return nil, fmt.Errorf("column %q default type differs from its storage type", column.Name)
		}
		text, err := Literal(literal)
		if err != nil {
			return nil, fmt.Errorf("column %q default: %w", column.Name, err)
		}
		result.DefaultExpression = new(text)
	}
	if sequence := column.GetFromSequence(); sequence != nil {
		if err := observeSequence(result, sequence); err != nil {
			return nil, fmt.Errorf("column %q: %w", column.Name, err)
		}
	}
	return result, nil
}

func validateObservedSequence(column *protocol.ObservedColumn, sequence *native.SequenceDescription) error {
	maximum := int64(math.MaxInt64)
	switch column.ColumnType {
	case ydbtype.Int16:
		maximum = math.MaxInt16
	case ydbtype.Int32:
		maximum = math.MaxInt32
	case ydbtype.Int64:
	default:
		return fmt.Errorf("a Serial sequence requires a signed integer column")
	}
	if sequence.Name != nil && sequence.GetName() != "_serial_column_"+column.Name {
		return fmt.Errorf("sequence name differs from the column's owned sequence")
	}
	if sequence.MinValue != nil && sequence.GetMinValue() != 1 || sequence.Cache != nil && sequence.GetCache() != 1 || sequence.GetCycle() {
		return fmt.Errorf("sequence requires minimum 1, cache 1, and no cycling")
	}
	if sequence.MaxValue != nil && sequence.GetMaxValue() != maximum && sequence.GetMaxValue() != math.MaxInt64 {
		return fmt.Errorf("sequence maximum differs from the column or Int64 maximum")
	}
	if sequence.DataType != nil && !proto.Equal(sequence.DataType, &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT64}}) {
		return fmt.Errorf("sequence requires Int64 values")
	}
	return nil
}

func observeSequence(column *protocol.ObservedColumn, sequence *native.SequenceDescription) error {
	if err := validateObservedSequence(column, sequence); err != nil {
		return err
	}
	column.AutoIncrement = true
	if sequence.StartValue != nil && sequence.GetStartValue() != 1 {
		column.IdentityStart = strconv.FormatInt(sequence.GetStartValue(), 10)
	}
	if sequence.Increment != nil && sequence.GetIncrement() != 1 {
		column.IdentityIncrement = strconv.FormatInt(sequence.GetIncrement(), 10)
	}
	if restart := sequence.GetSetVal(); restart != nil {
		column.SequenceRestart = strconv.FormatInt(restart.GetNextValue(), 10)
	}
	return nil
}
