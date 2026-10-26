package externals

import (
	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// SameDataSource reports whether declared and current describe the same data
// source on a database whose root is root: the source type, location and auth
// method as written, and the options by upper-case name, with a secret's path
// compared relative to root, as the server stores it absolute.
func SameDataSource(declared, current ydbdecl.ExternalDataSource, root string) bool {
	return declared.SourceType == current.SourceType && declared.Location == current.Location && declared.AuthMethod == current.AuthMethod &&
		equalOptions(canonicalSourceOptions(declared.Options, root), canonicalSourceOptions(current.Options, root))
}

// SameTable reports whether declared and current describe the same external
// table on a database whose root is root: the data source path, the location,
// the columns in order, and the options, with PARTITIONED_BY compared as the
// list of names it holds.
func SameTable(declared, current ydbdecl.ExternalTable, root string) bool {
	return externalpath.RelativePath(declared.DataSource, root) == externalpath.RelativePath(current.DataSource, root) && declared.Location == current.Location &&
		slices.EqualFunc(declared.Columns, current.Columns, sameColumn) &&
		equalOptions(canonicalTableOptions(declared.Options), canonicalTableOptions(current.Options))
}

// sameColumn compares a column's type without letter case or spaces: YQL
// reads `utf8` and `Decimal(22, 9)` as `Utf8` and `Decimal(22,9)`.
func sameColumn(a, b ydbdecl.ExternalColumn) bool {
	return a.Name == b.Name && a.NotNull == b.NotNull && canonicalType(a.Type) == canonicalType(b.Type)
}

func canonicalType(name string) string {
	return strings.ToUpper(strings.Join(strings.Fields(name), ""))
}

func canonicalSourceOptions(options map[string]string, root string) map[string]string {
	canonical := make(map[string]string, len(options))
	for name, value := range options {
		name = strings.ToUpper(name)
		if externalpath.IsSecretOption(name) {
			value = externalpath.RelativePath(value, root)
		}
		canonical[name] = value
	}
	return canonical
}

func canonicalTableOptions(options map[string]string) map[string]string {
	canonical := make(map[string]string, len(options))
	for name, value := range options {
		name = strings.ToUpper(name)
		if name == optionPartitionedBy {
			value = compactList(value)
		}
		canonical[name] = value
	}
	return canonical
}

func equalOptions(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if other, ok := b[name]; !ok || other != value {
			return false
		}
	}
	return true
}

// Displaces reports a data source change the external tables over the source
// cannot stay through: a drop, a recreation on a target without CREATE OR
// REPLACE, and a replacement with a new source type, which CREATE OR REPLACE
// refuses while a table reads the source (measured on 26.2.1.14).
func Displaces(current, desired *ydbdecl.ExternalDataSource, replace bool) bool {
	if current == nil {
		return false
	}
	return desired == nil || !replace || current.SourceType != desired.SourceType
}

// Recreates reports a data source change made by dropping the source and
// creating it again: a target without CREATE OR REPLACE.
func Recreates(current, desired *ydbdecl.ExternalDataSource, replace bool) bool {
	return current != nil && desired != nil && !replace
}
