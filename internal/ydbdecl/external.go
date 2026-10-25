package ydbdecl

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The attributes that name an external object's parts in a refusal.
const (
	externalName       = "name"
	externalSchema     = "schema"
	externalSourceType = "source_type"
	externalLocation   = "location"
	externalAuthMethod = "auth_method"
	externalOptions    = "options"
	externalDataSource = "data_source"
	externalColumns    = "columns"
)

// The options an external object writes with an attribute of its own.
var (
	dataSourceReserved = []string{"SOURCE_TYPE", "LOCATION", "AUTH_METHOD"}
	tableReserved      = []string{"DATA_SOURCE", "LOCATION"}
)

// ExternalError is a part of an external object a declaration cannot carry.
type ExternalError struct {
	Attribute string
	Reason    string
}

func (e *ExternalError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Attribute, e.Reason)
}

// ExternalDataSource is an external data source as the host's spec encodes
// it.
type ExternalDataSource struct {
	SourceType string            `json:"source_type"`
	Location   string            `json:"location,omitempty"`
	AuthMethod string            `json:"auth_method"`
	Options    map[string]string `json:"options,omitempty"`
}

// ExternalColumn is one column of an external table.
type ExternalColumn struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null,omitempty"`
}

// ExternalTable is an external table as the host's spec encodes it.
type ExternalTable struct {
	DataSource string            `json:"data_source"`
	Location   string            `json:"location"`
	Columns    []ExternalColumn  `json:"columns"`
	Options    map[string]string `json:"options,omitempty"`
}

// CheckExternalOptions checks option names, upper-cases them, refuses the
// ones written with an attribute of their own and REFERENCES, and trims each
// value.
func CheckExternalOptions(options map[string]string, reserved ...string) (map[string]string, error) {
	names := slices.Sorted(maps.Keys(options))
	if len(names) == 0 {
		return nil, nil
	}
	checked := make(map[string]string, len(names))
	for _, written := range names {
		name := strings.ToUpper(strings.TrimSpace(written))
		switch {
		case name == "" || !optionName(name):
			return nil, &ExternalError{Attribute: externalOptions,
				Reason: fmt.Sprintf("%q is not an option name: letters, digits and underscores", name)}
		case slices.Contains(reserved, name):
			return nil, &ExternalError{Attribute: externalOptions,
				Reason: fmt.Sprintf("%s is written with its own attribute, %s", name, strings.ToLower(name))}
		case name == "REFERENCES":
			return nil, &ExternalError{Attribute: externalOptions,
				Reason: "REFERENCES lists the external tables over a data source, and the server keeps it"}
		}
		if _, repeated := checked[name]; repeated {
			return nil, &ExternalError{Attribute: externalOptions, Reason: name + " is written twice"}
		}
		checked[name] = strings.TrimSpace(options[written])
	}
	return checked, nil
}

func optionName(name string) bool {
	for _, r := range name {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// CheckExternalColumns refuses an external table without a column, a column
// without a name or a type, and a column written twice.
func CheckExternalColumns(columns []ExternalColumn) error {
	if len(columns) == 0 {
		return &ExternalError{Attribute: externalColumns, Reason: "an external table needs a column"}
	}
	seen := make(map[string]bool, len(columns))
	for _, column := range columns {
		switch {
		case strings.TrimSpace(column.Name) == "":
			return &ExternalError{Attribute: externalColumns, Reason: "a column has no name"}
		case strings.TrimSpace(column.Type) == "":
			return &ExternalError{Attribute: externalColumns, Reason: "column " + column.Name + " has no type"}
		case seen[column.Name]:
			return &ExternalError{Attribute: externalColumns, Reason: column.Name + " is written twice"}
		}
		seen[column.Name] = true
	}
	return nil
}

// ValidateDataSource refuses a data source YDB cannot keep.
func ValidateDataSource(s ExternalDataSource) error {
	switch {
	case strings.TrimSpace(s.SourceType) == "":
		return &ExternalError{Attribute: externalSourceType, Reason: "a data source needs a source_type, such as ObjectStorage or PostgreSQL"}
	case strings.TrimSpace(s.AuthMethod) == "":
		return &ExternalError{Attribute: externalAuthMethod, Reason: "a data source needs an auth_method, such as NONE or BASIC"}
	}
	return checkCanonicalOptions(s.Options, dataSourceReserved)
}

// ValidateExternalTable refuses an external table YDB cannot keep.
func ValidateExternalTable(t ExternalTable) error {
	switch {
	case strings.TrimSpace(t.DataSource) == "":
		return &ExternalError{Attribute: externalDataSource, Reason: "an external table needs a data_source, the path of the data source it reads"}
	case strings.TrimSpace(t.Location) == "":
		return &ExternalError{Attribute: externalLocation, Reason: "an external table needs a location"}
	}
	if err := CheckExternalColumns(t.Columns); err != nil {
		return err
	}
	return checkCanonicalOptions(t.Options, tableReserved)
}

func checkCanonicalOptions(options map[string]string, reserved []string) error {
	checked, err := CheckExternalOptions(options, reserved...)
	if err != nil {
		return err
	}
	for name := range options {
		if _, kept := checked[name]; !kept {
			return &ExternalError{Attribute: externalOptions, Reason: fmt.Sprintf("option name %q is kept in upper case", name)}
		}
	}
	return nil
}

// ParseExternalPath reads the path of an external object relative to the
// database root.
func ParseExternalPath(written string) (Path, error) {
	schema, name, err := splitPath(written)
	if err != nil {
		return Path{}, fmt.Errorf("%q is not a path (dir/name): %w: %w", written, errInvalidValue, err)
	}
	if !pathIdentity(schema, name) {
		return Path{}, fmt.Errorf("%q is not a path (dir/name): %w: an external object requires a schema-scoped YDB identity", written, errInvalidValue)
	}
	return Path{Schema: schema, Name: name}, nil
}

// ExternalPath checks the directory and name an external object is declared
// at and returns them as the host keeps them.
func ExternalPath(schema, name string) (Path, error) {
	name = strings.TrimSpace(name)
	schema = strings.Trim(strings.TrimSpace(schema), "/")
	switch {
	case name == "":
		return Path{}, &ExternalError{Attribute: externalName, Reason: "an external object needs a name"}
	case strings.Contains(name, "/"):
		return Path{}, &ExternalError{Attribute: externalName,
			Reason: fmt.Sprintf("%q holds a slash; name the directory with %s", name, externalSchema)}
	case !pathIdentity("", name):
		return Path{}, &ExternalError{Attribute: externalName, Reason: fmt.Sprintf("%q is not a path segment", name)}
	case schema != "" && !pathIdentity(schema, name):
		return Path{}, &ExternalError{Attribute: externalSchema,
			Reason: fmt.Sprintf("%q is not a directory path relative to the database root", schema)}
	}
	return Path{Schema: schema, Name: name}, nil
}

// ExternalDisplay names an external object by its path, for messages.
func ExternalDisplay(schema, name string) string {
	if schema == "" {
		return name
	}
	return schema + "/" + name
}
