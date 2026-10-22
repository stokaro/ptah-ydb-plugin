package tablecreate

import (
	"bytes"
	"fmt"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// columnClaim is one host column step this service replaces with a native
// operation of its own.
type columnClaim struct {
	source    *protocol.CommonPlanningStep
	name      string
	add       bool
	id        *protocol.StepIdentity
	operation *protocol.NormalizedValue
}

// planColumnClaims classifies the host steps that add or drop a column of an
// altered table. A step this service can carry out natively is claimed: a
// nullable column without a default, or the removal of a column the declaration
// no longer names. Any other step of the table, such as an addition that needs
// a default or a NOT NULL constraint, is not claimed and stays with the host.
//
// A non-empty reason refuses the whole request: the host asks for a change the
// captured declaration contradicts.
func planColumnClaims(owner *protocol.Reference, table *protocol.PlanningTable, steps []*protocol.CommonPlanningStep) ([]*columnClaim, string, error) {
	declared := make(map[string]bool, len(table.Desired.GetColumns()))
	for _, column := range table.Desired.GetColumns() {
		declared[column.Name] = true
	}
	var claims []*columnClaim
	claimed := make(map[string]bool)
	for _, step := range steps {
		if step.Parent == nil || provider.CompareIdentities(step.Parent, table.Subject) != 0 {
			continue
		}
		name, action, shaped := columnFootprint(step, table.Subject)
		if !shaped {
			continue
		}
		claim := &columnClaim{source: step, name: name}
		switch {
		case step.AddedColumn != nil && action == protocol.PlanningEffect_CREATE:
			addable, err := nativeAddition(owner, step.AddedColumn)
			if err != nil {
				return nil, "", err
			}
			if !addable || step.AddedColumn.Name != name {
				continue
			}
			claim.add = true
			if !declared[name] {
				return nil, fmt.Sprintf("The host adds column %q, which the declaration does not state", name), nil
			}
		case step.AddedColumn == nil && action == protocol.PlanningEffect_DROP:
			if declared[name] {
				return nil, fmt.Sprintf("The host drops column %q, which the declaration states", name), nil
			}
		default:
			continue
		}
		if claimed[name] {
			return nil, fmt.Sprintf("The host changes column %q more than once", name), nil
		}
		claimed[name] = true
		claims = append(claims, claim)
	}
	return claims, "", nil
}

// columnFootprint returns the column a host step touches, when the step does
// nothing else: its effects are reads of the table and one effect on a column of
// that table. The caller decides from the action whether the effect is one it
// replaces. Any other footprint is unknown to this service and not shaped for a
// rewrite.
func columnFootprint(step *protocol.CommonPlanningStep, table *protocol.ComparisonIdentity) (string, protocol.PlanningEffect_Action, bool) {
	var column *protocol.PlanningEffect
	for _, effect := range step.GetMetadata().GetEffects() {
		if effect.Action == protocol.PlanningEffect_READ && provider.CompareIdentities(effect.Subject, table) == 0 {
			continue
		}
		if column != nil {
			return "", 0, false
		}
		column = effect
	}
	if column == nil {
		return "", 0, false
	}
	subject := column.Subject
	if subject.GetKind() != "column" || subject.GetName().GetSource() == "" || subject.GetName().GetSource() != subject.GetName().GetNormalized() ||
		subject.GetParent().GetNormalized() != table.GetName().GetNormalized() || subject.GetDatabase().GetNormalized() != table.GetDatabase().GetNormalized() ||
		subject.GetSchema().GetNormalized() != table.GetSchema().GetNormalized() || subject.GetCatalog().GetSource() != "" || len(subject.Path)+len(subject.Signature) != 0 {
		return "", 0, false
	}
	return subject.Name.Source, column.Action, true
}

// nativeAddition reports whether a native AlterTable can add the column as the
// host defined it. The column must be nullable and carry nothing the call
// cannot state: no default, constraint, key membership, identity, generation,
// collation, comment or reference, and no column property but its family. The
// family itself is applied by the family operation that follows the addition.
func nativeAddition(owner *protocol.Reference, column *protocol.PlanningColumn) (bool, error) {
	column = proto.CloneOf(column)
	if proto.Equal(column.Default, &protocol.PlanningDefault{}) {
		column.Default = nil
	}
	if only(column, "name", "type", "type_raw_sql", "type_is_declared_text", "nullable", "facets") != nil || !column.Nullable || !validName(column.Name) {
		return false, nil
	}
	if _, err := schema.StorageType(column.Type, true); err != nil {
		return false, nil
	}
	for _, captured := range column.Facets {
		if len(captured.Targets) > 0 && !slices.Contains(captured.Targets, "ydb") {
			continue
		}
		value, err := capturedDesired(captured)
		if err != nil {
			return false, err
		}
		if value.Owner.GetName() != column.Name || !slices.Equal(value.Owner.GetPath(), []string{owner.Name}) ||
			value.Owner.GetDatabase() != owner.Database || value.Owner.GetSchema() != owner.Schema {
			return false, fmt.Errorf("YDB added column facet belongs to a different owner")
		}
		var intent native.ColumnIntent
		if err := protojson.Unmarshal(value.Payload, &intent); err != nil {
			return false, err
		}
		if only(&intent, "family") != nil {
			return false, nil
		}
	}
	return true, nil
}

// capturedDesired re-normalizes a facet the host attached to an added column and
// requires its receipt to match, as normalizedFacet does for table facets.
func capturedDesired(captured *protocol.CapturedFacet) (*protocol.Envelope, error) {
	value, err := schema.NormalizeDesired(captured.GetValue().GetValue())
	if err != nil {
		return nil, err
	}
	expected := &protocol.NormalizedValue{Id: captured.GetValue().GetId(), Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
		CanonicalFormat: value.Kind + "/desired-json-v1", CanonicalProjection: bytes.Clone(value.Payload)}
	if !proto.Equal(expected, captured.GetValue()) {
		return nil, fmt.Errorf("YDB added column facet has a forged normalization receipt")
	}
	return value, nil
}

// captureColumns builds the native operation of one claimed step.
func captureColumns(id string, owner *protocol.Reference, claim *columnClaim) (*protocol.NormalizedValue, error) {
	request := &Ydb_Table.AlterTableRequest{}
	if claim.add {
		kind, err := schema.StorageType(claim.source.AddedColumn.Type, true)
		if err != nil {
			return nil, err
		}
		request.AddColumns = []*Ydb_Table.ColumnMeta{{Name: claim.name, Type: kind, NotNull: new(false)}}
	} else {
		request.DropColumns = []string{claim.name}
	}
	return captureRequest(id, owner, &Alteration{Request: request, HeldColumns: map[string]string{}})
}
