// Package tablecompare compares the table and column facets of captured YDB
// tables offline: the TTL, the automatic partitioning settings, the column
// families, the family each column sits in, the set of secondary indexes and
// the changefeeds of a row table, and the storage of a column table.
package tablecompare

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ServiceID names the owner of the row-table facet comparison.
const ServiceID = "ydb-row-table-properties"

// Kinds lists the facet models this comparison owns.
func Kinds() []string {
	return []string{schema.TableProperties, schema.ColumnProperties, schema.IndexProperties, schema.IndexSettingsProperties, schema.ChangefeedProperties, schema.ColumnStorageProperties}
}

// ChangeKinds lists the changes this comparison reports.
func ChangeKinds() []string {
	return []string{schema.TablePropertiesChange, schema.ColumnStorageChange}
}

// OwnerKinds lists the subject kinds that carry those facets.
func OwnerKinds() []string { return []string{"table", "column", "index"} }

// Compare reports every table or column whose declared facet differs from the
// one the existing table holds. A facet on only one side states nothing to
// compare: the alteration planner decides what an absent read means. The
// exception is the table properties of a table that exists on both sides: when
// the desired coverage establishes them, a table with no declared facet is
// declared to have no TTL, and a TTL it holds is reported; likewise a source
// that describes the changefeeds of every table it declares declares a table
// with none to have none. A changefeed set the comparison cannot decide, one
// holding a changefeed the read cannot represent or a replication-managed
// stream the declaration changes, is reported as undecided. Only differences
// the alteration planner acts on are reported, and each carries both complete
// operands. The backend validates the request and the result around this
// call.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	held := make(map[string]*protocol.ComparisonRecord, len(request.GetCurrent().GetRecords()))
	for _, record := range request.GetCurrent().GetRecords() {
		held[recordKey(record.GetKind(), record.GetSubject())] = record
	}
	result := &protocol.ComparedModels{Complete: true, Desired: proto.CloneOf(request.GetDesired())}
	if err := compareDeclared(ctx, request, result, held); err != nil {
		return nil, err
	}
	if err := compareUnstatedTables(ctx, request, result, held); err != nil {
		return nil, err
	}
	undescribedProperties(request, result)
	if err := compareUnstatedParts(ctx, request, result, held); err != nil {
		return nil, err
	}
	if err := compareStorages(ctx, request, result, held); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// validateRequest refuses a request addressed to another comparison, one that
// resolves a name to another spelling, and one with identifier defaults.
func validateRequest(request *protocol.CompareModelsRequest) error {
	if request.GetComparisonId() != ServiceID {
		return fmt.Errorf("unknown row-table facet comparison")
	}
	for _, part := range request.GetIdentifiers().GetResolvedNames() {
		if part.Name != part.Key {
			return fmt.Errorf("YDB comparison requires exact identifiers")
		}
	}
	if request.GetIdentifiers().GetDefaultSchema() != "" || request.GetIdentifiers().GetCatalogCollation() != "" {
		return fmt.Errorf("YDB comparison has unsupported identifier defaults")
	}
	for _, side := range []*protocol.ComparisonState{request.GetDesired(), request.GetCurrent()} {
		if kind := presentButAbsent(side); kind != "" {
			return fmt.Errorf("a present %s facet has absent coverage", kind)
		}
	}
	return nil
}

// presentButAbsent is the kind of a facet side carries a value for while its
// coverage claims the facet absent for the same subject, or "": the side
// contradicts itself, and the comparison answers it with no partial result,
// as the host's embedded engine refused it.
func presentButAbsent(side *protocol.ComparisonState) string {
	for _, record := range side.GetRecords() {
		if record.GetValue() == nil {
			continue
		}
		for _, claim := range side.GetCoverage().GetSubjects() {
			if claim.GetKind() == record.GetKind() && claim.GetKnowledge().GetState() == protocol.ModelKnowledge_ABSENT &&
				provider.CompareIdentities(claim.GetSubject(), record.GetSubject()) == 0 {
				return record.GetKind()
			}
		}
	}
	return ""
}

// compareDeclared compares each desired record, in kind and subject order,
// with the record of the same kind and subject the current side holds, when
// both records are compared and both sides hold the record's parent. Column
// storage is left to compareStorages.
func compareDeclared(ctx context.Context, request *protocol.CompareModelsRequest, result *protocol.ComparedModels, held map[string]*protocol.ComparisonRecord) error {
	both := make(map[string]bool, len(request.GetParents()))
	for _, parent := range request.GetParents() {
		both[recordKey("", parent.GetSubject())] = parent.GetDesired() && parent.GetCurrent()
	}
	wanted := slices.Clone(request.GetDesired().GetRecords())
	slices.SortFunc(wanted, func(a, b *protocol.ComparisonRecord) int {
		if compared := strings.Compare(a.GetKind(), b.GetKind()); compared != 0 {
			return compared
		}
		return provider.CompareIdentities(a.GetSubject(), b.GetSubject())
	})
	for _, want := range wanted {
		if err := ctx.Err(); err != nil {
			return err
		}
		if want.GetKind() == schema.ColumnStorageProperties {
			// Compared per table by compareStorages, where a missing side means
			// row storage.
			continue
		}
		observed := held[recordKey(want.GetKind(), want.GetSubject())]
		if !compared(request, want) || observed == nil || !compared(request, observed) || !both[recordKey("", want.GetSubject())] {
			continue
		}
		// A concrete platform block can coexist with a captured loss for the
		// same table. Its value is partial, so the limit below must decide the
		// whole model before a change can be returned to the host.
		if want.GetKind() == schema.TableProperties {
			if _, limited := unrepresentableClaim(request.GetDesired(), want.GetSubject()); limited {
				continue
			}
			if _, limited := unrepresentableClaim(request.GetCurrent(), want.GetSubject()); limited {
				continue
			}
		}
		if err := compareRecord(result, want, observed); err != nil {
			return err
		}
	}
	return nil
}

// compareUnstatedTables compares the table properties a table holds when both
// sides hold the table and the desired side has no table-properties record
// for it, valued or excluded.
func compareUnstatedTables(ctx context.Context, request *protocol.CompareModelsRequest, result *protocol.ComparedModels, held map[string]*protocol.ComparisonRecord) error {
	// declared holds every table that has a desired table-properties record,
	// valued or excluded.
	declared := make(map[string]bool)
	for _, record := range request.GetDesired().GetRecords() {
		if record.GetKind() == schema.TableProperties {
			declared[recordKey(record.GetKind(), record.GetSubject())] = true
		}
	}
	for _, parent := range request.GetParents() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parent.GetSubject().GetKind() != "table" || !parent.GetDesired() || !parent.GetCurrent() || declared[recordKey(schema.TableProperties, parent.GetSubject())] {
			continue
		}
		observed := held[recordKey(schema.TableProperties, parent.GetSubject())]
		if observed == nil || !compared(request, observed) {
			continue
		}
		if err := compareUnstated(request, result, parent.GetSubject(), observed); err != nil {
			return err
		}
	}
	return nil
}

// undescribedProperties reports undecided the table properties of a table
// a side claims unrepresentable. A source that met a TTL, a column family or
// a partitioning setting it could not describe would have the table keep
// what it holds, or be created without them, and drop what the author wrote
// without a word; a read that found one it could not describe, such as a
// family field a newer server writes, would have the declaration change what
// nobody read. So the plan stops there, as the host's embedded engine stopped
// it: for the declaration of any table the plan keeps or creates, and for the
// read of a table both sides hold whose properties the declaration states.
func undescribedProperties(request *protocol.CompareModelsRequest, result *protocol.ComparedModels) {
	for _, parent := range request.GetParents() {
		if parent.GetSubject().GetKind() != "table" || !parent.GetDesired() {
			continue
		}
		reason, found := unrepresentableClaim(request.GetDesired(), parent.GetSubject())
		if found {
			result.Undecided = append(result.Undecided, &protocol.ComparisonUndecided{Kind: schema.TableProperties, Subject: proto.CloneOf(parent.GetSubject()),
				Reason: "The desired source could not describe the table's TTL, column families or partitioning: " + reason})
			continue
		}
		stated := tablecreate.FacetKnown(request.GetDesired().GetCoverage(), schema.TableProperties, parent.GetSubject(), tablecreate.DesiredSide) ||
			slices.ContainsFunc(request.GetDesired().GetRecords(), func(record *protocol.ComparisonRecord) bool {
				return record.GetKind() == schema.TableProperties && provider.CompareIdentities(record.GetSubject(), parent.GetSubject()) == 0
			})
		if reason, found = unrepresentableClaim(request.GetCurrent(), parent.GetSubject()); found && parent.GetCurrent() && stated {
			result.Undecided = append(result.Undecided, &protocol.ComparisonUndecided{Kind: schema.TableProperties, Subject: proto.CloneOf(parent.GetSubject()),
				Reason: "The read found a TTL, a column family or a partitioning setting of the table it could not describe: " + reason})
		}
	}
}

// unrepresentableClaim is the reason side gives for claiming the table
// properties of subject unrepresentable, and whether it claims so.
func unrepresentableClaim(side *protocol.ComparisonState, subject *protocol.ComparisonIdentity) (string, bool) {
	for _, claim := range side.GetCoverage().GetSubjects() {
		if claim.GetKind() == schema.TableProperties && claim.GetKnowledge().GetState() == protocol.ModelKnowledge_UNREPRESENTABLE &&
			provider.CompareIdentities(claim.GetSubject(), subject) == 0 && forYDB(claim.GetTargets()) {
			return cmp.Or(claim.GetKnowledge().GetReason(), "it gives no reason"), true
		}
	}
	return "", false
}

// compareUnstatedParts compares the changefeeds of every table both sides
// hold, then the family of every column both sides hold, where the desired
// side may state them by leaving them out.
func compareUnstatedParts(ctx context.Context, request *protocol.CompareModelsRequest, result *protocol.ComparedModels, held map[string]*protocol.ComparisonRecord) error {
	for _, parent := range request.GetParents() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parent.GetSubject().GetKind() != "table" || !parent.GetDesired() || !parent.GetCurrent() {
			continue
		}
		if err := compareUnstatedChangefeeds(request, result, parent.GetSubject(), held); err != nil {
			return err
		}
		if err := compareUnstatedIndexes(request, result, parent.GetSubject(), held); err != nil {
			return err
		}
	}
	for _, parent := range request.GetParents() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parent.GetSubject().GetKind() != "column" || !parent.GetDesired() || !parent.GetCurrent() {
			continue
		}
		if err := compareUnplacedColumn(request, result, parent.GetSubject(), held); err != nil {
			return err
		}
	}
	return nil
}

// compareStorages compares the storage of every table both sides hold, when
// the request compares column storage.
func compareStorages(ctx context.Context, request *protocol.CompareModelsRequest, result *protocol.ComparedModels, held map[string]*protocol.ComparisonRecord) error {
	// desiredStorage holds the declared column storage of each table,
	// rowStated every table the declaration states row-table settings for, and
	// statedTTL the TTL alone a source that cannot say a table's storage
	// states for one.
	desiredStorage := make(map[string]*protocol.ComparisonRecord)
	rowStated := make(map[string]bool)
	statedTTL := make(map[string]*protocol.Envelope)
	for _, record := range request.GetDesired().GetRecords() {
		if !compared(request, record) {
			continue
		}
		switch record.GetKind() {
		case schema.ColumnStorageProperties:
			desiredStorage[recordKey(record.GetKind(), record.GetSubject())] = record
		case schema.TableProperties:
			storage, err := ttlAloneStorage(request, record)
			if err != nil {
				return err
			}
			if storage != nil {
				statedTTL[recordKey(schema.ColumnStorageProperties, record.GetSubject())] = storage
				continue
			}
			rowStated[recordKey(record.GetKind(), record.GetSubject())] = true
		}
	}
	for _, parent := range request.GetParents() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parent.GetSubject().GetKind() != "table" || !parent.GetDesired() || !parent.GetCurrent() || !slices.Contains(request.GetKinds(), schema.ColumnStorageProperties) {
			continue
		}
		if err := compareStorage(request, result, parent.GetSubject(), storageSides{desired: desiredStorage, held: held, rowStated: rowStated, ttl: statedTTL}); err != nil {
			return err
		}
	}
	return nil
}

// storageSides are what compareStorage reads of both sides: the declared
// column storage and the held facets of each table, every table the
// declaration states row-table settings for, and the column storage the TTL
// alone a source states asks for.
type storageSides struct {
	desired, held map[string]*protocol.ComparisonRecord
	rowStated     map[string]bool
	ttl           map[string]*protocol.Envelope
}

// compareStorage compares the storage of a table both sides hold. A column
// table whose hash key, shard count, TTL or stated local indexes differ from
// the declaration is a change, and so is a table that is a row table on one
// side and a column table on the other: a declared column table over a table
// read as a row table, or a column table where the declaration asks for a row
// table, which it does when the source describes the column storage of every
// table it declares and states none for this one, or states row-table
// settings for it. A source that cannot say a table's storage, such as an HCL
// platform group, and states a TTL alone, states the TTL of a column table it
// holds: the table keeps its storage and takes that TTL, a change of the
// column storage the row-table properties the source states answer, since the
// source states no column storage the host could hold a change of. A side the
// comparison cannot establish states nothing.
func compareStorage(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, subject *protocol.ComparisonIdentity, sides storageSides) error {
	wanted := sides.desired[recordKey(schema.ColumnStorageProperties, subject)]
	holding := sides.held[recordKey(schema.ColumnStorageProperties, subject)]
	if holding != nil && !compared(request, holding) {
		holding = nil
	}
	var before, after *protocol.Envelope
	var err error
	if wanted != nil {
		if after, err = tablecreate.NormalizedFacet(wanted, protocol.ValueInput_DESIRED); err != nil {
			return err
		}
	}
	// compared is the facet the change answers: the declared column storage,
	// or the row-table properties whose TTL alone a column table takes.
	compared := schema.ColumnStorageProperties
	if stated := sides.ttl[recordKey(schema.ColumnStorageProperties, subject)]; wanted == nil && holding != nil && stated != nil {
		after, compared = stated, schema.TableProperties
	}
	if holding != nil {
		if before, err = tablecreate.NormalizedFacet(holding, protocol.ValueInput_OBSERVED); err != nil {
			return err
		}
	}
	changed, err := storageChanged(request, subject, sides, before, after)
	if err != nil || !changed {
		return err
	}
	owner := after.GetOwner()
	if owner == nil {
		owner = before.GetOwner()
	}
	change, err := schema.CaptureColumnStorageChange("change-"+strconv.Itoa(len(result.Changes)), owner, before, after)
	if err != nil {
		return err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: compared, Subject: proto.CloneOf(subject), Value: change})
	return nil
}

// storageChanged reports whether the column storage of a table changes: both
// sides hold a storage and they differ, a declared storage meets a read row
// table, or a held storage meets a declaration of row-table settings or a
// source that knows the table's column storage.
func storageChanged(request *protocol.CompareModelsRequest, subject *protocol.ComparisonIdentity, sides storageSides, before, after *protocol.Envelope) (bool, error) {
	row := sides.held[recordKey(schema.TableProperties, subject)]
	switch {
	case after != nil && before != nil:
		return tablecreate.StorageFacetDiffers(after, before)
	case after != nil && row != nil && compared(request, row):
		return true, nil
	case before != nil && (sides.rowStated[recordKey(schema.TableProperties, subject)] ||
		tablecreate.FacetKnown(request.GetDesired().GetCoverage(), schema.ColumnStorageProperties, subject, tablecreate.DesiredSide)):
		return true, nil
	}
	return false, nil
}

// ttlAloneStorage is the column storage a desired row-table record that
// states a TTL alone asks of a column table, and nil for any other record;
// see tablecreate.TTLAloneStorage.
func ttlAloneStorage(request *protocol.CompareModelsRequest, record *protocol.ComparisonRecord) (*protocol.Envelope, error) {
	storage, err := tablecreate.TTLAloneStorage(request.GetDesired().GetCoverage(), record)
	return storage.GetValue(), err
}

// compareUnstated compares a table the desired side carries no table properties
// for with the properties the table holds. A source that describes the table
// properties of every table it declares (complete desired knowledge) states the
// TTL of a table by leaving it out: the table is declared to have none. A side
// that could not describe them states nothing, and a record the desired side
// carries without a value is an exclusion, so neither is compared.
func compareUnstated(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, subject *protocol.ComparisonIdentity, observed *protocol.ComparisonRecord) error {
	if !tablecreate.FacetKnown(request.GetDesired().GetCoverage(), schema.TableProperties, subject, tablecreate.DesiredSide) {
		return nil
	}
	before, err := tablecreate.NormalizedFacet(observed, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	after, err := tablecreate.UnstatedTableFacet(before.Owner)
	if err != nil {
		return err
	}
	differs, err := tablecreate.FacetDiffers(after, before)
	if err != nil || !differs {
		return err
	}
	change, err := schema.CaptureTablePropertiesChange("change-"+strconv.Itoa(len(result.Changes)), after.Owner, before, after)
	if err != nil {
		return err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: schema.TableProperties, Subject: proto.CloneOf(subject), Value: change})
	return nil
}

// compareUnplacedColumn compares a column both sides hold, for which the
// desired side carries no column properties, with the family the column sits
// in. A source that describes where every column sits places such a column in
// the default family (see tablecreate.PlacedInDefault), so a column the table
// holds in another family is reported to move back. A source that could not
// describe the column properties states nothing, and neither does the
// declaration of a column table, whose storage the column table comparison
// owns.
func compareUnplacedColumn(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, subject *protocol.ComparisonIdentity,
	held map[string]*protocol.ComparisonRecord) error {
	for _, record := range request.GetDesired().GetRecords() {
		if record.GetKind() == schema.ColumnProperties && provider.CompareIdentities(record.GetSubject(), subject) == 0 {
			return nil
		}
	}
	observed := held[recordKey(schema.ColumnProperties, subject)]
	if observed == nil || !compared(request, observed) || !tablecreate.PlacedInDefault(request.GetDesired().GetCoverage(), subject) {
		return nil
	}
	table := proto.CloneOf(subject)
	table.Kind, table.Name, table.Parent = "table", proto.CloneOf(subject.GetParent()), nil
	if held[recordKey(schema.ColumnStorageProperties, table)] != nil || slices.ContainsFunc(request.GetDesired().GetRecords(), func(record *protocol.ComparisonRecord) bool {
		return record.GetKind() == schema.ColumnStorageProperties && provider.CompareIdentities(record.GetSubject(), table) == 0
	}) {
		return nil
	}
	before, err := tablecreate.NormalizedFacet(observed, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	after, err := tablecreate.UnstatedColumnFacet(before.Owner)
	if err != nil {
		return err
	}
	differs, err := tablecreate.FacetDiffers(after, before)
	if err != nil || !differs {
		return err
	}
	change, err := schema.CaptureTablePropertiesChange("change-"+strconv.Itoa(len(result.Changes)), after.Owner, before, after)
	if err != nil {
		return err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: schema.ColumnProperties, Subject: proto.CloneOf(subject), Value: change})
	return nil
}

// compareUnstatedChangefeeds compares a table both sides hold, for which the
// desired side carries no changefeed set, with the changefeeds the table holds.
// A source that describes the changefeeds of every table it declares (complete
// desired knowledge) declares such a table to have none; a source that could
// not describe them states nothing.
func compareUnstatedChangefeeds(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, subject *protocol.ComparisonIdentity,
	held map[string]*protocol.ComparisonRecord) error {
	if !slices.Contains(request.GetKinds(), schema.ChangefeedProperties) {
		return nil
	}
	for _, record := range request.GetDesired().GetRecords() {
		if record.GetKind() == schema.ChangefeedProperties && provider.CompareIdentities(record.GetSubject(), subject) == 0 {
			return nil
		}
	}
	observed := held[recordKey(schema.ChangefeedProperties, subject)]
	if observed == nil || !compared(request, observed) || !tablecreate.FacetKnown(request.GetDesired().GetCoverage(), schema.ChangefeedProperties, subject, tablecreate.DesiredSide) {
		return nil
	}
	before, err := tablecreate.NormalizedFacet(observed, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	after, err := tablecreate.UnstatedChangefeedFacet(before.Owner)
	if err != nil {
		return err
	}
	return compareChangefeeds(result, schema.ChangefeedProperties, subject, after, before)
}

// compareUnstatedIndexes compares the native index set of a table both sides
// hold, for which the desired side carries no native index set, with the
// vector and full-text indexes the table holds. A source of a host that
// negotiates source.index-kinds declares those indexes on the table they are
// on, so one that describes the native index sets of every table it declares
// declares a table that writes none to have none, and each one the table
// holds is reported to go, as the host's embedded engine plans it. Without the
// feature a source cannot declare one, and the table keeps what it holds.
func compareUnstatedIndexes(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, subject *protocol.ComparisonIdentity,
	held map[string]*protocol.ComparisonRecord) error {
	if !slices.Contains(request.GetKinds(), schema.IndexProperties) || !tablecreate.SourcesStateNativeIndexes(request.GetTargetFeatures()) {
		return nil
	}
	for _, record := range request.GetDesired().GetRecords() {
		if record.GetKind() == schema.IndexProperties && provider.CompareIdentities(record.GetSubject(), subject) == 0 {
			return nil
		}
	}
	observed := held[recordKey(schema.IndexProperties, subject)]
	if observed == nil || !compared(request, observed) || !tablecreate.FacetKnown(request.GetDesired().GetCoverage(), schema.IndexProperties, subject, tablecreate.DesiredSide) {
		return nil
	}
	before, err := tablecreate.NormalizedFacet(observed, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	after, err := tablecreate.UnstatedIndexFacet(before.Owner)
	if err != nil {
		return err
	}
	differs, err := tablecreate.FacetDiffers(after, before)
	if err != nil || !differs {
		return err
	}
	change, err := schema.CaptureTablePropertiesChange("change-"+strconv.Itoa(len(result.Changes)), after.Owner, before, after)
	if err != nil {
		return err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: schema.IndexProperties, Subject: proto.CloneOf(subject), Value: change})
	return nil
}

// compareChangefeeds reports how a table's changefeeds differ from the
// declared ones: a change, an undecided set, or nothing.
func compareChangefeeds(result *protocol.ComparedModels, kind string, subject *protocol.ComparisonIdentity, after, before *protocol.Envelope) error {
	differs, undecided, err := tablecreate.ChangefeedDifference(after, before)
	if err != nil {
		return err
	}
	if undecided != "" {
		result.Undecided = append(result.Undecided, &protocol.ComparisonUndecided{Kind: kind, Subject: proto.CloneOf(subject), Reason: undecided})
		return nil
	}
	if !differs {
		return nil
	}
	change, err := schema.CaptureTablePropertiesChange("change-"+strconv.Itoa(len(result.Changes)), after.Owner, before, after)
	if err != nil {
		return err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: kind, Subject: proto.CloneOf(subject), Value: change})
	return nil
}

// compared reports whether this comparison reads a record: it owns the model,
// the record states a value, and the value is not bound to other targets.
func compared(request *protocol.CompareModelsRequest, record *protocol.ComparisonRecord) bool {
	return slices.Contains(request.GetKinds(), record.GetKind()) && record.GetValue() != nil && forYDB(record.GetTargets())
}

// forYDB reports whether a record or a claim scoped to targets applies to
// YDB. One with no targets applies to every engine.
func forYDB(targets []string) bool {
	return len(targets) == 0 || slices.ContainsFunc(targets, func(target string) bool {
		target = strings.ToLower(strings.TrimSpace(target))
		return target == "ydb" || target == "ydbs"
	})
}

func compareRecord(result *protocol.ComparedModels, want, observed *protocol.ComparisonRecord) error {
	after, err := tablecreate.NormalizedFacet(want, protocol.ValueInput_DESIRED)
	if err != nil {
		return err
	}
	before, err := tablecreate.NormalizedFacet(observed, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	if !proto.Equal(before.Owner, after.Owner) {
		result.Undecided = append(result.Undecided, &protocol.ComparisonUndecided{Kind: want.Kind, Subject: proto.CloneOf(want.Subject),
			Reason: "The declared and the observed facet name different owners"})
		return nil
	}
	if want.Kind == schema.ChangefeedProperties {
		return compareChangefeeds(result, want.Kind, want.Subject, after, before)
	}
	differs, err := tablecreate.FacetDiffers(after, before)
	if err != nil || !differs {
		return err
	}
	change, err := schema.CaptureTablePropertiesChange("change-"+strconv.Itoa(len(result.Changes)), after.Owner, before, after)
	if err != nil {
		return err
	}
	result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: want.Kind, Subject: proto.CloneOf(want.Subject), Value: change})
	return nil
}

// recordKey identifies one facet of one subject by the normalized spelling the
// comparison orders identities by.
func recordKey(kind string, subject *protocol.ComparisonIdentity) string {
	var key strings.Builder
	key.WriteString(strconv.Quote(kind))
	write := func(part *protocol.ComparisonPart) { key.WriteString(strconv.Quote(part.GetNormalized())) }
	key.WriteString(strconv.Quote(subject.GetKind()))
	for _, part := range []*protocol.ComparisonPart{subject.GetDatabase(), subject.GetCatalog(), subject.GetSchema(), subject.GetParent(), subject.GetName()} {
		write(part)
	}
	for _, parts := range [][]*protocol.ComparisonPart{subject.GetPath(), subject.GetSignature()} {
		key.WriteString(strconv.Itoa(len(parts)))
		for _, part := range parts {
			write(part)
		}
	}
	return key.String()
}
