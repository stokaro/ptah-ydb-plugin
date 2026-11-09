package inspect

import (
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// YDB tables and native scheme objects share one directory namespace. A service
// object occupies every such identity; omitting it must not imply native absence.
//
// A read that carries views on the common wire claims nothing about the native
// view model, so the reservation leaves that model out when c reports views.
// Its unrepresentable subject would make the host require an object comparison
// for the native view model, which has none: the host compares those views as
// common objects. The SDK takes no conflict kind for a common view, so a view
// declared under a reserved name is refused by YDB when it is created.
func (c Common) reserve(snapshot *protocol.Schema, object *protocol.Reference, purpose protocol.ReservedObject_Purpose) {
	reservation := &protocol.ReservedObject{Object: object, NormalizedObject: proto.CloneOf(object), Purpose: purpose, ConflictKinds: []string{"table"}}
	for _, model := range schema.ObservedModels() {
		if !schema.IsNativeObject(model.Kind) || (c.Views && model.Kind == schema.ViewProperties) {
			continue
		}
		reservation.ConflictKinds = append(reservation.ConflictKinds, model.Kind)
		subject := proto.CloneOf(object)
		subject.Kind = model.Kind
		snapshot.Coverage.Subjects = append(snapshot.Coverage.Subjects, &protocol.ModelSubjectCoverage{
			Kind: model.Kind, Subject: subject, NormalizedSubject: proto.CloneOf(subject),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNREPRESENTABLE, Reason: schema.ReservedNameReason},
		})
	}
	snapshot.ReservedObjects = append(snapshot.ReservedObjects, reservation)
}
