package tablecreate

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// layout is the starting layout of a new table as a source states it: a count
// of evenly split partitions, or split points written as text. Both empty is no
// layout.
type layout struct {
	uniform uint64
	points  [][]string
}

func layoutOf(settings *native.RowTableIntent) layout {
	result := layout{uniform: settings.GetUniformPartitions()}
	for _, point := range settings.GetPartitionAtKeys() {
		result.points = append(result.points, slices.Clone(point.GetValues()))
	}
	return result
}

// partitions is how many partitions the layout creates, and 0 for no layout.
func (l layout) partitions() uint64 {
	switch {
	case l.uniform != 0:
		return l.uniform
	case len(l.points) != 0:
		return uint64(len(l.points)) + 1
	default:
		return 0
	}
}

// integerKinds are the integer key types a split point value may be written
// for, with their width and whether they are unsigned.
var integerKinds = map[string]struct {
	bits     int
	unsigned bool
	id       Ydb.Type_PrimitiveTypeId
}{
	"Int8": {8, false, Ydb.Type_INT8}, "Int16": {16, false, Ydb.Type_INT16}, "Int32": {32, false, Ydb.Type_INT32}, "Int64": {64, false, Ydb.Type_INT64},
	"Uint8": {8, true, Ydb.Type_UINT8}, "Uint16": {16, true, Ydb.Type_UINT16}, "Uint32": {32, true, Ydb.Type_UINT32}, "Uint64": {64, true, Ydb.Type_UINT64},
}

// keyTypes returns the YDB type names of the key columns, in key order.
func keyTypes(key []string, columns []*Ydb_Table.ColumnMeta) ([]string, error) {
	types := make([]string, 0, len(key))
	for _, name := range key {
		at := slices.IndexFunc(columns, func(column *Ydb_Table.ColumnMeta) bool { return column.GetName() == name })
		if at < 0 {
			return nil, fmt.Errorf("invalid YDB primary key column")
		}
		kind, _, err := schema.ColumnType(columns[at].GetType())
		if err != nil {
			return nil, err
		}
		types = append(types, kind)
	}
	return types, nil
}

// request writes the layout as the partitions of a native creation request for
// a table whose key columns have the given types, in key order, or nil for no
// layout. A layout YDB would refuse for this key is an error saying why.
// Measured on 25.1.4.7 and 26.2.1.14:
//
//   - uniform_partitions needs a Uint32 or Uint64 first key column (`Unsupported
//     first key column type Int64, only Uint32 and Uint64 are supported`);
//   - a split point holds at most one value per key column (`Partition at keys
//     has 3 key values while there are only 2 key columns`);
//   - a value is a literal number or string: a whole number for an integer
//     column, in the column's range and not negative, and a string for a Utf8 or
//     String column. A column of another type has no literal the setting takes.
//
// Split points that are not in ascending order are left to YDB, which refuses
// them before it creates the table (`Partition ranges are not sorted at index
// 1`).
func (l layout) request(types []string) (*Ydb_Table.CreateTableRequest, error) {
	result := &Ydb_Table.CreateTableRequest{}
	switch {
	case l.uniform != 0:
		if len(types) == 0 {
			return nil, errors.New("uniform_partitions needs the table's key, and the table declares none")
		}
		if types[0] != "Uint32" && types[0] != "Uint64" {
			return nil, fmt.Errorf("uniform_partitions splits the range of the first key column, which YDB does for a Uint32 "+
				"or Uint64 column only, and this one is %s (`Unsupported first key column type %s, only Uint32 and Uint64 "+
				"are supported`)", types[0], types[0])
		}
		result.Partitions = &Ydb_Table.CreateTableRequest_UniformPartitions{UniformPartitions: l.uniform}
	case len(l.points) != 0:
		explicit := &Ydb_Table.ExplicitPartitions{}
		for i, point := range l.points {
			written, err := splitPoint(point, types)
			if err != nil {
				return nil, fmt.Errorf("split point %d of partition_at_keys: %w", i+1, err)
			}
			explicit.SplitPoints = append(explicit.SplitPoints, written)
		}
		result.Partitions = &Ydb_Table.CreateTableRequest_PartitionAtKeys{PartitionAtKeys: explicit}
	}
	return result, nil
}

// splitPoint writes one split point as a tuple of the leading key types. Each
// element is optional, as every key column is for the table service.
func splitPoint(point, types []string) (*Ydb.TypedValue, error) {
	switch {
	case len(point) == 0:
		return nil, errors.New("it holds no value")
	case len(point) > len(types):
		return nil, fmt.Errorf("it holds %d values, and the key has %d columns (`Partition at keys has %d key values while "+
			"there are only %d key columns`)", len(point), len(types), len(point), len(types))
	}
	elements := &Ydb.TupleType{}
	value := &Ydb.Value{}
	for i, text := range point {
		kind, item, err := splitValue(text, types[i])
		if err != nil {
			return nil, err
		}
		elements.Elements = append(elements.Elements, &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: kind}}})
		value.Items = append(value.Items, &Ydb.Value{Value: &Ydb.Value_NestedValue{NestedValue: item}})
	}
	return &Ydb.TypedValue{Type: &Ydb.Type{Type: &Ydb.Type_TupleType{TupleType: elements}}, Value: value}, nil
}

// splitValue writes one value of a split point for a key column of ydbType.
func splitValue(text, ydbType string) (*Ydb.Type, *Ydb.Value, error) {
	if integer, ok := integerKinds[ydbType]; ok {
		parsed, err := strconv.ParseUint(text, 10, integer.bits)
		if err != nil || (!integer.unsigned && parsed > 1<<(integer.bits-1)-1) {
			return nil, nil, fmt.Errorf("%q is not a whole number from 0 to the largest %s, which is what YDB takes for a "+
				"%s key column here", text, ydbType, ydbType)
		}
		return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: integer.id}}, integerValue(ydbType, parsed), nil
	}
	switch ydbType {
	case "Utf8":
		return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}, &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: text}}, nil
	case "String":
		return &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_STRING}}, &Ydb.Value{Value: &Ydb.Value_BytesValue{BytesValue: []byte(text)}}, nil
	default:
		return nil, nil, fmt.Errorf("its key column is %s, and YDB takes only literal numbers and strings here, for integer, "+
			"Utf8 and String columns", ydbType)
	}
}

// integerValue wraps a parsed non-negative integer in the value field its type
// uses.
func integerValue(ydbType string, parsed uint64) *Ydb.Value {
	switch ydbType {
	case "Int8", "Int16", "Int32":
		return &Ydb.Value{Value: &Ydb.Value_Int32Value{Int32Value: int32(parsed)}}
	case "Uint8", "Uint16", "Uint32":
		return &Ydb.Value{Value: &Ydb.Value_Uint32Value{Uint32Value: uint32(parsed)}}
	case "Int64":
		return &Ydb.Value{Value: &Ydb.Value_Int64Value{Int64Value: int64(parsed)}}
	default:
		return &Ydb.Value{Value: &Ydb.Value_Uint64Value{Uint64Value: parsed}}
	}
}

// layoutOfRequest reads the layout a native creation request carries back into
// text, so that a request can be checked against the rules the source side
// follows. It refuses a request that states a layout in a shape the provider
// does not write.
func layoutOfRequest(request *Ydb_Table.CreateTableRequest) (layout, error) {
	switch partitions := request.Partitions.(type) {
	case nil:
		return layout{}, nil
	case *Ydb_Table.CreateTableRequest_UniformPartitions:
		return layout{uniform: partitions.UniformPartitions}, nil
	case *Ydb_Table.CreateTableRequest_PartitionAtKeys:
		result := layout{}
		for _, point := range partitions.PartitionAtKeys.GetSplitPoints() {
			var values []string
			for _, item := range point.GetValue().GetItems() {
				values = append(values, textOfValue(item.GetNestedValue()))
			}
			result.points = append(result.points, values)
		}
		return result, nil
	default:
		return layout{}, fmt.Errorf("YDB creation does not support this partitions form")
	}
}

// textOfValue is the text a split point value was written from.
func textOfValue(value *Ydb.Value) string {
	switch kind := value.GetValue().(type) {
	case *Ydb.Value_Int32Value:
		return strconv.FormatInt(int64(kind.Int32Value), 10)
	case *Ydb.Value_Uint32Value:
		return strconv.FormatUint(uint64(kind.Uint32Value), 10)
	case *Ydb.Value_Int64Value:
		return strconv.FormatInt(kind.Int64Value, 10)
	case *Ydb.Value_Uint64Value:
		return strconv.FormatUint(kind.Uint64Value, 10)
	case *Ydb.Value_TextValue:
		return kind.TextValue
	case *Ydb.Value_BytesValue:
		return string(kind.BytesValue)
	default:
		return ""
	}
}

// validateRequestLayout requires the layout a native creation request carries
// to be exactly what the rules write for its key, so a forged request cannot
// state a layout YDB would refuse or one of another shape.
func validateRequestLayout(request *Ydb_Table.CreateTableRequest) error {
	stated, err := layoutOfRequest(request)
	if err != nil {
		return err
	}
	if stated.partitions() == 0 {
		if request.Partitions != nil {
			return fmt.Errorf("YDB creation states an empty starting layout")
		}
		return nil
	}
	types, err := keyTypes(request.PrimaryKey, request.Columns)
	if err != nil {
		return err
	}
	expected, err := stated.request(types)
	if err != nil {
		return err
	}
	if !proto.Equal(&Ydb_Table.CreateTableRequest{Partitions: request.Partitions}, expected) {
		return fmt.Errorf("YDB creation states a starting layout of a shape the provider does not write")
	}
	return nil
}

// StartingPartitions is how many partitions the starting layout of a creation
// request makes the table start with, and 0 for a request with no layout. YDB
// keeps no record of a layout, only the minimum partition count it sets from
// it, which a creation that names no minimum of its own is confirmed by.
func StartingPartitions(request *Ydb_Table.CreateTableRequest) uint64 {
	started, err := layoutOfRequest(request)
	if err != nil {
		return 0
	}
	return started.partitions()
}

// describe names the layout as YDB spells the setting.
func (l layout) describe() string {
	switch {
	case l.uniform != 0:
		return fmt.Sprintf("UNIFORM_PARTITIONS = %d", l.uniform)
	case len(l.points) == 1:
		return "PARTITION_AT_KEYS with one split point"
	default:
		return fmt.Sprintf("PARTITION_AT_KEYS with %d split points", len(l.points))
	}
}

// keyword is the setting the layout uses.
func (l layout) keyword() string {
	if l.uniform != 0 {
		return "UNIFORM_PARTITIONS"
	}
	return "PARTITION_AT_KEYS"
}

// defaultMinPartitions is the minimum a table holds when nothing sets one.
const defaultMinPartitions = 1

// layoutRefusal says why a table holding held cannot take the starting layout
// a declaration states, or returns the empty string. YDB takes a layout only
// when it creates a table (`ALTER TABLE t SET (UNIFORM_PARTITIONS = 4)` answers
// `UNIFORM_PARTITIONS alter is not supported`, and PARTITION_AT_KEYS answers the
// same), and keeps no record of one except the minimum partition count it sets
// from it. That minimum is the one witness a table has: a declaration whose
// layout sets the minimum (it states none of its own) and whose minimum differs
// from the table's asks for a layout the table was not created with. A layout
// beside a declared minimum leaves no witness, and is not compared.
func layoutRefusal(wanted layout, desired, held *Ydb_Table.PartitioningSettings) string {
	started := wanted.partitions()
	if started == 0 || desired.GetMinPartitionsCount() != 0 {
		return ""
	}
	current := held.GetMinPartitionsCount()
	if current == 0 {
		current = defaultMinPartitions
	}
	if started == current {
		return ""
	}
	return fmt.Sprintf("it declares %s, which YDB takes only when it creates a table (`%s alter is not supported`) and "+
		"which gives a new table a minimum of %d partitions, where this table holds %d. Declare "+
		"auto_partitioning_min_partitions_count to change the minimum in place", wanted.describe(), wanted.keyword(), started, current)
}
