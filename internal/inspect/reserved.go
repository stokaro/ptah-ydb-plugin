package inspect

import (
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// YDB tables and native scheme objects share one directory namespace. A service
// object occupies every such identity; omitting it must not imply native absence.
func reserve(snapshot *protocol.Schema, object *protocol.Reference, purpose protocol.ReservedObject_Purpose) {
	reservation := &protocol.ReservedObject{Object: object, NormalizedObject: proto.CloneOf(object), Purpose: purpose, ConflictKinds: []string{"table"}}
	for _, model := range schema.ObservedModels() {
		if !schema.IsNativeObject(model.Kind) {
			continue
		}
		reservation.ConflictKinds = append(reservation.ConflictKinds, model.Kind)
		subject := proto.CloneOf(object)
		subject.Kind = model.Kind
		snapshot.Coverage.Subjects = append(snapshot.Coverage.Subjects, &protocol.ModelSubjectCoverage{
			Kind: model.Kind, Subject: subject, NormalizedSubject: proto.CloneOf(subject),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNREPRESENTABLE, Reason: "Name is occupied by provider-owned operational storage"},
		})
	}
	snapshot.ReservedObjects = append(snapshot.ReservedObjects, reservation)
}
