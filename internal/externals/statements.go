package externals

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// Creation is how a statement creates an external object: CREATE, or CREATE
// OR REPLACE, which replaces one that exists at the path.
type Creation string

// The two creations.
const (
	Create  Creation = "CREATE"
	Replace Creation = "CREATE OR REPLACE"
)

// The reasons a statement on an external object needs review, the host's
// words. Neither object holds data in YDB, so none of them loses data YDB
// stores.
const (
	// DropSourceReason is why dropping a data source needs review.
	DropSourceReason = "DROP EXTERNAL DATA SOURCE removes what YDB reads another system through; no data YDB stores is lost"
	// DropTableReason is why dropping an external table needs review.
	DropTableReason = "DROP EXTERNAL TABLE removes the columns YDB reads files through; the files stay"
	// ReplaceReason is why replacing an external object needs review.
	ReplaceReason = "CREATE OR REPLACE changes what queries reading the external object read"
)

// CreateDataSourceStatement writes the creation of a data source at path, one
// quoted YQL path relative to the database root or absolute.
func CreateDataSourceStatement(path string, source ydbdecl.ExternalDataSource, creation Creation) string {
	settings := []string{optionSetting(optionSourceType, source.SourceType)}
	if source.Location != "" {
		settings = append(settings, optionSetting(optionLocation, source.Location))
	}
	settings = append(settings, optionSetting(optionAuthMethod, source.AuthMethod))
	settings = append(settings, sortedSettings(source.Options)...)
	return fmt.Sprintf("%s EXTERNAL DATA SOURCE %s WITH (\n    %s\n);", creation, ydbtype.QuoteIdentifier(path), strings.Join(settings, ",\n    "))
}

// CreateTableStatement writes the creation of an external table at path.
func CreateTableStatement(path string, table ydbdecl.ExternalTable, creation Creation) string {
	columns := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		definition := ydbtype.QuoteIdentifier(column.Name) + " " + column.Type
		if column.NotNull {
			definition += " NOT NULL"
		}
		columns = append(columns, definition)
	}
	settings := []string{optionSetting(optionDataSource, table.DataSource), optionSetting(optionLocation, table.Location)}
	settings = append(settings, sortedSettings(table.Options)...)
	return fmt.Sprintf("%s EXTERNAL TABLE %s (\n    %s\n) WITH (\n    %s\n);", creation, ydbtype.QuoteIdentifier(path),
		strings.Join(columns, ",\n    "), strings.Join(settings, ",\n    "))
}

// DropDataSourceStatement writes what drops the data source at path.
func DropDataSourceStatement(path string) string {
	return "DROP EXTERNAL DATA SOURCE " + ydbtype.QuoteIdentifier(path) + ";"
}

// DropTableStatement writes what drops the external table at path.
func DropTableStatement(path string) string {
	return "DROP EXTERNAL TABLE " + ydbtype.QuoteIdentifier(path) + ";"
}

func optionSetting(name, value string) string {
	return name + " = " + ydbtype.StringLiteral(value)
}

func sortedSettings(options map[string]string) []string {
	settings := make([]string, 0, len(options))
	for _, name := range slices.Sorted(maps.Keys(options)) {
		settings = append(settings, optionSetting(name, options[name]))
	}
	return settings
}
