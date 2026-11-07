package backend

import (
	"cmp"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
)

// renderOmissions are what the units of a complete render go on without, for
// a host that lists render.omissions (contract 25.33), as the host's embedded
// engine reported them: the part of a created column's declared type the YDB
// type does not keep, and, in the TEXT form of a creation that names no
// database, the start and the increment of a Serial's sequence (see
// tablecreate.SequenceOmissions). Each names its column as the embedded
// engine did, `<directory>.<table>.<column>`, and they are sorted by kind,
// name, property and detail, as it sorted them. operations are the request's
// native operations, nil for a common object.
func renderOmissions(request *protocol.RenderRequest, operations []*protocol.Envelope) ([]*protocol.RenderOmission, error) {
	if !slices.Contains(request.GetTargetFeatures(), provider.RenderOmissionsFeature) {
		return nil, nil
	}
	var result []*protocol.RenderOmission
	for _, operation := range operations {
		if operation == nil {
			continue
		}
		omissions, err := tablecreate.CreationOmissions(operation)
		if err != nil {
			return nil, err
		}
		owner := operation.GetOwner()
		if request.GetForm() == protocol.RenderRequest_TEXT && operation.GetKind() == tablecreate.OperationKind {
			creation, err := tablecreate.Request(operation)
			if err != nil {
				return nil, err
			}
			omissions = append(omissions, tablecreate.SequenceOmissions(owner.GetDatabase(), creation)...)
		}
		for _, omission := range omissions {
			result = append(result, &protocol.RenderOmission{Reason: "unsupported", Kind: "column",
				Name: tablecreate.TableName(owner.GetSchema(), owner.GetName()) + "." + omission.Column, Property: omission.Property, Detail: omission.Detail})
		}
	}
	slices.SortStableFunc(result, func(a, b *protocol.RenderOmission) int {
		return cmp.Or(cmp.Compare(a.GetKind(), b.GetKind()), cmp.Compare(a.GetName(), b.GetName()), cmp.Compare(a.GetProperty(), b.GetProperty()),
			cmp.Compare(a.GetDetail(), b.GetDetail()))
	})
	return result, nil
}
