package inspect_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func TestUnavailableSecretRetainsUnknownIdentity(t *testing.T) {
	c := testcheck.New(t)
	read := snapshot()
	description := backend.Description()
	read.TargetFeatures = description.Features
	read.CommonObjects = &protocol.ObservedCommonObjects{}
	for _, kind := range description.Engines[0].CommonObjects.Kinds {
		read.CommonObjects.Coverage = append(read.CommonObjects.Coverage, &protocol.CommonObjectCoverage{Kind: kind,
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Not read by this fixture"}})
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: "app", Name: "pg.pw"}
	value, err := schema.CaptureObject(owner, &native.SecretPresence{})
	c.Assert(err, qt.IsNil)
	read.NativeObjects = []*protocol.Envelope{value}
	read.Coverage.Subjects = []*protocol.ModelSubjectCoverage{{Kind: schema.SecretProperties, Subject: owner, NormalizedSubject: proto.CloneOf(owner), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}}
	for range 2 {
		inspect.UnavailableSecrets(read)
		c.Assert(read.NativeObjects, qt.HasLen, 0)
		c.Assert(read.Coverage.Subjects, qt.HasLen, 1)
		c.Assert(proto.Equal(read.Coverage.Subjects[0].Subject, owner), qt.IsTrue)
		c.Assert(read.Coverage.Subjects[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNINSPECTED)
		c.Assert(provider.ValidateSchema(read), qt.IsNil)
		_, err := provider.InspectionValues(description, &protocol.InspectRequest{SessionId: "test-session"}, read)
		c.Assert(err, qt.IsNil)
	}
}

// A name the revision table occupies is reserved on a target without
// secrets too: its secret coverage stays unrepresentable, and the snapshot
// passes the contract's reservation check.
func TestUnavailableSecretKeepsAReservedName(t *testing.T) {
	c := testcheck.New(t)
	read := snapshot()
	description := backend.Description()
	read.TargetFeatures = description.Features
	read.CommonObjects = &protocol.ObservedCommonObjects{}
	for _, kind := range description.Engines[0].CommonObjects.Kinds {
		read.CommonObjects.Coverage = append(read.CommonObjects.Coverage, &protocol.CommonObjectCoverage{Kind: kind,
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Not read by this fixture"}})
	}
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "revisions"}
	read.ReservedObjects = []*protocol.ReservedObject{{Object: table, NormalizedObject: proto.CloneOf(table), Purpose: protocol.ReservedObject_HISTORY,
		ConflictKinds: []string{"table", schema.SecretProperties}}}
	occupied := proto.CloneOf(table)
	occupied.Kind = schema.SecretProperties
	read.Coverage.Subjects = []*protocol.ModelSubjectCoverage{{Kind: schema.SecretProperties, Subject: occupied, NormalizedSubject: proto.CloneOf(occupied),
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNREPRESENTABLE, Reason: "Name is occupied by provider-owned operational storage"}}}
	inspect.UnavailableSecrets(read)
	c.Assert(read.Coverage.Subjects, qt.HasLen, 1)
	c.Assert(read.Coverage.Subjects[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNREPRESENTABLE)
	_, err := provider.InspectionValues(description, &protocol.InspectRequest{SessionId: "test-session"}, read)
	c.Assert(err, qt.IsNil)
}
