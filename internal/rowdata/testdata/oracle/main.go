// Command ydbrowsoracle records what the Ptah host's embedded YDB engine
// answers for the row cases of the provider's corpora, so the provider's row
// services can be compared with it without importing host code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages that sit behind an internal boundary, so it has to be copied
// into a checkout of the host and run from there:
//
//	mkdir -p <host>/cmd/ydbrowsoracle
//	cp internal/rowdata/testdata/oracle/main.go <host>/cmd/ydbrowsoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbrowsoracle <plugin>)
//
// It writes, next to each input, a file of the same name with the suffix
// .golden.json:
//
//   - internal/rowdata/testdata/corpus/type-*.json: the column types and the
//     canonical rows internal/ydbdata.Rows.TypeRows gives, or its error;
//   - internal/rowdata/testdata/corpus/write-*.json: the statement
//     migration/datadiff renders for each write with the YDB data services, or
//     its error;
//   - internal/yqlquery/testdata/corpus/*.yql: the queries
//     internal/yqlquery.Split cuts the text into, or its error;
//
// and internal/rowdata/testdata/query-dialect.golden.json, the query-builder
// spelling of the YDB descriptor and the capabilities the builder assumes for
// YDB when its caller names no target.
//
// A value travels in the corpus as a JSON object with one key that names its
// case, as the provider contract's DataValue does: null, bool, int64, uint64
// (decimal text), double, string, bytes (base64), decimal (digits, precision,
// scale), timestamp (seconds, nanos), date (days since 1970-01-01), interval
// (months, days, nanoseconds), json and native. An empty object is a column the
// row does not carry. The program turns each value into the Go value the host
// compares and writes, and each canonical Go value back into the case of its
// column's kind; a column the host leaves untyped keeps the value it was given.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ptah.run/catalog"
	"ptah.run/core/platform"
	"ptah.run/core/platform/capability"
	"ptah.run/internal/ydbdata"
	"ptah.run/internal/ydbtype"
	"ptah.run/internal/yqlquery"
	"ptah.run/migration/datadiff"
)

// value is one tagged value.
type value map[string]json.RawMessage

type column struct {
	Name         string `json:"name"`
	DeclaredType string `json:"declared_type,omitempty"`
	Type         string `json:"type,omitempty"`
}

type typeCase struct {
	Capabilities map[string]bool `json:"capabilities"`
	LiveTable    []column        `json:"live_table"`
	Columns      []column        `json:"columns"`
	Declared     [][]value       `json:"declared"`
	Live         [][]value       `json:"live"`
}

type typeGolden struct {
	HostCommit string    `json:"host_commit"`
	Accepted   bool      `json:"accepted"`
	Error      string    `json:"error,omitempty"`
	Columns    []column  `json:"columns,omitempty"`
	Declared   [][]value `json:"declared,omitempty"`
	Live       [][]value `json:"live,omitempty"`
}

type write struct {
	ChangeID    string            `json:"change_id"`
	Mode        string            `json:"mode"`
	Schema      string            `json:"schema,omitempty"`
	Table       string            `json:"table"`
	Columns     []string          `json:"columns"`
	KeyColumns  []string          `json:"key_columns"`
	Values      []value           `json:"values"`
	ColumnTypes map[string]string `json:"column_types"`
}

type writeCase struct {
	Writes []write `json:"writes"`
}

type writeAnswer struct {
	ChangeID  string `json:"change_id"`
	Statement string `json:"statement,omitempty"`
	Error     string `json:"error,omitempty"`
	// Unrendered names a write mode the host's row diff writes no statement
	// for, an UPSERT. Statement is then the INSERT the host writes for the same
	// row, whose shape YQL's UPSERT INTO shares.
	Unrendered bool `json:"unrendered,omitempty"`
}

type writeGolden struct {
	HostCommit string        `json:"host_commit"`
	Writes     []writeAnswer `json:"writes"`
}

type query struct {
	Kind       string   `json:"kind"`
	Text       string   `json:"text"`
	Statements []string `json:"statements"`
	Mixed      bool     `json:"mixed,omitempty"`
}

type splitGolden struct {
	HostCommit string  `json:"host_commit"`
	Error      string  `json:"error,omitempty"`
	Queries    []query `json:"queries"`
}

type dialectGolden struct {
	HostCommit          string          `json:"host_commit"`
	Placeholder         string          `json:"placeholder"`
	Quoting             any             `json:"quoting"`
	Query               any             `json:"query"`
	DefaultCapabilities map[string]bool `json:"default_capabilities"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ydbrowsoracle <plugin>")
		os.Exit(2)
	}
	root := os.Args[1]
	commit := os.Getenv("HOST_COMMIT")
	if commit == "" {
		fmt.Fprintln(os.Stderr, "HOST_COMMIT is required")
		os.Exit(2)
	}
	steps := []func(string, string) error{recordTypes, recordWrites, recordSplits, recordDialect}
	for _, step := range steps {
		if err := step(root, commit); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func recordTypes(root, commit string) error {
	return eachInput(filepath.Join(root, "internal/rowdata/testdata/corpus"), "type-", ".json", func(path string, data []byte) (any, error) {
		var input typeCase
		if err := json.Unmarshal(data, &input); err != nil {
			return nil, err
		}
		flags := map[capability.Capability]bool{}
		for name, value := range input.Capabilities {
			flags[capability.Capability(name)] = value
		}
		var live *catalog.Table
		if input.LiveTable != nil {
			live = &catalog.Table{Name: "t"}
			for _, column := range input.LiveTable {
				live.Columns = append(live.Columns, catalog.Column{Name: column.Name, DataType: column.Type})
			}
		}
		declaredTypes := map[string]string{}
		for _, column := range input.Columns {
			if column.DeclaredType != "" {
				declaredTypes[column.Name] = column.DeclaredType
			}
		}
		desired, err := goRows(input.Columns, input.Declared)
		if err != nil {
			return nil, err
		}
		liveRows, err := goRows(input.Columns, input.Live)
		if err != nil {
			return nil, err
		}
		typed, err := ydbdata.Rows{}.TypeRows(capability.Of(flags), live, declaredTypes, desired, liveRows)
		if err != nil {
			return typeGolden{HostCommit: commit, Error: err.Error()}, nil
		}
		golden := typeGolden{HostCommit: commit, Accepted: true}
		for _, column := range input.Columns {
			golden.Columns = append(golden.Columns, column2(column.Name, typed.ColumnTypes[column.Name]))
		}
		if golden.Declared, err = taggedRows(input.Columns, typed.ColumnTypes, input.Declared, typed.Desired); err != nil {
			return nil, err
		}
		if golden.Live, err = taggedRows(input.Columns, typed.ColumnTypes, input.Live, typed.Live); err != nil {
			return nil, err
		}
		return golden, nil
	})
}

func column2(name, ydbType string) column { return column{Name: name, Type: ydbType} }

func recordWrites(root, commit string) error {
	return eachInput(filepath.Join(root, "internal/rowdata/testdata/corpus"), "write-", ".json", func(path string, data []byte) (any, error) {
		var input writeCase
		if err := json.Unmarshal(data, &input); err != nil {
			return nil, err
		}
		golden := writeGolden{HostCommit: commit}
		for _, w := range input.Writes {
			answer := writeAnswer{ChangeID: w.ChangeID}
			row := map[string]any{}
			for i, name := range w.Columns {
				value, carried, err := goValue(w.Values[i])
				if err != nil {
					return nil, err
				}
				if carried {
					row[name] = value
				}
			}
			diff := &datadiff.DataDiff{Schema: w.Schema, Table: w.Table, Keys: w.KeyColumns, ColumnTypes: w.ColumnTypes}
			switch w.Mode {
			case "INSERT":
				diff.Inserts = []datadiff.Row{row}
			case "UPSERT":
				answer.Unrendered = true
				diff.Inserts = []datadiff.Row{row}
			case "UPDATE_BY_KEY":
				key := map[string]any{}
				for _, name := range w.KeyColumns {
					key[name] = row[name]
				}
				diff.Updates = []datadiff.RowUpdate{{Key: key, Desired: row, Live: row}}
			case "DELETE_BY_KEY":
				diff.Deletes = []datadiff.Row{row}
			default:
				return nil, fmt.Errorf("unknown write mode %q", w.Mode)
			}
			up, _, err := datadiff.RenderStatements(diff, "ydb", ydbdata.Services())
			if err != nil {
				answer.Error = err.Error()
			} else {
				answer.Statement = up[0]
			}
			golden.Writes = append(golden.Writes, answer)
		}
		return golden, nil
	})
}

func recordSplits(root, commit string) error {
	return eachInput(filepath.Join(root, "internal/yqlquery/testdata/corpus"), "", ".yql", func(path string, data []byte) (any, error) {
		queries, err := yqlquery.Split(string(data))
		golden := splitGolden{HostCommit: commit, Queries: []query{}}
		if err != nil {
			golden.Error = err.Error()
			return golden, nil
		}
		for _, q := range queries {
			golden.Queries = append(golden.Queries, query{Kind: q.Kind.String(), Text: q.Text, Statements: q.Statements, Mixed: q.Mixed})
		}
		return golden, nil
	})
}

func recordDialect(root, commit string) error {
	described, ok := platform.Lookup("ydb")
	if !ok {
		return fmt.Errorf("the host describes no ydb dialect")
	}
	caps := capability.ForDialect("ydb")
	defaults := map[string]bool{}
	for _, key := range []capability.Capability{
		capability.CommonTableExpressions, capability.CorrelatedSubqueries, capability.NonEquiJoins,
		capability.OffsetWithoutLimit, capability.ReturningClause,
	} {
		if caps.Established(key) {
			defaults[string(key)] = caps.Has(key)
		}
	}
	golden := dialectGolden{HostCommit: commit, Placeholder: string(described.Placeholder), Quoting: described.Quoting,
		Query: described.Query, DefaultCapabilities: defaults}
	return writeJSON(filepath.Join(root, "internal/rowdata/testdata/query-dialect.golden.json"), golden)
}

// eachInput records the golden file of every input in dir whose name has the
// prefix and suffix.
func eachInput(dir, prefix, suffix string, record func(string, []byte) (any, error)) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) || strings.HasSuffix(name, ".golden.json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		golden, err := record(path, data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := writeJSON(strings.TrimSuffix(path, suffix)+".golden.json", golden); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("%s holds no %s*%s input", dir, prefix, suffix)
	}
	return nil
}

func writeJSON(path string, golden any) error {
	out, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func goRows(columns []column, rows [][]value) ([]map[string]any, error) {
	if rows == nil {
		return nil, nil
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i] = map[string]any{}
		for j, tagged := range row {
			value, carried, err := goValue(tagged)
			if err != nil {
				return nil, err
			}
			if carried {
				out[i][columns[j].Name] = value
			}
		}
	}
	return out, nil
}

func taggedRows(columns []column, types map[string]string, inputs [][]value, rows []map[string]any) ([][]value, error) {
	if rows == nil {
		return nil, nil
	}
	out := make([][]value, len(rows))
	for i, row := range rows {
		out[i] = make([]value, len(columns))
		for j, column := range columns {
			goValue, carried := row[column.Name]
			ydbType := types[column.Name]
			switch {
			case !carried:
				out[i][j] = value{}
			case ydbType == "":
				out[i][j] = inputs[i][j]
			default:
				tagged, err := tag(ydbType, goValue)
				if err != nil {
					return nil, err
				}
				out[i][j] = tagged
			}
		}
	}
	return out, nil
}

// goValue is the Go value a tagged value stands for.
func goValue(tagged value) (any, bool, error) {
	for name, raw := range tagged {
		switch name {
		case "null":
			return nil, true, nil
		case "bool":
			var v bool
			return v, true, json.Unmarshal(raw, &v)
		case "int64":
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, true, err
			}
			v, err := strconv.ParseInt(text, 10, 64)
			return v, true, err
		case "uint64":
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, true, err
			}
			v, err := strconv.ParseUint(text, 10, 64)
			return v, true, err
		case "double":
			var v float64
			return v, true, json.Unmarshal(raw, &v)
		case "string", "json":
			var v string
			return v, true, json.Unmarshal(raw, &v)
		case "bytes":
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, true, err
			}
			v, err := base64.StdEncoding.DecodeString(text)
			return v, true, err
		case "decimal":
			var v struct {
				Digits string `json:"digits"`
				Scale  uint32 `json:"scale"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, true, err
			}
			return decimalString(v.Digits, v.Scale), true, nil
		case "timestamp":
			var v struct {
				Seconds int64 `json:"seconds"`
				Nanos   int64 `json:"nanos"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, true, err
			}
			return time.Unix(v.Seconds, v.Nanos).UTC(), true, nil
		case "date":
			var days int64
			if err := json.Unmarshal(raw, &days); err != nil {
				return nil, true, err
			}
			return time.Unix(days*86400, 0).UTC(), true, nil
		case "interval":
			var v struct {
				Months      int64  `json:"months"`
				Days        int64  `json:"days"`
				Nanoseconds string `json:"nanoseconds"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, true, err
			}
			if v.Months != 0 {
				return nil, true, fmt.Errorf("a month has no YDB interval")
			}
			nanos, err := strconv.ParseInt(v.Nanoseconds, 10, 64)
			return time.Duration(v.Days)*24*time.Hour + time.Duration(nanos), true, err
		default:
			return nil, true, fmt.Errorf("the corpus holds a %s value the host has no Go value for", name)
		}
	}
	return nil, false, nil
}

func decimalString(digits string, scale uint32) string {
	negative := strings.HasPrefix(digits, "-")
	unsigned := strings.TrimPrefix(digits, "-")
	if scale > 0 {
		if pad := int(scale) + 1 - len(unsigned); pad > 0 {
			unsigned = strings.Repeat("0", pad) + unsigned
		}
		unsigned = unsigned[:len(unsigned)-int(scale)] + "." + unsigned[len(unsigned)-int(scale):]
	}
	if negative {
		return "-" + unsigned
	}
	return unsigned
}

// tag writes a canonical Go value of a column of ydbType as the case of the
// column's kind.
func tag(ydbType string, goValue any) (value, error) {
	raw := func(v any) value {
		out, _ := json.Marshal(v)
		return value{caseOf(ydbType, goValue): out}
	}
	switch typed := goValue.(type) {
	case nil:
		return value{"null": json.RawMessage("true")}, nil
	case int64:
		return raw(strconv.FormatInt(typed, 10)), nil
	case uint64:
		return raw(strconv.FormatUint(typed, 10)), nil
	case bool, float64:
		return raw(typed), nil
	case []byte:
		return raw(base64.StdEncoding.EncodeToString(typed)), nil
	case time.Duration:
		return raw(map[string]string{"nanoseconds": strconv.FormatInt(int64(typed), 10)}), nil
	case time.Time:
		utc := typed.UTC()
		if caseOf(ydbType, goValue) == "date" {
			return raw(utc.Unix() / 86400), nil
		}
		return raw(map[string]int64{"seconds": utc.Unix(), "nanos": int64(utc.Nanosecond())}), nil
	case string:
		if precision, scale, ok := ydbtype.DecimalArguments(ydbType); ok {
			digits, err := scaled(typed, scale)
			if err != nil {
				return nil, err
			}
			return raw(map[string]any{"digits": digits, "precision": precision, "scale": scale}), nil
		}
		return raw(typed), nil
	default:
		return nil, fmt.Errorf("no tagged form for a Go %T", goValue)
	}
}

// caseOf is the case a canonical value of a column of ydbType takes.
func caseOf(ydbType string, goValue any) string {
	switch goValue.(type) {
	case int64:
		return "int64"
	case uint64:
		return "uint64"
	case bool:
		return "bool"
	case float64:
		return "double"
	case []byte:
		return "bytes"
	case time.Duration:
		return "interval"
	case time.Time:
		if ydbType == ydbtype.Date || ydbType == ydbtype.Date32 {
			return "date"
		}
		return "timestamp"
	}
	if _, _, ok := ydbtype.DecimalArguments(ydbType); ok {
		return "decimal"
	}
	if ydbType == ydbtype.JSON || ydbType == ydbtype.JSONDocument {
		return "json"
	}
	return "string"
}

func scaled(text string, scale int) (string, error) {
	negative := strings.HasPrefix(text, "-")
	whole, fraction, _ := strings.Cut(strings.TrimPrefix(text, "-"), ".")
	if len(fraction) > scale {
		return "", fmt.Errorf("the decimal %q does not fit scale %d", text, scale)
	}
	number, ok := new(big.Int).SetString(whole+fraction+strings.Repeat("0", scale-len(fraction)), 10)
	if !ok {
		return "", fmt.Errorf("the decimal %q is not digits", text)
	}
	if negative {
		number.Neg(number)
	}
	return number.String(), nil
}
