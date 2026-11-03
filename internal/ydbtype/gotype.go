package ydbtype

import "strings"

// GoType is the Go type a struct field holding a YDB column is generated as:
// the type name as Go source writes it, and the import path that name needs,
// empty when it needs none.
type GoType struct {
	Name   string
	Import string
}

// decimalImport is the package that holds the one Go type ydb-go-sdk scans a
// Decimal into.
const decimalImport = "github.com/ydb-platform/ydb-go-sdk/v3/table/types"

// goTypes are the Go types of the YDB types a read reports, and of the Serial
// types a declaration may name. They are the host's embedded engine's types:
// each is what ydb-go-sdk's database/sql driver hands back when a value of
// the type is scanned into `any`. The deliberate exceptions are JSON, which
// the driver returns as bytes and which is generated as the string a JSON
// document is, and Decimal, which scans into the SDK's types.Decimal only.
var goTypes = map[string]GoType{
	Bool:         {Name: "bool"},
	Int8:         {Name: "int8"},
	Int16:        {Name: "int16"},
	Int32:        {Name: "int32"},
	Int64:        {Name: "int64"},
	Uint8:        {Name: "uint8"},
	Uint16:       {Name: "uint16"},
	Uint32:       {Name: "uint32"},
	Uint64:       {Name: "uint64"},
	Float:        {Name: "float32"},
	Double:       {Name: "float64"},
	DyNumber:     {Name: "string"},
	String:       {Name: "[]byte"},
	Utf8:         {Name: "string"},
	JSON:         {Name: "string"},
	JSONDocument: {Name: "string"},
	Yson:         {Name: "[]byte"},
	UUID:         {Name: "string"},
	Date:         {Name: "time.Time", Import: "time"},
	Datetime:     {Name: "time.Time", Import: "time"},
	Timestamp:    {Name: "time.Time", Import: "time"},
	Date32:       {Name: "time.Time", Import: "time"},
	Datetime64:   {Name: "time.Time", Import: "time"},
	Timestamp64:  {Name: "time.Time", Import: "time"},
	Interval:     {Name: "time.Duration", Import: "time"},
	Interval64:   {Name: "time.Duration", Import: "time"},
	SmallSerial:  {Name: "int16"},
	Serial:       {Name: "int32"},
	BigSerial:    {Name: "int64"},
}

// GoTypeOf answers the Go type a field holding a column of ydbType is
// generated as. ydbType is a YDB spelling as a read reports it and [Map]
// writes it: Utf8, Uint64, Decimal(22,9). It answers false for a name that is
// not a YDB type, including a declaration such as VARCHAR(255), which [Map]
// has to read first.
func GoTypeOf(ydbType string) (GoType, bool) {
	trimmed := strings.TrimSpace(ydbType)
	if base, _, ok := splitArguments(trimmed); ok && base == "Decimal" {
		return GoType{Name: "types.Decimal", Import: decimalImport}, true
	}
	goType, ok := goTypes[trimmed]
	return goType, ok
}
