package inspect_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// accessSecret is a snapshot that captured the secret app/pw.
func accessSecret() *protocol.Schema {
	snapshot := accessSnapshot()
	snapshot.NativeObjects = []*protocol.Envelope{{Kind: schema.SecretProperties,
		Owner: &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: "app", Name: "pw"}}}
	return snapshot
}

// secretEntries answers the database root, the directory app with the
// effective permissions given, and the secret app/pw with the entries YDB
// gave it and one GRANT made.
func secretEntries(directory *Ydb_Scheme.Entry) map[string]*Ydb_Scheme.Entry {
	return map[string]*Ydb_Scheme.Entry{
		"/local":     accessRoot("root"),
		"/local/app": directory,
		"/local/app/pw": {Name: "pw", Type: Ydb_Scheme.Entry_SECRET, Owner: "root", Permissions: []*Ydb_Scheme.Permissions{
			{Subject: "METADATA-READERS", PermissionNames: []string{"ydb.granular.describe_schema"}},
			{Subject: "team", PermissionNames: []string{"ydb.granular.describe_schema"}},
			{Subject: "team", PermissionNames: []string{"ydb.granular.select_row"}},
			{Subject: "app", PermissionNames: []string{"ydb.granular.describe_schema"}},
		}},
	}
}

// answers describes the paths entries holds, and every other one as YDB
// answers a path it does not show.
func answers(entries map[string]*Ydb_Scheme.Entry) func(context.Context, string) (*Ydb_Scheme.Entry, error) {
	return func(_ context.Context, absolute string) (*Ydb_Scheme.Entry, error) {
		entry := entries[absolute]
		if entry == nil {
			return nil, nativeapi.ErrAccessPathNotVisible
		}
		return entry, nil
	}
}

// secretGrant is how a read reports one entry of the secret app/pw.
type secretGrant struct {
	Role, Privilege string
	Implicit        bool
}

func secretGrants(objects *protocol.ObservedCommonObjects) []secretGrant {
	var grants []secretGrant
	for _, grant := range objects.Grants {
		if grant.Target.Kind == "secret" {
			grants = append(grants, secretGrant{grant.Role, grant.Privilege, grant.Implicit})
		}
	}
	return grants
}

// YDB gives a secret it creates a describe_schema entry for every subject
// that may describe its directory. The read marks those entries implicit, as
// the host's embedded engine marks them, and leaves every other entry of the
// secret, a describe_schema of a subject that may not describe the directory
// included, an ordinary grant. The grants come in the order YDB describes the
// entries, as the host's embedded engine read them.
func TestAccessMarksTheEntriesYDBGivesASecret_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	snapshot := accessSecret()
	directory := &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY, Owner: "root", EffectivePermissions: []*Ydb_Scheme.Permissions{
		{Subject: "METADATA-READERS", PermissionNames: []string{"ydb.generic.list"}},
		{Subject: "team", PermissionNames: []string{"ydb.granular.create_table", "ydb.generic.read"}},
		{Subject: "app", PermissionNames: []string{"ydb.granular.select_row"}},
	}}
	err := inspect.Access(c.Context(), snapshot, "/local", inspect.DatabaseScope, answers(secretEntries(directory)),
		func(context.Context) (nativeapi.Principals, error) { return nativeapi.Principals{}, nil })
	c.Assert(err, qt.IsNil)
	c.Assert(secretGrants(snapshot.CommonObjects), qt.DeepEquals, []secretGrant{
		{Role: "METADATA-READERS", Privilege: "ydb.granular.describe_schema", Implicit: true},
		{Role: "team", Privilege: "ydb.granular.describe_schema", Implicit: true},
		{Role: "team", Privilege: "ydb.granular.select_row"},
		{Role: "app", Privilege: "ydb.granular.describe_schema"},
	})
}

// The directory a secret is in has to answer for the subjects that may
// describe it, or the read does not tell YDB's entries from granted ones and
// fails without changing the snapshot.
func TestAccessMarksTheEntriesYDBGivesASecret_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory *Ydb_Scheme.Entry
		wantErr   string
	}{
		{name: "a directory it cannot see", wantErr: "the access path does not exist or is not visible"},
		{name: "a table at the directory's path", directory: &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_TABLE},
			wantErr: "YDB access description disagrees with the directory of a captured secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			snapshot := accessSecret()
			before := proto.CloneOf(snapshot)
			err := inspect.Access(c.Context(), snapshot, "/local", inspect.DatabaseScope, answers(secretEntries(test.directory)),
				func(context.Context) (nativeapi.Principals, error) { return nativeapi.Principals{}, nil })
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(proto.Equal(snapshot, before), qt.IsTrue)
		})
	}
}
