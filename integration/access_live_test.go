//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

func runAccess(c *qt.C, client *providerruntime.Client, session *protocol.Session, object *protocol.Reference, change *protocol.CommonObjectChange) {
	c.Helper()
	rendered, err := client.Render(c.Context(), &protocol.RenderRequest{Engine: "ydb", Form: protocol.RenderRequest_TEXT,
		TargetFeatures: session.Features, Capabilities: session.Capabilities,
		Changes: []*protocol.Change{{Id: "access", Object: object, Operation: &protocol.Change_CommonObject{CommonObject: change}}}})
	c.Assert(err, qt.IsNil)
	result, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "access", Units: rendered.Units})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
}

func TestCommonAccessSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	createFixtureTable(c, opened, database, directory)
	client := startProviderFor(c, provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.TextUnitsFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	name := "ptahaccess" + strings.ToLower(rand.Text())
	user, group := name+"u", name+"g"
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP GROUP IF EXISTS `"+group+"`"), qt.IsNil)
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP USER IF EXISTS `"+user+"`"), qt.IsNil)
	})
	userRef := &protocol.Reference{Engine: "ydb", Kind: "role", Name: user}
	groupRef := &protocol.Reference{Engine: "ydb", Kind: "group", Name: group}
	runAccess(c, client, session, userRef, &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{
		CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: user}}}})
	runAccess(c, client, session, groupRef, &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateRole{
		CreateRole: &protocol.RoleCreation{Role: &protocol.SQLRole{Name: group, Group: true}}}})
	runAccess(c, client, session, groupRef, &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AddMember{
		AddMember: &protocol.RoleMembership{Group: group, Member: user}}})
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}
	absolute := path.Join(database, directory, "records")
	held, err := nativeapi.AccessReader(opened.Driver)(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	setOwner := func(ctx context.Context, owner string) error {
		response, err := Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(opened.Driver)).ModifyPermissions(ctx,
			&Ydb_Scheme.ModifyPermissionsRequest{Path: absolute, Actions: []*Ydb_Scheme.PermissionsAction{{
				Action: &Ydb_Scheme.PermissionsAction_ChangeOwner{ChangeOwner: owner}}}})
		if err != nil {
			return err
		}
		return nativeapi.Status(response.GetOperation())
	}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(setOwner(ctx, held.GetOwner()), qt.IsNil)
	})
	c.Assert(setOwner(c.Context(), user), qt.IsNil)
	grant := &protocol.SQLGrant{Role: group, Privileges: []string{"SELECT"}, Target: table}
	runAccess(c, client, session, table, &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Grant{Grant: grant}})
	read := func() *protocol.ObservedCommonObjects {
		snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
		c.Assert(err, qt.IsNil)
		for _, coverage := range snapshot.GetCommonObjects().GetCoverage() {
			c.Assert(coverage.GetKnowledge().GetState(), qt.Equals, protocol.ModelKnowledge_COMPLETE)
		}
		return snapshot.GetCommonObjects()
	}
	objects := read()
	index := slices.IndexFunc(objects.Roles, func(role *protocol.ObservedRole) bool { return role.GetName() == user })
	c.Assert(index >= 0, qt.IsTrue)
	c.Assert(objects.Roles[index].Login, qt.IsFalse)
	c.Assert(objects.Roles[index].Password, qt.Equals, protocol.ObservedRole_PASSWORD_UNKNOWN)
	c.Assert(slices.ContainsFunc(objects.Memberships, func(m *protocol.RoleMembership) bool { return m.Group == group && m.Member == user }), qt.IsTrue)
	granted := func(g *protocol.ObservedGrant) bool {
		return g.Role == group && g.Target.Schema == directory && g.Target.Name == "records" && g.Privilege == "ydb.generic.read"
	}
	c.Assert(slices.ContainsFunc(objects.Grants, granted), qt.IsTrue)
	owner := slices.IndexFunc(objects.ObjectOwners, func(o *protocol.ObjectOwner) bool {
		return o.Object.Kind == "table" && o.Object.Schema == directory && o.Object.Name == "records"
	})
	c.Assert(owner >= 0, qt.IsTrue)
	c.Assert(objects.ObjectOwners[owner].Owner, qt.Equals, user)
	c.Assert(objects.ObjectOwners[owner].OwnerCanLogin, qt.IsNotNil)
	c.Assert(*objects.ObjectOwners[owner].OwnerCanLogin, qt.IsFalse)
	runAccess(c, client, session, table, &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Revoke{Revoke: grant}})
	runAccess(c, client, session, groupRef, &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_RemoveMember{RemoveMember: &protocol.RoleMembership{Group: group, Member: user}}})
	objects = read()
	c.Assert(slices.ContainsFunc(objects.Grants, granted), qt.IsFalse)
	c.Assert(slices.ContainsFunc(objects.Memberships, func(m *protocol.RoleMembership) bool { return m.Group == group && m.Member == user }), qt.IsFalse)
}
