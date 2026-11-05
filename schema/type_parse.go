package schema

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
)

// StorageType parses the column grammar exported by ColumnType into the type
// a NOT NULL column stores. Nullability is a separate common property, so
// nested Optional syntax is never accepted; [Optional] gives a nullable
// column's type.
func StorageType(spelling string) (*Ydb.Type, error) {
	var result *Ydb.Type
	for id, name := range primitiveTypes {
		if strings.EqualFold(spelling, name) {
			result = &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: id}}
			break
		}
	}
	if result == nil && strings.HasPrefix(strings.ToLower(spelling), "decimal(") && strings.HasSuffix(spelling, ")") {
		arguments := strings.Split(spelling[len("Decimal("):len(spelling)-1], ",")
		if len(arguments) == 2 {
			precision, a := decimalArgument(arguments[0])
			scale, b := decimalArgument(arguments[1])
			if a == nil && b == nil && precision > 0 && precision <= 35 && scale <= precision {
				result = &Ydb.Type{Type: &Ydb.Type_DecimalType{DecimalType: &Ydb.DecimalType{Precision: precision, Scale: scale}}}
			}
		}
	}
	if result == nil {
		return nil, fmt.Errorf("unsupported YDB storage column type")
	}
	return result, nil
}

// Optional is the type a nullable column of the storage type item stores.
func Optional(item *Ydb.Type) *Ydb.Type {
	return &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: item}}}
}

func decimalArgument(value string) (uint32, error) {
	// Only decimal digits and spaces around an argument are type syntax.
	value = strings.Trim(value, " \t\r\n")
	if value == "" {
		return 0, fmt.Errorf("missing Decimal argument")
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("invalid Decimal argument")
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	return uint32(parsed), err
}
