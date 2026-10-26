// Package columnsemantics compares captured YDB column definitions without I/O.
package columnsemantics

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// Compare answers exactly the requested aspects in captured table and pair order.
// Identity pairing belongs to the host; this service never matches names itself.
func Compare(ctx context.Context, request *protocol.CompareColumnsRequest) (*protocol.ComparedColumns, error) {
	result := &protocol.ComparedColumns{Complete: true}
	for _, table := range request.GetTables() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		output := &protocol.ComparedColumnTable{Subject: proto.CloneOf(table.Subject), CurrentSubject: proto.CloneOf(table.CurrentSubject)}
		for _, pair := range table.Pairs {
			di := slices.IndexFunc(table.GetDesired().GetColumns(), func(c *protocol.DeclaredColumn) bool { return c.Name == pair.DesiredColumn })
			ci := slices.IndexFunc(table.GetCurrent().GetColumns(), func(c *protocol.ObservedColumn) bool { return c.Name == pair.CurrentColumn })
			if di < 0 || ci < 0 {
				return nil, fmt.Errorf("column comparison pair is outside its captured parent")
			}
			desired, current := table.Desired.Columns[di], table.Current.Columns[ci]
			column := &protocol.ComparedColumn{Subject: proto.CloneOf(pair.Subject), CurrentSubject: proto.CloneOf(pair.CurrentSubject)}
			for _, aspect := range pair.Aspects {
				receipt := &protocol.ColumnAspectReceipt{Aspect: aspect}
				switch aspect {
				case protocol.ColumnAspect_COLUMN_TYPE:
					want, held := desired.Type, currentType(current)
					if desired.AutoIncrement || desired.IdentityGeneration != "" {
						want = serialOf(want)
					}
					if current.AutoIncrement {
						held = serialOf(held)
					}
					want, held = comparableTypes(want, held)
					receipt.Receipt = &protocol.ColumnAspectReceipt_Difference{Difference: difference(held, want)}
				case protocol.ColumnAspect_COLUMN_DEFAULT:
					receipt.Receipt = &protocol.ColumnAspectReceipt_Difference{Difference: difference(current.GetDefaultExpression(), defaultLiteral(desired, currentType(current)))}
				case protocol.ColumnAspect_COLUMN_NULLABILITY:
					primary := desired.Primary || slices.Contains(table.Desired.PrimaryKey, desired.Name) || slices.ContainsFunc(table.Desired.PrimaryKeyParts, func(p *protocol.DeclaredKeyPart) bool { return p.Column == desired.Name })
					receipt.Receipt = &protocol.ColumnAspectReceipt_Nullability{Nullability: &protocol.ColumnNullability{Nullable: new(EffectiveNullable(desired, primary))}}
				case protocol.ColumnAspect_COLUMN_SEQUENCE:
					receipt.Receipt = &protocol.ColumnAspectReceipt_Sequence{Sequence: sequenceChanges(desired, current)}
				default:
					return nil, fmt.Errorf("unsupported YDB column comparison aspect")
				}
				column.Aspects = append(column.Aspects, receipt)
			}
			output.Columns = append(output.Columns, column)
		}
		result.Tables = append(result.Tables, output)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadDefaults distinguishes absent defaults, literal values, and expressions.
// Empty string literals stay present; no caller has to infer them from text.
func ReadDefaults(ctx context.Context, request *protocol.ReadColumnDefaultsRequest) (*protocol.ReadColumnDefaultsResult, error) {
	result := &protocol.ReadColumnDefaultsResult{Complete: true}
	for _, input := range request.GetColumns() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		receipt := &protocol.ColumnDefaultReceipt{Subject: proto.CloneOf(input.Subject), Kind: protocol.ColumnDefaultReceipt_ABSENT}
		text := input.GetColumn().GetDefaultExpression()
		if text != "" {
			receipt.Kind, receipt.Value = protocol.ColumnDefaultReceipt_EXPRESSION, text
			if value, ok := ydbtype.LiteralValue(text); ok {
				receipt.Kind, receipt.Value = protocol.ColumnDefaultReceipt_LITERAL, value
			}
		}
		result.Columns = append(result.Columns, receipt)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// EffectiveNullable matches the renderer's NOT NULL primary and serial columns.
// The declaration remains unchanged; this is the definition creation produces.
func EffectiveNullable(column *protocol.DeclaredColumn, primary bool) bool {
	return column.Nullable && !primary && !declaresSerial(column)
}

func difference(current, desired string) *protocol.ColumnDifference {
	relation := protocol.ColumnDifference_SAME
	if current != desired {
		relation = protocol.ColumnDifference_CHANGED
	}
	return &protocol.ColumnDifference{Relation: relation, Current: current, Desired: desired}
}

func currentType(column *protocol.ObservedColumn) string {
	for _, value := range []string{column.ColumnType, column.FormattedType, column.DataType, column.UdtName} {
		if value != "" {
			return value
		}
	}
	return ""
}

func renderings(declared string) []string {
	var result []string
	for _, wide := range []bool{true, false} {
		mapping, err := ydbtype.Map(declared, ydbtype.Capabilities{ydbtype.WideDateTimeTypes: wide, ydbtype.ParameterizedDecimal: true, ydbtype.SerialColumns: true})
		if err == nil && !slices.Contains(result, mapping.Type) {
			result = append(result, mapping.Type)
		}
	}
	return result
}

func comparableTypes(declared, current string) (desiredType, currentType string) {
	want, have := renderings(declared), renderings(current)
	if len(have) == 0 {
		have = []string{strings.TrimSpace(current)}
	}
	for _, a := range want {
		for _, b := range have {
			if strings.EqualFold(a, b) {
				return b, b
			}
		}
	}
	if len(want) == 0 {
		return strings.TrimSpace(declared), have[0]
	}
	return want[0], have[0]
}

func serialOf(declared string) string {
	types := renderings(declared)
	if len(types) == 0 {
		return declared
	}
	switch types[0] {
	case ydbtype.Int16:
		return ydbtype.SmallSerial
	case ydbtype.Int32:
		return ydbtype.Serial
	case ydbtype.Int64:
		return ydbtype.BigSerial
	}
	return declared
}

func declaresSerial(column *protocol.DeclaredColumn) bool {
	if column.AutoIncrement || column.IdentityGeneration != "" {
		return true
	}
	mapping, err := ydbtype.Map(column.Type, ydbtype.Capabilities{ydbtype.SerialColumns: true})
	return err == nil && mapping.Serial
}

func sequenceChanges(desired *protocol.DeclaredColumn, current *protocol.ObservedColumn) *protocol.ColumnSequenceDifferences {
	result := &protocol.ColumnSequenceDifferences{}
	if !declaresSerial(desired) || !current.AutoIncrement {
		return result
	}
	for _, setting := range []struct {
		kind             protocol.ColumnSequenceDifference_Attribute
		desired, current string
	}{
		{protocol.ColumnSequenceDifference_IDENTITY_START, desired.IdentityStart, current.IdentityStart},
		{protocol.ColumnSequenceDifference_IDENTITY_INCREMENT, desired.IdentityIncrement, current.IdentityIncrement},
	} {
		want, have := canonicalSequence(setting.desired), canonicalSequence(setting.current)
		if want != have {
			result.Changes = append(result.Changes, &protocol.ColumnSequenceDifference{Attribute: setting.kind, Current: have, Desired: want})
		}
	}
	return result
}

func canonicalSequence(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "1"
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return value
	}
	return strconv.FormatInt(number, 10)
}

func defaultLiteral(column *protocol.DeclaredColumn, heldType string) string {
	if !column.DefaultSet && column.DefaultValue == "" {
		return column.DefaultExpression
	}
	value, isNull := declaredValue(column.DefaultValue)
	if isNull {
		return ""
	}
	typ, _ := comparableTypes(column.Type, heldType)
	literal, err := ydbtype.ValueLiteral(typ, value)
	if err != nil {
		return column.DefaultValue
	}
	return literal
}

// Source defaults can be bare values or SQL string literals with an optional
// cast. Only a complete quoted literal is decoded; arbitrary expressions stay
// with the comparison and rendering refusal paths.
func declaredValue(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if len(text) < 2 || text[0] != '\'' {
		return text, strings.EqualFold(text, "NULL")
	}
	var value strings.Builder
	for i := 1; i < len(text); i++ {
		if text[i] != '\'' {
			value.WriteByte(text[i])
			continue
		}
		if i+1 < len(text) && text[i+1] == '\'' {
			value.WriteByte('\'')
			i++
			continue
		}
		rest := text[i+1:]
		if rest == "" || strings.HasPrefix(rest, "::") && len(rest) > 2 {
			return value.String(), false
		}
		return text, false
	}
	return text, false
}
