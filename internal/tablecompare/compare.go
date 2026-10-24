// Package tablecompare compares the table and column facets of captured YDB row
// tables offline: the TTL, the automatic partitioning settings, the column
// families, the family each column sits in and the set of secondary indexes.
// The changefeeds of a table are accepted and never reported as a difference.
package tablecompare

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the owner of the row-table facet comparison.
const ServiceID = "ydb-row-table-properties"

// Kinds lists the facet models this comparison owns.
func Kinds() []string {
	return []string{schema.TableProperties, schema.ColumnProperties, schema.IndexProperties, schema.ChangefeedProperties}
}

// OwnerKinds lists the subject kinds that carry those facets.
func OwnerKinds() []string { return []string{"table", "column"} }

// Compare reports every table or column whose declared facet differs from the
// one the existing table holds. A facet on only one side states nothing to
// compare: the alteration planner decides what an absent read means. The
// exception is the table properties of a table that exists on both sides: when
// the desired coverage establishes them, a table with no declared facet is
// declared to have no TTL, and a TTL it holds is reported. A table's
// changefeeds are read and validated, and never differ: the planner does not
// alter them. Only
// differences the alteration planner acts on are reported, and each carries
// both complete operands. The backend validates the request and the result
// around this call.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.GetComparisonId() != ServiceID {
		return nil, fmt.Errorf("unknown row-table facet comparison")
	}
	for _, part := range request.GetIdentifiers().GetResolvedNames() {
		if part.Name != part.Key {
			return nil, fmt.Errorf("YDB comparison requires exact identifiers")
		}
	}
	if request.GetIdentifiers().GetDefaultSchema() != "" || request.GetIdentifiers().GetCatalogCollation() != "" {
		return nil, fmt.Errorf("YDB comparison has unsupported identifier defaults")
	}
	held := make(map[string]*protocol.ComparisonRecord, len(request.GetCurrent().GetRecords()))
	for _, record := range request.GetCurrent().GetRecords() {
		held[recordKey(record.GetKind(), record.GetSubject())] = record
	}
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
	// declared holds every table that has a desired table-properties record,
	// valued or excluded.
	declared := make(map[string]bool)
	for _, record := range request.GetDesired().GetRecords() {
		if record.GetKind() == schema.TableProperties {
			declared[recordKey(record.GetKind(), record.GetSubject())] = true
		}
	}
	result := &protocol.ComparedModels{Complete: true, Desired: proto.CloneOf(request.GetDesired())}
	for _, want := range wanted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		observed := held[recordKey(want.GetKind(), want.GetSubject())]
		if !compared(request, want) || observed == nil || !compared(request, observed) || !both[recordKey("", want.GetSubject())] {
			continue
		}
		if err := compareRecord(result, want, observed); err != nil {
			return nil, err
		}
	}
	for _, parent := range request.GetParents() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if parent.GetSubject().GetKind() != "table" || !parent.GetDesired() || !parent.GetCurrent() || declared[recordKey(schema.TableProperties, parent.GetSubject())] {
			continue
		}
		observed := held[recordKey(schema.TableProperties, parent.GetSubject())]
		if observed == nil || !compared(request, observed) {
			continue
		}
		if err := compareUnstated(request, result, parent.GetSubject(), observed); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// compareUnstated compares a table the desired side carries no table properties
// for with the properties the table holds. A source that describes the table
// properties of every table it declares (complete desired knowledge) states the
// TTL of a table by leaving it out: the table is declared to have none. A side
// that could not describe them states nothing, and a record the desired side
// carries without a value is an exclusion, so neither is compared.
func compareUnstated(request *protocol.CompareModelsRequest, result *protocol.ComparedModels, subject *protocol.ComparisonIdentity, observed *protocol.ComparisonRecord) error {
	if !tablecreate.FacetKnown(request.GetDesired().GetCoverage(), schema.TableProperties, subject, true) {
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

// compared reports whether this comparison reads a record: it owns the model,
// the record states a value, and the value is not bound to other targets.
func compared(request *protocol.CompareModelsRequest, record *protocol.ComparisonRecord) bool {
	return slices.Contains(request.GetKinds(), record.GetKind()) && record.GetValue() != nil &&
		(len(record.GetTargets()) == 0 || slices.ContainsFunc(record.GetTargets(), func(target string) bool {
			target = strings.ToLower(strings.TrimSpace(target))
			return target == "ydb" || target == "ydbs"
		}))
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
