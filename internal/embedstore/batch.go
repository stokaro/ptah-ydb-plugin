package embedstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// The text a generation's state column holds for each kind of write, the
// host's (embedrun.WriteKind).
var writeKinds = map[protocol.EmbeddingWriteKind]string{
	protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_UPSERT:    "upsert",
	protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_SKIP:      "skip",
	protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_TOMBSTONE: "tombstone",
}

// kindOf reads a stored state back, UNSPECIFIED for one nothing wrote.
func kindOf(state string) protocol.EmbeddingWriteKind {
	for kind, text := range writeKinds {
		if text == state {
			return kind
		}
	}
	return protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_UNSPECIFIED
}

// targetKey is the key a target row is addressed by: the source key, as the
// target table holds it.
func (s *Store) targetKey(ctx context.Context, spec *protocol.EmbeddingSpec) ([]keyColumn, RowTable, error) {
	name := targetPath(spec)
	described, exists, err := s.describe(ctx, name)
	if err != nil {
		return nil, RowTable{}, err
	}
	if !exists {
		return nil, RowTable{}, refuse(protocol.EmbeddingRefusal_ABSENT, "target table %s does not exist; prepare it first", name)
	}
	fields := spec.GetSource().GetKeyFields()
	if ownsTable(spec) {
		owned := append(append([]string{}, fields...), spec.GetTarget().GetColumn()+ordinalSuffix)
		key, err := keyOf(described, owned, name)
		if err != nil {
			return nil, RowTable{}, err
		}
		return key[:len(fields)], described, nil
	}
	key, err := keyOf(described, fields, name)
	return key, described, err
}

// held reads what the target holds at keys: the generation, hash, version and
// state of each key's first stored row. A row whose generation is NULL was
// never written and is absent from the answer.
func held(ctx context.Context, db queryer, spec *protocol.EmbeddingSpec, key []keyColumn, keys []*protocol.EmbeddingKey) ([]*protocol.EmbeddingHeldTarget, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	names, tuples, arguments := keyTuples(key, keys, nil)
	selected := make([]string, 0, len(key)+4)
	for _, column := range key {
		selected = append(selected, asText("", column.name))
	}
	column := spec.GetTarget().GetColumn()
	for _, suffix := range metadataSuffixes() {
		selected = append(selected, Quote(column+suffix))
	}
	first := ""
	if ownsTable(spec) {
		first = " AND " + Quote(column+ordinalSuffix) + " = 0"
	}
	order := make([]string, len(key))
	for i, part := range key {
		order[i] = Quote(part.name)
	}
	// #nosec G201 -- names from the specification through Quote; key values
	// are parameters.
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)%s ORDER BY %s", strings.Join(selected, ", "), Quote(targetPath(spec)),
		names, tuples, first, strings.Join(order, ", "))
	rows, err := db.QueryContext(ctx, query, named(arguments...)...)
	if err != nil {
		if retryable(err) {
			return nil, err
		}
		return nil, fmt.Errorf("read the target rows: %s", statusName(err))
	}
	defer func() { _ = rows.Close() }()
	var states []*protocol.EmbeddingHeldTarget
	for rows.Next() {
		values := make([]sql.NullString, len(key)+4)
		destinations := make([]any, len(values))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("read the target rows: %s", statusName(err))
		}
		generation := values[len(key)]
		if !generation.Valid {
			continue
		}
		components := make([]string, len(key))
		for i := range key {
			components[i] = values[i].String
		}
		states = append(states, &protocol.EmbeddingHeldTarget{Key: &protocol.EmbeddingKey{Components: components},
			Generation: generation.String, InputHash: values[len(key)+1].String, Version: values[len(key)+2].String,
			Kind: kindOf(values[len(key)+3].String)})
	}
	if err := rows.Err(); err != nil {
		if retryable(err) {
			return nil, err
		}
		return nil, fmt.Errorf("read the target rows: %s", statusName(err))
	}
	return states, nil
}

// batchState reads the run and the held state of keys, and the token over
// both.
func batchState(ctx context.Context, db queryer, spec *protocol.EmbeddingSpec, key []keyColumn, runID string, keys []*protocol.EmbeddingKey) (*protocol.EmbeddingBatchState, error) {
	runs, err := readRows(ctx, db, runLayout, "WHERE "+Quote("id")+" = $p1", 1, scannedRun, runID)
	if err != nil {
		return nil, err
	}
	state := &protocol.EmbeddingBatchState{RunId: runID}
	if len(runs) > 0 {
		state.Run = runs[0]
	}
	if state.Held, err = held(ctx, db, spec, key, keys); err != nil {
		return nil, err
	}
	state.Token, err = batchToken(state)
	return state, err
}

// batchToken is a digest of the run, absent or not, and the held states.
func batchToken(state *protocol.EmbeddingBatchState) (string, error) {
	return provider.EmbeddingStateToken(&protocol.EmbeddingBatchState{RunId: state.GetRunId(), Run: state.GetRun(), Held: state.GetHeld()})
}

// ReadBatchState returns the run and what the target holds at a batch's keys,
// with a token over both, read in one transaction.
func (s *Store) ReadBatchState(ctx context.Context, request *protocol.ReadEmbeddingBatchStateRequest) (*protocol.EmbeddingBatchState, error) {
	exists, err := s.presence(ctx)
	switch {
	case err != nil:
		return nil, err
	case !exists:
		return nil, ErrStoreAbsent
	}
	key, _, err := s.targetKey(ctx, request.GetSpec())
	if err != nil {
		return nil, err
	}
	var state *protocol.EmbeddingBatchState
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		var readErr error
		state, readErr = batchState(ctx, tx, request.GetSpec(), key, request.GetRunId(), request.GetKeys())
		return readErr
	})
	return state, err
}

// CommitBatch makes each written key's stored rows equal to its set and
// writes the checkpointed run, in one transaction, when the token the state
// was read with still holds. A write into a generation whose index exists is
// refused on a line whose vector index does not take in later writes: the
// index would go on answering as it stood at the build while the table moved
// on.
func (s *Store) CommitBatch(ctx context.Context, request *protocol.CommitEmbeddingBatchRequest) *protocol.EmbeddingWriteResult {
	spec := request.GetSpec()
	exists, err := s.presence(ctx)
	switch {
	case err != nil:
		return outcome(request.GetRequestId(), err)
	case !exists:
		return outcome(request.GetRequestId(), ErrStoreAbsent)
	}
	key, described, err := s.targetKey(ctx, spec)
	if err != nil {
		return outcome(request.GetRequestId(), err)
	}
	if _, indexed := described.Index(spec.GetNames().GetIndex()); indexed && len(request.GetWrites()) > 0 && !s.caps.MaintainedOnWrite {
		return outcome(request.GetRequestId(), refuse(protocol.EmbeddingRefusal_UNSUPPORTED,
			"this YDB server has no vector_index_maintained_on_write, and generation %s's index exists, so a write now would "+
				"leave the index answering as it stood at the build", short(spec.GetGeneration())))
	}
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		state, err := batchState(ctx, tx, spec, key, request.GetRunId(), request.GetKeys())
		if err != nil {
			return err
		}
		if state.GetToken() != request.GetExpectedToken() {
			return errConflict
		}
		for _, set := range sets(request.GetWrites()) {
			if err := s.applySet(ctx, tx, spec, key, set); err != nil {
				return err
			}
		}
		values, err := runValues(request.GetRun())
		if err != nil {
			return err
		}
		return upsert(ctx, tx, runLayout, values)
	})
	result := outcome(request.GetRequestId(), err)
	if err == nil {
		result.Written = &protocol.EmbeddingRecords{Runs: []*protocol.EmbeddingRun{normalRun(request.GetRun())}}
	}
	return result
}

// sets groups a batch's writes into one set per key, in the order they come;
// the SDK holds a key's writes adjacent and in ordinal order.
func sets(writes []*protocol.EmbeddingTargetWrite) [][]*protocol.EmbeddingTargetWrite {
	var grouped [][]*protocol.EmbeddingTargetWrite
	for i, write := range writes {
		if i > 0 && keyID(write.GetKey().GetComponents()) == keyID(writes[i-1].GetKey().GetComponents()) {
			grouped[len(grouped)-1] = append(grouped[len(grouped)-1], write)
			continue
		}
		grouped = append(grouped, []*protocol.EmbeddingTargetWrite{write})
	}
	return grouped
}

// applySet makes one key's stored rows equal to its set: the members are
// written and, in a table of the generation's own, the rows past its end are
// removed. A tombstone there removes the key's rows rather than writing one,
// because YDB has no foreign key to remove them when the source row goes.
func (s *Store) applySet(ctx context.Context, tx *sql.Tx, spec *protocol.EmbeddingSpec, key []keyColumn, set []*protocol.EmbeddingTargetWrite) error {
	if ownsTable(spec) && set[0].GetKind() == protocol.EmbeddingWriteKind_EMBEDDING_WRITE_KIND_TOMBSTONE {
		return removeFrom(ctx, tx, spec, key, set[0].GetKey(), 0)
	}
	for _, member := range set {
		statement, arguments := writeStatement(spec, key, member)
		if _, err := tx.ExecContext(ctx, statement, named(arguments...)...); err != nil {
			if retryable(err) {
				return err
			}
			return fmt.Errorf("write a target row: %s", statusName(err))
		}
	}
	if !ownsTable(spec) {
		return nil
	}
	return removeFrom(ctx, tx, spec, key, set[0].GetKey(), len(set))
}

// removeFrom deletes a key's stored rows from ordinal from onwards.
func removeFrom(ctx context.Context, tx *sql.Tx, spec *protocol.EmbeddingSpec, key []keyColumn, at *protocol.EmbeddingKey, from int) error {
	arguments := make([]any, 0, len(key)+1)
	for _, component := range at.GetComponents() {
		arguments = append(arguments, component)
	}
	arguments = append(arguments, int64(from))
	// #nosec G201 -- names from the specification, through Quote.
	statement := fmt.Sprintf("DELETE FROM %s WHERE %s AND %s >= $p%d", Quote(targetPath(spec)), keyMatch("", key, 1),
		Quote(spec.GetTarget().GetColumn()+ordinalSuffix), len(arguments))
	if _, err := tx.ExecContext(ctx, statement, named(arguments...)...); err != nil {
		if retryable(err) {
			return err
		}
		return fmt.Errorf("remove the surplus chunks of a key: %s", statusName(err))
	}
	return nil
}

// writeStatement renders one row's write in the shape the layout needs, as
// the host does. On the source's own columns it is an UPDATE by the key: the
// row is there and the vector is what is missing, and a key the source no
// longer has updates nothing rather than creating a row. In a table of the
// generation's own it is an UPSERT whose key is selected out of the source,
// so a write for a source row that is gone stores nothing.
func writeStatement(spec *protocol.EmbeddingSpec, key []keyColumn, write *protocol.EmbeddingTargetWrite) (string, []any) {
	column := spec.GetTarget().GetColumn()
	arguments := []any{write.GetGeneration(), write.GetInputHash(), optionalText(write.GetVersion()), writeKinds[write.GetKind()]}
	vector := "CAST(NULL AS String)"
	if len(write.GetVector()) > 0 {
		arguments = append(arguments, EncodeVector(write.GetVector()))
		vector = fmt.Sprintf("$p%d", len(arguments))
	}
	first := len(arguments) + 1
	for _, component := range write.GetKey().GetComponents() {
		arguments = append(arguments, component)
	}
	if !ownsTable(spec) {
		// #nosec G201 -- names from the specification, through Quote.
		statement := fmt.Sprintf("UPDATE %s SET %s = %s, %s = $p1, %s = $p2, %s = $p3, %s = $p4 WHERE %s", Quote(targetPath(spec)),
			Quote(column), vector, Quote(column+generationSuffix), Quote(column+inputHashSuffix), Quote(column+versionSuffix),
			Quote(column+stateSuffix), keyMatch("", key, first))
		return statement, arguments
	}
	arguments = append(arguments, int64(write.GetOrdinal()))
	columns := make([]string, 0, len(key)+6)
	selected := make([]string, 0, len(key)+6)
	for _, part := range key {
		columns = append(columns, Quote(part.name))
		selected = append(selected, qualified(sourceAlias, part.name)+" AS "+Quote(part.name))
	}
	columns = append(columns, Quote(column+ordinalSuffix), Quote(column), Quote(column+generationSuffix),
		Quote(column+inputHashSuffix), Quote(column+versionSuffix), Quote(column+stateSuffix))
	selected = append(selected, fmt.Sprintf("$p%d AS %s", len(arguments), Quote(column+ordinalSuffix)), vector+" AS "+Quote(column),
		"$p1 AS "+Quote(column+generationSuffix), "$p2 AS "+Quote(column+inputHashSuffix),
		"$p3 AS "+Quote(column+versionSuffix), "$p4 AS "+Quote(column+stateSuffix))
	// #nosec G201 -- names from the specification, through Quote.
	statement := fmt.Sprintf("UPSERT INTO %s (%s) SELECT %s FROM %s AS %s WHERE %s", Quote(targetPath(spec)), strings.Join(columns, ", "),
		strings.Join(selected, ", "), Quote(sourcePath(spec)), sourceAlias, keyMatch(sourceAlias, key, first))
	return statement, arguments
}

// sourceAlias names the source table in an own-table write.
const sourceAlias = "src"
