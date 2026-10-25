package ydbdecl

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// DefaultFamily is the name of the family every YDB row table has: it holds
// the key columns and every column no other family names.
const DefaultFamily = "default"

// The values COMPRESSION and CACHE_MODE take on a row table.
const (
	CompressionOff    = "off"
	CompressionLZ4    = "lz4"
	CacheModeRegular  = "regular"
	CacheModeInMemory = "in_memory"
)

// The attributes that declare a column family.
const (
	FamilyName        = "name"
	FamilyData        = "data"
	FamilyCompression = "compression"
	FamilyCacheMode   = "cache_mode"
	FamilyFields      = "fields"
)

// ColumnFamily is one family of a row table.
type ColumnFamily struct {
	Name         string   `json:"name"`
	Data         string   `json:"data,omitempty"`
	Compression  string   `json:"compression,omitempty"`
	CacheMode    string   `json:"cache_mode,omitempty"`
	KeepInMemory bool     `json:"keep_in_memory,omitempty"`
	Columns      []string `json:"columns,omitempty"`
}

// ColumnFamilies is the families a row table declares.
type ColumnFamilies struct {
	Families []ColumnFamily `json:"families"`
}

// MarshalJSON writes the families as the host's codec does: sorted by name,
// each with its columns sorted.
func (v ColumnFamilies) MarshalJSON() ([]byte, error) {
	type wire ColumnFamilies
	sorted := make([]ColumnFamily, 0, len(v.Families))
	for _, family := range v.Families {
		family.Columns = slices.Clone(family.Columns)
		slices.Sort(family.Columns)
		sorted = append(sorted, family)
	}
	slices.SortFunc(sorted, func(a, b ColumnFamily) int { return strings.Compare(a.Name, b.Name) })
	return json.Marshal(wire{Families: sorted})
}

// ParseFamily reads one column family out of values, keyed by attribute
// name, and ignores every key it does not name.
func ParseFamily(values map[string]string) (ColumnFamily, error) {
	spec := ColumnFamily{Name: strings.TrimSpace(values[FamilyName])}
	if spec.Name == "" {
		return ColumnFamily{}, &DeclarationError{Attribute: FamilyName, Reason: "a column family needs a name"}
	}
	if raw, ok := values[FamilyData]; ok {
		spec.Data = strings.TrimSpace(raw)
		if spec.Data == "" {
			return ColumnFamily{}, &DeclarationError{Attribute: FamilyData, Value: raw,
				Reason: "names the kind of storage pool the family is kept in, such as ssd; leave it out to keep " +
					"the database's own pool (YDB answers an empty one with `database doesn't have required storage pools`)"}
		}
	}
	var err error
	if spec.Compression, err = familyCompression(values); err != nil {
		return ColumnFamily{}, err
	}
	if spec.CacheMode, err = choice(values, FamilyCacheMode, []string{CacheModeRegular, CacheModeInMemory}); err != nil {
		return ColumnFamily{}, err
	}
	if raw, ok := values[FamilyFields]; ok {
		if spec.Columns, err = familyFields(raw); err != nil {
			return ColumnFamily{}, err
		}
	}
	if spec.Name == DefaultFamily && len(spec.Columns) > 0 {
		return ColumnFamily{}, &DeclarationError{Attribute: FamilyFields, Value: values[FamilyFields],
			Reason: "the default family holds the key and every column no other family names, so it lists none"}
	}
	return spec, nil
}

func familyCompression(values map[string]string) (string, error) {
	raw, ok := values[FamilyCompression]
	if !ok {
		return "", nil
	}
	if strings.EqualFold(strings.TrimSpace(raw), "zstd") {
		return "", &DeclarationError{Attribute: FamilyCompression, Value: raw,
			Reason: "takes off or lz4: YDB keeps zstd for column-oriented tables, and a row table answers " +
				"`Unsupported compression value 3`"}
	}
	return choice(values, FamilyCompression, []string{CompressionOff, CompressionLZ4})
}

// choice reads an attribute that takes one of allowed, in any case, and keeps
// it in lower case. An absent attribute is empty.
func choice(values map[string]string, attribute string, allowed []string) (string, error) {
	raw, ok := values[attribute]
	if !ok {
		return "", nil
	}
	value := strings.ToLower(strings.TrimSpace(raw))
	if !slices.Contains(allowed, value) {
		return "", &DeclarationError{Attribute: attribute, Value: raw, Reason: "takes " + strings.Join(allowed, " or ")}
	}
	return value, nil
}

func familyFields(raw string) ([]string, error) {
	var columns []string
	for part := range strings.SplitSeq(raw, ",") {
		column := strings.TrimSpace(part)
		switch {
		case column == "":
			return nil, &DeclarationError{Attribute: FamilyFields, Value: raw,
				Reason: "takes a comma-separated list of column names, none of them empty"}
		case slices.Contains(columns, column):
			return nil, &DeclarationError{Attribute: FamilyFields, Value: raw,
				Reason: fmt.Sprintf("names column %q twice", column)}
		}
		columns = append(columns, column)
	}
	return columns, nil
}

// ValidateFamilies refuses families YDB cannot hold, as the host refuses a
// desired column family declaration.
func ValidateFamilies(families []ColumnFamily) error {
	return invalidModel(kindColumnFamilies, validateFamilies(families))
}

func validateFamilies(families []ColumnFamily) error {
	names := make(map[string]bool, len(families))
	holder := make(map[string]string)
	for _, family := range families {
		if err := validateFamily(family); err != nil {
			return err
		}
		if names[family.Name] {
			return fmt.Errorf("%w: column family %q is listed twice", errInvalidValue, family.Name)
		}
		names[family.Name] = true
		for _, column := range family.Columns {
			if held, found := holder[column]; found {
				return fmt.Errorf("%w: column %q is in two column families, %q and %q", errInvalidValue, column, held, family.Name)
			}
			holder[column] = family.Name
		}
	}
	return nil
}

func validateFamily(family ColumnFamily) error {
	if strings.TrimSpace(family.Name) == "" {
		return fmt.Errorf("%w: a column family needs a name", errInvalidValue)
	}
	for _, text := range []struct{ field, value string }{
		{"column family name", family.Name}, {"column family storage pool", family.Data},
	} {
		if err := validText(text.field, text.value); err != nil {
			return err
		}
	}
	if family.Data != "" && strings.TrimSpace(family.Data) == "" {
		return fmt.Errorf("%w: column family %q names an empty storage pool kind", errInvalidValue, family.Name)
	}
	if !slices.Contains([]string{"", CompressionOff, CompressionLZ4}, family.Compression) {
		return fmt.Errorf("%w: column family %q has compression %q, which is not %s or %s",
			errInvalidValue, family.Name, family.Compression, CompressionOff, CompressionLZ4)
	}
	if !slices.Contains([]string{"", CacheModeRegular, CacheModeInMemory}, family.CacheMode) {
		return fmt.Errorf("%w: column family %q has cache mode %q, which is not %s or %s",
			errInvalidValue, family.Name, family.CacheMode, CacheModeRegular, CacheModeInMemory)
	}
	if family.Name == DefaultFamily && len(family.Columns) > 0 {
		return fmt.Errorf("%w: the default column family lists columns, and it holds every column no other family names",
			errInvalidValue)
	}
	seen := make(map[string]bool, len(family.Columns))
	for _, column := range family.Columns {
		if strings.TrimSpace(column) == "" {
			return fmt.Errorf("%w: column family %q names an empty column", errInvalidValue, family.Name)
		}
		if err := validText("column family column", column); err != nil {
			return err
		}
		if seen[column] {
			return fmt.Errorf("%w: column family %q names column %q twice", errInvalidValue, family.Name, column)
		}
		seen[column] = true
	}
	return nil
}
