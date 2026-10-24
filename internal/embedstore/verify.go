package embedstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The verification walk reads both sides of the comparison in one read per
// page, as the host's embedded engine reads it in one: one table read once
// when both sides are one table, and a full join of the two otherwise, so a
// stored row whose key the source no longer has is seen. Equal keys arrive
// adjacent, ordered by the key and the chunk ordinal, and a page never splits
// a key's rows. The width of a stored vector is computed by the server from
// the length of its bytes, so the vectors themselves are not read.

// The names the joined walk gives what each side selects, so neither side
// publishes a name the specification chose and could collide with.
const (
	scopeAlias          = "ptah_in_scope"
	presenceAlias       = "ptah_target_present"
	generationAlias     = "ptah_generation"
	inputHashAlias      = "ptah_input_hash"
	targetVersionAlias  = "ptah_target_version"
	stateAlias          = "ptah_state"
	storedWidthAlias    = "ptah_stored_width"
	storedOrdinalAlias  = "ptah_stored_ordinal"
	keyAliasPrefix      = "ptah_key_"
	inputFieldPrefix    = "ptah_input_"
	sourceVersionPrefix = "ptah_source_version_"
)

func aliasAt(prefix string, index int) string { return prefix + strconv.Itoa(index) }

// storedWidth is the element count of the float vector in a column: four
// bytes an element and the tag. NULL stays NULL.
func storedWidth(alias, column string) string {
	return "CAST((Length(" + qualified(alias, column) + ") - 1) / 4 AS Int64)"
}

// versionFields names the column a source version is read from, if any.
func versionFields(source *protocol.EmbeddingSource) []string {
	if field := strings.TrimSpace(source.GetVersionField()); field != "" {
		return []string{field}
	}
	return nil
}

// oneRelationWalk reads both sides off one table, the rows the filter asks
// for and every row a generation wrote, after the key a page ended at.
func oneRelationWalk(spec *protocol.EmbeddingSpec, key []keyColumn, after []string, limit int) (string, []any) {
	source := spec.GetSource()
	columns := sourceColumns(source, "")
	column := spec.GetTarget().GetColumn()
	for _, suffix := range metadataSuffixes() {
		columns = append(columns, Quote(column+suffix))
	}
	inScope := "true"
	filter := filterOf(source)
	if filter != "" {
		inScope = filter
	}
	columns = append(columns, storedWidth("", column), "CAST(0 AS Int64)", inScope, "true")
	order := make([]string, len(key))
	for i, part := range key {
		order[i] = Quote(part.name)
	}
	var conditions []string
	if filter != "" {
		conditions = append(conditions, "("+filter+" OR "+Quote(column+generationSuffix)+" IS NOT NULL)")
	}
	var arguments []any
	if after != nil {
		casts := make([]string, len(key))
		for i, part := range key {
			arguments = append(arguments, after[i])
			casts[i] = part.cast(len(arguments))
		}
		conditions = append(conditions, fmt.Sprintf("(%s) > (%s)", strings.Join(order, ", "), strings.Join(casts, ", ")))
	}
	// #nosec G201 -- names from the specification, through Quote; the filter
	// is the specification's own YQL.
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(columns, ", "), Quote(sourcePath(spec)))
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	return query + " ORDER BY " + strings.Join(order, ", ") + " LIMIT " + strconv.Itoa(limit), arguments
}

// joinedWalk reads the two tables in one full join on the key as text, which
// is how the write path addresses a row. A position only the target has is a
// stored row the source no longer asks for; one only the source has is a row
// with nothing stored yet.
func joinedWalk(spec *protocol.EmbeddingSpec, after []string, limit int) (string, []any) {
	source := spec.GetSource()
	keyCount := len(source.GetKeyFields())
	columns := make([]string, 0, keyCount+len(source.GetInputFields())+9)
	on := make([]string, 0, keyCount)
	order := make([]string, 0, keyCount+1)
	merged := make([]string, 0, keyCount)
	for i := range keyCount {
		alias := aliasAt(keyAliasPrefix, i)
		coalesced := fmt.Sprintf("COALESCE(s.%s, t.%s)", alias, alias)
		columns = append(columns, coalesced+" AS "+alias)
		on = append(on, fmt.Sprintf("s.%s = t.%s", alias, alias))
		order = append(order, alias)
		merged = append(merged, coalesced)
	}
	for i := range source.GetInputFields() {
		columns = append(columns, "s."+aliasAt(inputFieldPrefix, i))
	}
	for i := range versionFields(source) {
		columns = append(columns, "s."+aliasAt(sourceVersionPrefix, i))
	}
	columns = append(columns, "t."+generationAlias, "t."+inputHashAlias, "t."+targetVersionAlias, "t."+stateAlias, "t."+storedWidthAlias,
		fmt.Sprintf("COALESCE(t.%s, 0) AS %s", storedOrdinalAlias, storedOrdinalAlias),
		fmt.Sprintf("COALESCE(s.%s, false) AS %s", scopeAlias, scopeAlias),
		fmt.Sprintf("COALESCE(t.%s, false) AS %s", presenceAlias, presenceAlias))
	if ownsTable(spec) {
		order = append(order, storedOrdinalAlias)
	}
	conditions := []string{fmt.Sprintf("(COALESCE(s.%s, false) OR t.%s IS NOT NULL)", scopeAlias, generationAlias)}
	var arguments []any
	if after != nil {
		placeholders := make([]string, len(after))
		for i, component := range after {
			arguments = append(arguments, component)
			placeholders[i] = "$p" + strconv.Itoa(len(arguments))
		}
		conditions = append(conditions, fmt.Sprintf("(%s) > (%s)", strings.Join(merged, ", "), strings.Join(placeholders, ", ")))
	}
	// #nosec G201 -- names from the specification through Quote, and this
	// package's own aliases.
	query := fmt.Sprintf("SELECT %s FROM %s AS s FULL JOIN %s AS t ON %s WHERE %s ORDER BY %s LIMIT %d",
		strings.Join(columns, ", "), verificationSource(spec), verificationTarget(spec), strings.Join(on, " AND "),
		strings.Join(conditions, " AND "), strings.Join(order, ", "), limit)
	return query, arguments
}

// verificationSource is the source half of the join.
func verificationSource(spec *protocol.EmbeddingSpec) string {
	source := spec.GetSource()
	selected := make([]string, 0, len(source.GetKeyFields())+len(source.GetInputFields())+2)
	for i, field := range source.GetKeyFields() {
		selected = append(selected, asText("", field)+" AS "+aliasAt(keyAliasPrefix, i))
	}
	for i, field := range source.GetInputFields() {
		selected = append(selected, asText("", field)+" AS "+aliasAt(inputFieldPrefix, i))
	}
	for i, field := range versionFields(source) {
		selected = append(selected, asText("", field)+" AS "+aliasAt(sourceVersionPrefix, i))
	}
	scope := "true"
	if filter := filterOf(source); filter != "" {
		scope = filter
	}
	selected = append(selected, scope+" AS "+scopeAlias)
	return fmt.Sprintf("(SELECT %s FROM %s)", strings.Join(selected, ", "), Quote(sourcePath(spec)))
}

// verificationTarget is the target half: the key as text, the four state
// columns, the width of the vector, its chunk ordinal, and a literal saying a
// row stands there at all.
func verificationTarget(spec *protocol.EmbeddingSpec) string {
	column := spec.GetTarget().GetColumn()
	selected := make([]string, 0, len(spec.GetSource().GetKeyFields())+7)
	for i, field := range spec.GetSource().GetKeyFields() {
		selected = append(selected, asText("", field)+" AS "+aliasAt(keyAliasPrefix, i))
	}
	ordinal := "CAST(0 AS Int64)"
	if ownsTable(spec) {
		ordinal = Quote(column + ordinalSuffix)
	}
	selected = append(selected,
		Quote(column+generationSuffix)+" AS "+generationAlias, Quote(column+inputHashSuffix)+" AS "+inputHashAlias,
		Quote(column+versionSuffix)+" AS "+targetVersionAlias, Quote(column+stateSuffix)+" AS "+stateAlias,
		storedWidth("", column)+" AS "+storedWidthAlias, ordinal+" AS "+storedOrdinalAlias, "true AS "+presenceAlias)
	return fmt.Sprintf("(SELECT %s FROM %s)", strings.Join(selected, ", "), Quote(targetPath(spec)))
}

// ReadVerification reads one page of the walk, at most max rows, ending at a
// key boundary. A key holding more rows than a page is an error.
func (s *Store) ReadVerification(ctx context.Context, request *protocol.ReadEmbeddingVerificationRequest) (*protocol.EmbeddingVerificationPage, error) {
	spec := request.GetSpec()
	var after []string
	if token := request.GetPageToken(); token != "" {
		if err := json.Unmarshal([]byte(token), &after); err != nil || len(after) != len(spec.GetSource().GetKeyFields()) {
			return nil, fmt.Errorf("the verification page token is not one this provider issued")
		}
	}
	bound := int(request.GetMaxRows())
	var query string
	var arguments []any
	if oneRelation(spec) {
		key, err := s.sourceKeyOf(ctx, spec.GetSource())
		if err != nil {
			return nil, err
		}
		query, arguments = oneRelationWalk(spec, key, after, bound+1)
	} else {
		query, arguments = joinedWalk(spec, after, bound+1)
	}
	rows, err := s.walk(ctx, spec, query, arguments)
	if err != nil {
		return nil, err
	}
	page := &protocol.EmbeddingVerificationPage{Complete: true}
	if len(rows) <= bound {
		page.Rows = rows
		return page, nil
	}
	next := keyID(rows[bound].GetKey().GetComponents())
	end := bound
	for end > 0 && keyID(rows[end-1].GetKey().GetComponents()) == next {
		end--
	}
	if end == 0 {
		return nil, fmt.Errorf("one key holds more than %d stored rows, more than a verification page holds", bound)
	}
	token, err := json.Marshal(rows[end-1].GetKey().GetComponents())
	if err != nil {
		return nil, err
	}
	page.Rows, page.NextPageToken, page.Complete = rows[:end], string(token), false
	return page, nil
}

// walk runs one page of the walk and reads its rows.
func (s *Store) walk(ctx context.Context, spec *protocol.EmbeddingSpec, query string, arguments []any) ([]*protocol.EmbeddingVerificationRow, error) {
	result, err := s.db.QueryContext(ctx, query, named(arguments...)...)
	if err != nil {
		return nil, fmt.Errorf("read %s for verification: %s", targetPath(spec), statusName(err))
	}
	defer func() { _ = result.Close() }()
	source := spec.GetSource()
	keyCount, inputCount, versions := len(source.GetKeyFields()), len(source.GetInputFields()), len(versionFields(source))
	texts := make([]sql.NullString, keyCount+inputCount+versions+len(metadataSuffixes()))
	var width, ordinal sql.NullInt64
	var inScope, present sql.NullBool
	destinations := make([]any, 0, len(texts)+4)
	for i := range texts {
		destinations = append(destinations, &texts[i])
	}
	destinations = append(destinations, &width, &ordinal, &inScope, &present)
	var rows []*protocol.EmbeddingVerificationRow
	for result.Next() {
		if err := result.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("read a verification row: %s", statusName(err))
		}
		row := &protocol.EmbeddingVerificationRow{Key: &protocol.EmbeddingKey{}}
		for i := range keyCount {
			row.Key.Components = append(row.Key.Components, texts[i].String)
		}
		if inScope.Valid && inScope.Bool {
			row.Source = &protocol.EmbeddingVerificationSource{}
			for i := range inputCount {
				row.Source.Fields = append(row.Source.Fields, textValue(texts[keyCount+i]))
			}
			if versions > 0 {
				row.Source.Version = texts[keyCount+inputCount].String
			}
		}
		if present.Valid && present.Bool {
			offset := keyCount + inputCount + versions
			row.Stored = &protocol.EmbeddingStoredRow{Generation: texts[offset].String, InputHash: texts[offset+1].String,
				Version: texts[offset+2].String, Kind: kindOf(texts[offset+3].String),
				Ordinal: uint32(max(ordinal.Int64, 0))} // #nosec G115 -- a chunk ordinal
			if width.Valid && width.Int64 > 0 {
				row.Stored.Dimension = uint32(min(width.Int64, 1<<32-1)) // #nosec G115 -- clamped into range on this line
			}
		}
		if row.Source == nil && row.Stored == nil {
			continue
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read %s for verification: %s", targetPath(spec), statusName(err))
	}
	return rows, nil
}
