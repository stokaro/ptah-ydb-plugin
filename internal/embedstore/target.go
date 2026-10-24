package embedstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// sourceKey reads the source table's key and refuses a specification whose
// key fields are not its primary key, or a source that is not there.
func (s *Store) sourceKey(ctx context.Context, spec *protocol.EmbeddingSpec) ([]keyColumn, error) {
	described, exists, err := s.describe(ctx, sourcePath(spec))
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, refuse(protocol.EmbeddingRefusal_ABSENT, "source table %s does not exist", sourcePath(spec))
	}
	return keyOf(described, spec.GetSource().GetKeyFields(), sourcePath(spec))
}

// PrepareTarget creates a generation's storage, or in VERIFY mode checks that
// a prepared one is still complete. Both are idempotent: YDB has no ADD COLUMN
// IF NOT EXISTS, so the table is described and only what is missing is added.
// A schema statement runs outside any transaction, so a preparation that
// failed after its first statement is UNKNOWN; running it again completes it.
func (s *Store) PrepareTarget(ctx context.Context, request *protocol.PrepareEmbeddingTargetRequest) *protocol.EmbeddingWriteResult {
	spec := request.GetSpec()
	if err := checkTarget(spec); err != nil {
		return outcome(request.GetRequestId(), err)
	}
	if request.GetMode() == protocol.PrepareEmbeddingTargetRequest_VERIFY {
		return outcome(request.GetRequestId(), s.verifyTarget(ctx, spec))
	}
	if _, err := s.sourceKey(ctx, spec); err != nil {
		return outcome(request.GetRequestId(), err)
	}
	if ownsTable(spec) {
		return outcome(request.GetRequestId(), s.prepareOwnedTable(ctx, spec))
	}
	return outcome(request.GetRequestId(), s.prepareColumns(ctx, spec))
}

// prepareColumns adds a generation's five columns to the target table and
// comments its vector column. A column of the right name and another type is
// refused rather than written into, and so is a column that holds another
// generation's vectors: two generations over one table need two columns,
// which is what makes a cutover a pointer move rather than a data migration.
func (s *Store) prepareColumns(ctx context.Context, spec *protocol.EmbeddingSpec) error {
	name := targetPath(spec)
	described, exists, err := s.describe(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return refuse(protocol.EmbeddingRefusal_ABSENT, "target table %s does not exist, so the generation's column has nowhere to go", name)
	}
	if _, err := keyOf(described, spec.GetSource().GetKeyFields(), name); err != nil {
		return err
	}
	var additions []string
	for _, wanted := range generationColumns(spec.GetTarget().GetColumn()) {
		present, found := described.Column(wanted.Name)
		switch {
		case !found:
			additions = append(additions, "ADD COLUMN "+Quote(wanted.Name)+" "+wanted.Type)
		case present.Type != wanted.Type:
			return refuse(protocol.EmbeddingRefusal_NOT_OWNED, "column %s on %s is %s, and a generation needs it to be %s",
				wanted.Name, name, present.Type, wanted.Type)
		}
	}
	if len(additions) > 0 {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+Quote(name)+" "+strings.Join(additions, ", ")); err != nil {
			return fmt.Errorf("create the target columns: %s", statusName(err))
		}
		if comment := spec.GetNames().GetColumnComment(); comment != "" {
			if err := s.setAttribute(ctx, name, columnCommentKey+spec.GetTarget().GetColumn(), comment); err != nil {
				return fmt.Errorf("%w: comment the target column: %s", errUndetermined, statusName(err))
			}
		}
	}
	return s.refuseAnotherGeneration(ctx, spec)
}

// refuseAnotherGeneration refuses a column that holds another generation's
// vectors, before any embedding is paid for.
func (s *Store) refuseAnotherGeneration(ctx context.Context, spec *protocol.EmbeddingSpec) error {
	column := Quote(spec.GetTarget().GetColumn() + generationSuffix)
	// #nosec G201 -- names from the specification, through Quote.
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL AND %s != $p1 LIMIT 1", column, Quote(targetPath(spec)), column, column)
	var occupant string
	switch err := s.db.QueryRowContext(ctx, query, named(spec.GetGeneration())...).Scan(&occupant); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read the target generation: %s", statusName(err))
	}
	return refuse(protocol.EmbeddingRefusal_NOT_OWNED,
		"column %q on %s holds generation %s, and this request is generation %s: a generation writes its own column so the "+
			"previous one is still there to go back to. Give this one its own target.column", spec.GetTarget().GetColumn(),
		targetPath(spec), short(occupant), short(spec.GetGeneration()))
}

// short is the first twelve characters of a generation's identity, as the
// host shows one.
func short(identity string) string {
	return identity[:min(len(identity), 12)]
}

// ownedColumns are a table of the generation's own, in the order it declares
// them: the source key with its types, the chunk ordinal, then the five
// generation columns.
func ownedColumns(key []keyColumn, column string) []RowColumn {
	columns := make([]RowColumn, 0, len(key)+6)
	for _, part := range key {
		columns = append(columns, RowColumn{Name: part.name, Type: part.ydbType, Nullable: part.nullable})
	}
	columns = append(columns, RowColumn{Name: column + ordinalSuffix, Type: "Int64"})
	return append(columns, generationColumns(column)...)
}

// createOwnedTable renders the CREATE TABLE of a table of the generation's
// own, keyed by the source key and the chunk ordinal.
func createOwnedTable(spec *protocol.EmbeddingSpec, key []keyColumn) string {
	column := spec.GetTarget().GetColumn()
	definitions := make([]string, 0, len(key)+6)
	for _, c := range ownedColumns(key, column) {
		definition := Quote(c.Name) + " " + c.Type
		if !c.Nullable {
			definition += " NOT NULL"
		}
		definitions = append(definitions, definition)
	}
	primary := make([]string, 0, len(key)+1)
	for _, part := range key {
		primary = append(primary, Quote(part.name))
	}
	primary = append(primary, Quote(column+ordinalSuffix))
	return fmt.Sprintf("CREATE TABLE %s (%s, PRIMARY KEY (%s))", Quote(targetPath(spec)), strings.Join(definitions, ", "), strings.Join(primary, ", "))
}

// prepareOwnedTable creates the table a chunked generation stores its sets in
// and marks it as the generation's. YDB runs CREATE TABLE and the mark as two
// scheme operations, so a preparation stopped between them leaves a table
// without one; such a table is adopted only when it is empty and has exactly
// the columns this generation creates, and any other unmarked table is
// refused as somebody else's.
func (s *Store) prepareOwnedTable(ctx context.Context, spec *protocol.EmbeddingSpec) error {
	key, err := s.sourceKey(ctx, spec)
	if err != nil {
		return err
	}
	name := targetPath(spec)
	mark := spec.GetNames().GetOwnershipMark()
	described, exists, err := s.describe(ctx, name)
	if err != nil {
		return err
	}
	created := false
	switch found := described.Attributes[ownCommentKey]; {
	case !exists:
		if _, err := s.db.ExecContext(ctx, createOwnedTable(spec, key)); err != nil {
			return fmt.Errorf("create the target table %s: %s", name, statusName(err))
		}
		created = true
	case found == mark:
	case found != "" || !slices.Equal(described.Columns, ownedColumns(key, spec.GetTarget().GetColumn())):
		return refuse(protocol.EmbeddingRefusal_NOT_OWNED,
			"table %s is not Ptah's: a generation of layout own_table creates its own table and drops it at retirement, "+
				"and this one carries the comment %q rather than %q", name, found, mark)
	default:
		empty, err := s.isEmpty(ctx, name)
		if err != nil {
			return err
		}
		if !empty {
			return refuse(protocol.EmbeddingRefusal_NOT_OWNED,
				"table %s has the shape a generation creates but no comment naming one, and it holds rows, so it is not Ptah's to adopt", name)
		}
	}
	if err := s.setAttribute(ctx, name, ownCommentKey, mark); err != nil {
		if created {
			return fmt.Errorf("%w: mark the target table %s as the generation's: %s", errUndetermined, name, statusName(err))
		}
		return fmt.Errorf("mark the target table %s as the generation's: %s", name, statusName(err))
	}
	return nil
}

// isEmpty reports whether a table holds no rows.
func (s *Store) isEmpty(ctx context.Context, name string) (bool, error) {
	var count uint64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+Quote(name)).Scan(&count); err != nil {
		return false, fmt.Errorf("count the rows of %s: %s", name, statusName(err))
	}
	return count == 0, nil
}

// verifyTarget checks that a prepared generation's columns are still there,
// which an idempotent prepare retry has to establish beyond the recorded
// state.
func (s *Store) verifyTarget(ctx context.Context, spec *protocol.EmbeddingSpec) error {
	name := targetPath(spec)
	described, exists, err := s.describe(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return refuse(protocol.EmbeddingRefusal_ABSENT, "prepared target table %s is absent", name)
	}
	for _, column := range generationColumns(spec.GetTarget().GetColumn()) {
		if _, found := described.Column(column.Name); !found {
			return refuse(protocol.EmbeddingRefusal_ABSENT, "prepared target column %s.%s is absent", name, column.Name)
		}
	}
	return nil
}

// BuildIndex builds the index a specification declares unless it is there
// and ready. An index that exists and is not ready is one whose build did not
// finish; it is dropped and built again. The build is ALTER TABLE ... ADD
// INDEX, which returns once YDB has built the index over every row; a build
// whose call ended without an answer is UNKNOWN.
func (s *Store) BuildIndex(ctx context.Context, request *protocol.BuildEmbeddingIndexRequest) *protocol.EmbeddingWriteResult {
	spec := request.GetSpec()
	built, err := s.buildIndex(ctx, spec)
	answer := outcome(request.GetRequestId(), err)
	if err == nil {
		answer.IndexOutcome = built
	}
	return answer
}

func (s *Store) buildIndex(ctx context.Context, spec *protocol.EmbeddingSpec) (protocol.EmbeddingWriteResult_IndexOutcome, error) {
	if err := checkTarget(spec); err != nil {
		return 0, err
	}
	if strings.TrimSpace(spec.GetTarget().GetIndexMethod()) == "" {
		return protocol.EmbeddingWriteResult_NOT_DECLARED, nil
	}
	if !s.caps.VectorIndexes {
		return 0, refuse(protocol.EmbeddingRefusal_UNSUPPORTED,
			"this YDB server has no vector_indexes, and building the generation's %s index needs it; YDB 25.1 builds one with "+
				"the EnableVectorIndex feature flag on", VectorMethod)
	}
	settings, err := settingsOf(spec)
	if err != nil {
		return 0, err
	}
	name := targetPath(spec)
	described, exists, err := s.describe(ctx, name)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, refuse(protocol.EmbeddingRefusal_ABSENT, "target table %s does not exist; prepare it first", name)
	}
	index := spec.GetNames().GetIndex()
	built := protocol.EmbeddingWriteResult_BUILT
	if found, present := described.Index(index); present {
		if found.Ready {
			return protocol.EmbeddingWriteResult_ALREADY_VALID, nil
		}
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+Quote(name)+" DROP INDEX "+Quote(index)); err != nil {
			return 0, fmt.Errorf("drop the index %s that did not finish building: %s", index, statusName(err))
		}
		built = protocol.EmbeddingWriteResult_REBUILT
	}
	statement := fmt.Sprintf("ALTER TABLE %s ADD INDEX %s GLOBAL USING %s ON (%s) %s", Quote(name), Quote(index), VectorMethod,
		Quote(spec.GetTarget().GetColumn()), settings.clause())
	if _, err := s.db.ExecContext(ctx, statement); err != nil {
		if ctx.Err() != nil || built == protocol.EmbeddingWriteResult_REBUILT {
			return 0, fmt.Errorf("%w: build the index %s: %s", errUndetermined, index, statusName(err))
		}
		return 0, fmt.Errorf("build the index %s: %s", index, statusName(err))
	}
	return built, nil
}

// searchSettings is the setting that decides what an indexed search returns.
// Ptah does not set it, so a search runs under the server's default.
const searchSettings = "ydb.KMeansTreeSearchTopSize=(server default)"

// ReadStructure reads back a registered generation's storage. A String
// column declares no width, so the dimension is the index's vector_dimension
// where the index is there and the generation's own otherwise. The vector
// functions are part of YDB, so vector support is always present.
func (s *Store) ReadStructure(ctx context.Context, request *protocol.ReadEmbeddingStructureRequest) (*protocol.EmbeddingStructure, error) {
	location := request.GetLocation()
	structure := &protocol.EmbeddingStructure{VectorSupport: true, SearchSettings: searchSettings}
	name := relationPath(location.GetTable())
	described, exists, err := s.describe(ctx, name)
	if err != nil {
		return nil, err
	}
	structure.TableExists = exists
	if column, found := described.Column(location.GetColumn()); found {
		structure.ColumnExists, structure.ColumnType, structure.Dimension = true, column.Type, location.GetDimension()
		if index, present := described.Index(location.GetNames().GetIndex()); present {
			structure.IndexExists, structure.IndexValid, structure.IndexMethod = true, index.Ready, "GLOBAL"
			if index.Vector != nil {
				structure.IndexMethod, structure.IndexOperator, structure.Dimension = VectorMethod, index.Vector.Metric, index.Vector.Dimension
			}
		}
	}
	if request.GetCountRows() {
		structure.RowsCounted = true
		if structure.ColumnExists {
			if structure.Rows, err = s.countGeneration(ctx, name, location); err != nil {
				return nil, err
			}
		}
	}
	return structure, nil
}

// countGeneration counts the rows a generation holds.
func (s *Store) countGeneration(ctx context.Context, name string, location *protocol.EmbeddingLocation) (int64, error) {
	// #nosec G201 -- names from the registry, through Quote.
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = $p1", Quote(name), Quote(location.GetColumn()+generationSuffix))
	var count uint64
	if err := s.db.QueryRowContext(ctx, query, named(location.GetGeneration())...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count the rows of generation %s: %s", short(location.GetGeneration()), statusName(err))
	}
	return int64(min(count, 1<<63-1)), nil // #nosec G115 -- clamped into range on this line
}

// DropGeneration drops what a retirement destroys, each step skipped when its
// object is already gone. Ownership is checked before anything is dropped: a
// table goes only when its comment is the ownership mark, and columns only
// when they hold no other generation's rows and, where both the column and the
// request carry a comment, the two are the same. A retirement holds the
// registry record rather than the specification, so its request may name no
// column comment. The index goes first, because YDB refuses
// to drop a column an index covers, and the five columns go in one statement.
func (s *Store) DropGeneration(ctx context.Context, request *protocol.DropEmbeddingGenerationRequest) *protocol.EmbeddingWriteResult {
	return outcome(request.GetRequestId(), s.dropGeneration(ctx, request.GetLocation(), request.GetStorage()))
}

func (s *Store) dropGeneration(ctx context.Context, location *protocol.EmbeddingLocation, storage protocol.DropEmbeddingGenerationRequest_Storage) error {
	name := relationPath(location.GetTable())
	described, exists, err := s.describe(ctx, name)
	if err != nil || !exists {
		return err
	}
	column := location.GetColumn()
	var drops []string
	switch storage {
	case protocol.DropEmbeddingGenerationRequest_TABLE:
		if found, mark := described.Attributes[ownCommentKey], location.GetNames().GetOwnershipMark(); found != mark {
			return refuse(protocol.EmbeddingRefusal_NOT_OWNED,
				"table %s is not Ptah's to drop: retiring generation %s would remove it and every row in it, and it carries the "+
					"comment %q rather than %q. Drop it by hand if it really is this generation's storage", name, short(location.GetGeneration()), found, mark)
		}
	case protocol.DropEmbeddingGenerationRequest_COLUMNS:
		if found, wanted := described.Attributes[columnCommentKey+column], location.GetNames().GetColumnComment(); found != "" && wanted != "" && found != wanted {
			return refuse(protocol.EmbeddingRefusal_NOT_OWNED, "column %s on %s carries the comment %q rather than %q, so it is not this generation's to drop",
				column, name, found, wanted)
		}
		if _, found := described.Column(column + generationSuffix); found {
			if err := s.refuseAnotherGeneration(ctx, &protocol.EmbeddingSpec{Generation: location.GetGeneration(),
				Target: &protocol.EmbeddingTarget{Table: location.GetTable(), Column: column}}); err != nil {
				return err
			}
		}
		for _, wanted := range generationColumns(column) {
			if _, found := described.Column(wanted.Name); found {
				drops = append(drops, "DROP COLUMN "+Quote(wanted.Name))
			}
		}
	}
	done := 0
	if _, found := described.Index(location.GetNames().GetIndex()); found {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+Quote(name)+" DROP INDEX "+Quote(location.GetNames().GetIndex())); err != nil {
			return fmt.Errorf("drop index %s: %s", location.GetNames().GetIndex(), statusName(err))
		}
		done++
	}
	partial := func(err error) error {
		if done > 0 {
			return fmt.Errorf("%w: %w; run the drop again to finish", errUndetermined, err)
		}
		return err
	}
	switch {
	case storage == protocol.DropEmbeddingGenerationRequest_TABLE:
		if _, err := s.db.ExecContext(ctx, "DROP TABLE "+Quote(name)); err != nil {
			return partial(fmt.Errorf("drop the target table %s: %s", name, statusName(err)))
		}
	case len(drops) > 0:
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+Quote(name)+" "+strings.Join(drops, ", ")); err != nil {
			return partial(fmt.Errorf("drop the columns of generation %s: %s", short(location.GetGeneration()), statusName(err)))
		}
		done++
		// The comment on the vector column is an attribute of the table, so it
		// outlives the column unless it is removed with it.
		if _, found := described.Attributes[columnCommentKey+column]; found {
			if err := s.setAttribute(ctx, name, columnCommentKey+column, ""); err != nil {
				return partial(fmt.Errorf("remove the comment of column %s: %s", column, statusName(err)))
			}
		}
	}
	return nil
}
