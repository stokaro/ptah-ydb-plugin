package tablecreate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// DropOperationKind identifies the removal of a table and its rows: a row
// table with its indexes and changefeeds, or a column table.
const DropOperationKind = schema.ProviderID + "/table-drop-operation"

// The stores a dropped table is held in.
const (
	RowStore    = "row"
	ColumnStore = "column"
)

var dropModel = buildDropModel()

// DropModel binds the removal payload grammar, its precondition and its
// confirmation.
func DropModel() *protocol.ValueModel { return proto.CloneOf(dropModel) }

func buildDropModel() *protocol.ValueModel {
	raw, err := json.Marshal(map[string]any{
		"kind": DropOperationKind, "representation": "operation", "schema_version": 1, "encoding": "json",
		"payload":      "store: row or column; columns: the names of the table's columns in order; key: the table's key columns",
		"execution":    "YQL DROP TABLE through the query service once, outside SQL transactions; no retries; session and absolute path bound at dispatch",
		"precondition": "the path must hold a table of the captured store with exactly the captured columns, in order, and the captured key columns",
		"confirmation": "the path must be gone from its directory",
		"semantics":    "destructive: the table's rows, indexes and changefeeds go with it",
		"database":     "an explicit owner database must match the selected session; an empty database binds to that session",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: DropOperationKind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: 1, Encoding: "json",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: DropOperationKind + "/json-v1"}
}

// changefeedDropRefusal is why table cannot be dropped, or "", for a
// changefeed it holds: one the read cannot represent, or one an asynchronous
// replication of another database controls.
//
// A changefeed the read cannot represent may be a stream a replication
// controls through a binding Ptah does not read, or hold consumers whose
// settings Ptah cannot say, and a reverse plan could not create it again, so
// the drop is refused, as the host's embedded engine refused it. Dropping the
// changefeed by hand first leaves a table Ptah reads whole.
//
// A replication reads the table through its stream, and YDB drops such a
// table and leaves the replication failing (`Discovery for all topics
// failed`), measured on 25.1.4.7 and 26.2.1.14. A replication lives in its
// replicas' database, and the stream names the replica it writes: one of
// another database is one no plan of this database drops, so the removal
// waits until it goes, as the host's embedded engine refused it. A replica in
// this database belongs to a replication the planner of replications reads
// on both sides, and it refuses the drop where the plan keeps the
// replication; a plan that drops both drops the replication first, with
// CASCADE, which releases the stream.
func changefeedDropRefusal(request *protocol.PlanFeaturesRequest, owner *protocol.Reference, table *protocol.PlanningTable) (string, error) {
	for _, record := range table.GetCurrent().GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() != schema.ChangefeedProperties || record.GetValue() == nil || !forThisEngine(record.GetTargets()) {
			continue
		}
		observed, err := normalizedFacet(record, protocol.ValueInput_OBSERVED)
		if err != nil {
			return "", err
		}
		_, held, err := changefeedSets(nil, observed)
		if err != nil {
			return "", err
		}
		for _, stream := range held.GetChangefeeds() {
			if _, err := changefeeds.FromObserved(stream); err != nil {
				return fmt.Sprintf("table %q: changefeed %q is not fully described: %v, so a plan cannot say what dropping the table "+
					"takes with it; drop the changefeed by hand first", tableLabel(owner), stream.GetDescription().GetName(), err), nil
			}
			replica := changefeeds.ReplicaPath(stream.GetDescription().GetAttributes())
			if replica == "" || inDatabase(replica, request.GetDatabasePath()) {
				continue
			}
			return fmt.Sprintf("table %q: changefeed %q is a replication-managed stream that depends on this table; "+
				"its replication's controller must release it before the table is dropped, and the replication, "+
				"which writes %s, lives in another database; drop it there first, or keep the table",
				tableLabel(owner), stream.GetDescription().GetName(), replica), nil
		}
	}
	return "", nil
}

// inDatabase reports whether the absolute path lies below the database at
// root. A root the request does not name holds nothing.
func inDatabase(absolute, root string) bool {
	root = strings.TrimRight(root, "/")
	return root != "" && strings.HasPrefix(absolute, root+"/")
}

// columnTable reports whether the read of table describes a column table,
// which holds a column storage facet.
func columnTable(table *protocol.PlanningTable) bool {
	for _, record := range table.GetCurrent().GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() == schema.ColumnStorageProperties && forThisEngine(record.GetTargets()) {
			return true
		}
	}
	return false
}

// Drop is a decoded removal: what the table holds when it is planned, which
// execution checks before it drops the table, so a plan never drops a table
// that took the path since.
type Drop struct {
	Store string `json:"store"`
	// Columns are the names of the table's columns, in order.
	Columns []string `json:"columns"`
	// Key are the table's key columns, in key order where the read reports
	// one.
	Key []string `json:"key"`
}

// validate refuses a removal that does not say what it drops.
func (d Drop) validate() error {
	if d.Store != RowStore && d.Store != ColumnStore {
		return fmt.Errorf("a YDB table removal names a row or a column store")
	}
	if len(d.Columns) == 0 || len(d.Key) == 0 {
		return fmt.Errorf("a YDB table removal states the table's columns and key")
	}
	names := make(map[string]bool, len(d.Columns))
	for _, column := range d.Columns {
		if !validName(column) || names[column] {
			return fmt.Errorf("a YDB table removal states each column once")
		}
		names[column] = true
	}
	for i, key := range d.Key {
		if !names[key] || slices.Contains(d.Key[:i], key) {
			return fmt.Errorf("a YDB table removal's key names its columns once each")
		}
	}
	return nil
}

// CaptureDrop builds the removal of the table owner names, which holds what
// observed reads: its columns, its key and, through its facets, its store.
// The key is in key order where the read of the table's properties states it,
// and in column order otherwise.
func CaptureDrop(id string, owner *protocol.Reference, observed *protocol.ObservedTable) (*protocol.NormalizedValue, error) {
	drop := Drop{Store: RowStore}
	for _, column := range observed.GetColumns() {
		drop.Columns = append(drop.Columns, column.GetName())
		if column.GetPrimaryKey() {
			drop.Key = append(drop.Key, column.GetName())
		}
	}
	for _, record := range observed.GetFeatures().GetFacets().GetRecords() {
		switch record.GetKind() {
		case schema.ColumnStorageProperties:
			drop.Store = ColumnStore
		case schema.TableProperties:
			var held native.DescribeTableResult
			if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &held); err != nil {
				return nil, err
			}
			if len(held.GetPrimaryKey()) != 0 {
				drop.Key = slices.Clone(held.GetPrimaryKey())
			}
		}
	}
	return CaptureDropShape(id, owner, drop)
}

// CaptureDropShape builds the removal of the table owner names, which holds
// what drop states.
func CaptureDropShape(id string, owner *protocol.Reference, drop Drop) (*protocol.NormalizedValue, error) {
	payload, err := json.Marshal(drop)
	if err != nil {
		return nil, err
	}
	definition := DropModel()
	return NormalizeDrop(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: DropOperationKind, Owner: proto.CloneOf(owner), SchemaVersion: 1, Encoding: definition.Encoding,
		DefinitionSha256: definition.DefinitionSha256, Payload: payload}})
}

// NormalizeDrop reconstructs a removal's safety facts from its payload.
func NormalizeDrop(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value, definition := input.GetValue(), DropModel()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != schema.ProviderID || value.GetKind() != definition.Kind ||
		value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB table removal does not match its negotiated operation model")
	}
	if err := validateOwner(value.Owner); err != nil {
		return nil, err
	}
	drop, err := decodeDrop(value.Payload)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(drop)
	if err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(result.Owner),
		CanonicalFormat: definition.CanonicalFormat, CanonicalProjection: bytes.Clone(payload),
		OperationSemantics: &protocol.OperationSemantics{Impact: protocol.ChangeSemantics_DESTRUCTIVE, Reason: "Drops a table and its rows",
			Access: protocol.ChangeSemantics_NARROWS, AccessReason: "The table's permissions go with it"}}, nil
}

// DecodeDrop returns the removal an envelope holds after validating it.
func DecodeDrop(value *protocol.Envelope) (*Drop, error) {
	normalized, err := NormalizeDrop(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return nil, err
	}
	return decodeDrop(normalized.Value.Payload)
}

func decodeDrop(payload []byte) (*Drop, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var drop Drop
	if err := decoder.Decode(&drop); err != nil {
		return nil, fmt.Errorf("invalid YDB table removal payload")
	}
	if decoder.More() {
		return nil, fmt.Errorf("invalid YDB table removal payload")
	}
	if err := drop.validate(); err != nil {
		return nil, err
	}
	return &drop, nil
}

// DropStatement writes the statement of a removal at path, as the host's
// embedded engine writes it: `DROP TABLE `path`;`.
func DropStatement(path string) string {
	return "DROP TABLE " + QuoteIdentifier(path) + ";"
}
