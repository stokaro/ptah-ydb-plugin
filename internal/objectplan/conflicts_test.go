package objectplan_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// object is the native object of kind at directory/name of the fixture
// database, as a planning request names a changed subject.
func object(kind, directory, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: kind, Database: testcomparison.Part("/local"), Schema: testcomparison.Part(directory), Name: testcomparison.Part(name)}
}

// objectChange is one changed subject of a plan, and whether the desired and
// the current objects hold it.
type objectChange struct {
	subject          *protocol.ComparisonIdentity
	desired, current bool
}

// created and dropped are the changes that create and drop subject.
func created(subject *protocol.ComparisonIdentity) objectChange {
	return objectChange{subject: subject, desired: true}
}

func dropped(subject *protocol.ComparisonIdentity) objectChange {
	return objectChange{subject: subject, current: true}
}

// conflictRequest is a planning request whose object context holds the
// changes: each changed subject, recorded on the sides that hold it.
func conflictRequest(changes ...objectChange) *protocol.PlanFeaturesRequest {
	request := &protocol.PlanFeaturesRequest{DesiredObjects: &protocol.ComparisonState{}, CurrentObjects: &protocol.ComparisonState{}}
	for _, change := range changes {
		request.ChangedSubjects = append(request.ChangedSubjects, change.subject)
		record := &protocol.ComparisonRecord{Kind: change.subject.GetKind(), Subject: change.subject}
		if change.desired {
			request.DesiredObjects.Records = append(request.DesiredObjects.Records, record)
		}
		if change.current {
			request.CurrentObjects.Records = append(request.CurrentObjects.Records, record)
		}
	}
	return request
}

// kindOnly is a sameService that puts each kind in a service of its own.
func kindOnly(a, b string) bool { return a == b }

// A plan whose two services touch paths that YDB can serve in some order is
// left alone: a creation below the path of a dropped object, which the
// creation follows, a creation beside a drop, an object replaced at its own
// path, and a pair one service plans, which that service refuses itself.
func TestDirectoryConflict_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		changes []objectChange
	}{
		{name: "a creation below a dropped object", changes: []objectChange{
			dropped(object(schema.CoordinationProperties, "app", "holder")), created(object(schema.TopicProperties, "app/holder", "events"))}},
		{name: "a creation beside a drop", changes: []objectChange{
			dropped(object(schema.TopicProperties, "app/holder", "events")), created(object(schema.CoordinationProperties, "app", "other"))}},
		{name: "an object replaced at its own path", changes: []objectChange{
			dropped(object(schema.TopicProperties, "app", "holder")), created(object(schema.CoordinationProperties, "app", "holder"))}},
		{name: "a pair one service plans", changes: []objectChange{
			dropped(object(schema.TopicProperties, "app/holder", "events")), created(object(schema.TopicProperties, "app", "holder"))}},
		{name: "an object both sides hold", changes: []objectChange{
			{subject: object(schema.TopicProperties, "app/holder", "events"), desired: true, current: true},
			created(object(schema.CoordinationProperties, "app", "holder"))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			err := objectplan.DirectoryConflict(conflictRequest(test.changes...), kindOnly)

			c.Assert(err, qt.IsNil)
		})
	}
}

// A plan that drops an object below a path and creates an object of another
// service at the path is refused in the host's embedded engine's words,
// naming the dropped object and the directory: YDB keeps the directory after
// the drop, so no order of the two can apply.
func TestDirectoryConflict_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		changes []objectChange
		want    string
	}{
		{name: "a coordination node over a dropped topic", changes: []objectChange{
			dropped(object(schema.TopicProperties, "app/holder", "events")), created(object(schema.CoordinationProperties, "app", "holder"))},
			want: "this plan drops app/holder/events and creates an object at app/holder, which is a directory in the database. "},
		{name: "a secret over a coordination node two levels down", changes: []objectChange{
			created(object(schema.SecretProperties, "", "ext")), dropped(object(schema.CoordinationProperties, "ext/locks", "lock"))},
			want: "this plan drops ext/locks/lock and creates an object at ext, which is a directory in the database. "},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			err := objectplan.DirectoryConflict(conflictRequest(test.changes...), kindOnly)

			c.Assert(err, qt.ErrorMatches, test.want+`YDB keeps a directory after its last object is dropped, and YQL has no statement that removes one: .*`)
		})
	}
}
