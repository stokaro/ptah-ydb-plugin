package inspect_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func TestUnavailableSecretRetainsUnknownIdentity(t *testing.T) {
	c := qt.New(t)
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
