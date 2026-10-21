// Package schema owns YDB schema observations and their wire definitions.
package schema

import (
	"fmt"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
)

var primitiveTypes = map[Ydb.Type_PrimitiveTypeId]string{
	Ydb.Type_BOOL: "Bool", Ydb.Type_INT8: "Int8", Ydb.Type_UINT8: "Uint8",
	Ydb.Type_INT16: "Int16", Ydb.Type_UINT16: "Uint16", Ydb.Type_INT32: "Int32", Ydb.Type_UINT32: "Uint32",
	Ydb.Type_INT64: "Int64", Ydb.Type_UINT64: "Uint64", Ydb.Type_FLOAT: "Float", Ydb.Type_DOUBLE: "Double",
	Ydb.Type_DATE: "Date", Ydb.Type_DATETIME: "Datetime", Ydb.Type_TIMESTAMP: "Timestamp", Ydb.Type_INTERVAL: "Interval",
	Ydb.Type_DATE32: "Date32", Ydb.Type_DATETIME64: "Datetime64", Ydb.Type_TIMESTAMP64: "Timestamp64", Ydb.Type_INTERVAL64: "Interval64",
	Ydb.Type_STRING: "String", Ydb.Type_UTF8: "Utf8", Ydb.Type_YSON: "Yson", Ydb.Type_JSON: "Json",
	Ydb.Type_UUID: "Uuid", Ydb.Type_JSON_DOCUMENT: "JsonDocument", Ydb.Type_DYNUMBER: "DyNumber",
}

// ColumnType preserves native spelling and nullability, including Decimal
// precision and scale. A storage type outside this grammar is refused.
func ColumnType(value *Ydb.Type) (string, bool, error) {
	if err := Known(value); err != nil {
		return "", false, err
	}
	if optional := value.GetOptionalType(); optional != nil {
		name, nullable, err := ColumnType(optional.GetItem())
		if err != nil {
			return "", false, err
		}
		if nullable {
			return "", false, fmt.Errorf("a storage column cannot be optional twice")
		}
		return name, true, nil
	}
	if decimal := value.GetDecimalType(); decimal != nil {
		if decimal.GetPrecision() == 0 || decimal.GetPrecision() > 35 || decimal.GetScale() > decimal.GetPrecision() {
			return "", false, fmt.Errorf("invalid YDB Decimal precision or scale")
		}
		return fmt.Sprintf("Decimal(%d,%d)", decimal.GetPrecision(), decimal.GetScale()), false, nil
	}
	if _, ok := value.GetType().(*Ydb.Type_TypeId); ok {
		if name, found := primitiveTypes[value.GetTypeId()]; found {
			return name, false, nil
		}
	}
	return "", false, fmt.Errorf("unsupported YDB storage column type")
}
