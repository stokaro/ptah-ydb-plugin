//go:build integration

package integration_test

import (
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// objectAccessNames are the principals one run creates: a user the grants
// name, and a group that may list the directory.
type objectAccessNames struct{ user, group string }

func newObjectAccessNames() objectAccessNames {
	suffix := strings.ToLower(rand.Text())[:8]
	return objectAccessNames{user: "ptahobju" + suffix, group: "ptahobjg" + suffix}
}

// accessObject is one scheme object a test declares: its key under the YAML
// document key that declares objects of its kind, the grant attribute that
// names it, the kind a read reports its grants under, the permission its
// grant names, and the entries YDB gives it when it creates it.
type accessObject struct {
	document, entry, attribute, kind, permission string
	defaults                                     []grantEntry
}

// objectAccessObjects are the objects a line holds, as the host's fixture
// declares them: a topic and a coordination node on every line, and a secret
// where the session establishes secrets. YDB gives a new secret an entry for
// each subject that may describe its directory, which on a default cluster is
// METADATA-READERS.
func objectAccessObjects(session *protocol.Session, directory string) []accessObject {
	objects := []accessObject{
		{document: "topics", entry: "events:\n    schema: " + directory + "\n", attribute: "on_topic", kind: "topic", permission: "ydb.granular.select_row"},
		{document: "coordination_nodes", entry: "locks:\n    schema: " + directory + "\n", attribute: "on_coordination_node", kind: "coordination node", permission: "ydb.generic.use"},
	}
	if session.Capabilities[secrets.Capability] {
		objects = append(objects, accessObject{document: "secrets", entry: "pw:\n    schema: " + directory + "\n    value_env: " + objectAccessSecretEnv + "\n",
			attribute: "on_secret", kind: "secret", permission: "ydb.granular.select_row",
			defaults: []grantEntry{{Role: "METADATA-READERS", Privilege: "ydb.granular.describe_schema", Kind: "secret", Name: directory + "/pw", Implicit: true}}})
	}
	return objects
}

// objectAccessSecretEnv is the variable the declared secret takes its value
// from.
const objectAccessSecretEnv = "PTAH_SECRET_LIVE_OBJECT_ACCESS" // #nosec G101 -- a variable name, not a credential

// name is the object's name: the key of its entry.
func (o accessObject) name() string { return strings.SplitN(o.entry, ":", 2)[0] }

// objectAccessDocument declares the principals, the group's LIST on the
// directory, and the objects of every kind the host's fixture describes, with
// a grant to the user on each: objects names the ones declared, so a kind
// with none is described empty and its objects are dropped. permission
// replaces each object's own where it is set.
func objectAccessDocument(directory string, names objectAccessNames, objects []accessObject, permission string) string {
	var text strings.Builder
	for _, document := range []string{"topics", "coordination_nodes", "secrets"} {
		fmt.Fprintf(&text, "%s:", document)
		declared := false
		for _, object := range objects {
			if object.document == document {
				fmt.Fprintf(&text, "\n  %s", object.entry)
				declared = true
			}
		}
		if !declared {
			text.WriteString(" {}\n")
		}
	}
	fmt.Fprintf(&text, "roles:\n  %s:\n    login: true\n  %s:\n    group: true\n", names.user, names.group)
	fmt.Fprintf(&text, "grants:\n  lister:\n    role: %s\n    privileges: [LIST]\n    on_schema: %s\n", names.group, directory)
	for _, object := range objects {
		fmt.Fprintf(&text, "  %s:\n    role: %s\n    privileges: [%s]\n    %s: %s.%s\n", object.name(), names.user, cmp.Or(permission, object.permission),
			object.attribute, directory, object.name())
	}
	return text.String()
}

// objectAccessExpected is what a read reports of objects once the user holds
// permission on each, beside the entries YDB gave each one, sorted as
// grantsIn sorts them.
func objectAccessExpected(directory string, names objectAccessNames, objects []accessObject, permission string) []grantEntry {
	var want []grantEntry
	for _, object := range objects {
		want = append(want, object.defaults...)
		want = append(want, grantEntry{Role: names.user, Privilege: cmp.Or(permission, object.permission), Kind: object.kind, Name: directory + "/" + object.name()})
	}
	sortGrantEntries(want)
	return want
}

// objectAccess is a locked fixture directory, a provider session on the test
// address, the run's principals and the line's objects.
type objectAccess struct {
	opened              *connection.Connection
	directory, database string
	client              *providerruntime.Client
	session             *protocol.Session
	names               objectAccessNames
	objects             []accessObject
}

// objectAccessFixture prepares an objectAccess. The cleanup drops the
// objects, which takes their entries, and then the principals.
func objectAccessFixture(c *qt.C) objectAccess {
	c.Helper()
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	names := newObjectAccessNames()
	c.Cleanup(func() { dropObjectAccess(c, opened, database, directory, names) })
	return objectAccess{opened: opened, directory: directory, database: database, client: client, session: session, names: names,
		objects: objectAccessObjects(session, directory)}
}

// grants is every entry a read of the directory reports on a topic, a
// coordination node, a secret or the kinds given that names one of the run's
// principals or the cluster's METADATA-READERS.
func (f objectAccess) grants(c *qt.C, kinds ...string) []grantEntry {
	c.Helper()
	return grantsIn(c, f.client, f.session, f.directory, append([]string{"topic", "coordination node", "secret"}, kinds...),
		[]string{f.names.user, f.names.group, "METADATA-READERS"})
}

// document is objectAccessDocument for the fixture's directory and principals.
func (f objectAccess) document(objects []accessObject, permission string) string {
	return objectAccessDocument(f.directory, f.names, objects, permission)
}

// expected is objectAccessExpected for the fixture's directory and principals.
func (f objectAccess) expected(permission string) []grantEntry {
	return objectAccessExpected(f.directory, f.names, f.objects, permission)
}

// dropObjectAccess drops what the run may have created in the directory and
// the run's principals. A missing object is no failure.
func dropObjectAccess(c *qt.C, opened *connection.Connection, database, directory string, names objectAccessNames) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	absolute := func(name string) string { return path.Join(database, directory, name) }
	_ = execFixtureSchema(ctx, opened.DB, "DROP TOPIC `"+absolute("events")+"`")
	_ = opened.Driver.Coordination().DropNode(ctx, absolute("locks"))
	_ = execFixtureSchema(ctx, opened.DB, "DROP SECRET `"+absolute("pw")+"`;")
	for _, statement := range []string{"DROP USER IF EXISTS `" + names.user + "`", "DROP GROUP IF EXISTS `" + names.group + "`"} {
		c.Check(execFixtureSchema(ctx, opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
}

// Ported from the host's TestYDBObjectAccess_RoundTrip: the host grants a user
// a permission on a topic, a coordination node and, where the line has
// secrets, a secret, through this provider; the read reports each entry as
// declared and nothing is left to plan, and a second apply plans nothing
// either. Each grant then changes its permission, which revokes one entry and
// grants another, and nothing is left to plan.
func TestHostAppliesObjectAccess_HappyPath(t *testing.T) {
	t.Setenv(objectAccessSecretEnv, "object-access-v1")
	c := testcheck.New(t)
	f := objectAccessFixture(c)
	declared, changed := f.document(f.objects, ""), f.document(f.objects, "ydb.generic.read")

	hostRun(c, f.directory, "the objects", declared, `(?s).*Schema apply completed successfully.*`)
	settled := hostPlanned(c, f.directory, "yaml", declared)
	hostRun(c, f.directory, "a second apply", declared, `(?s).*Schema is synced, no changes to be made.*`)
	granted := f.grants(c)
	changes := hostPlanned(c, f.directory, "yaml", changed)
	hostRun(c, f.directory, "the change", changed, `(?s).*Schema apply completed successfully.*`)

	c.Assert(settled, qt.HasLen, 0)
	c.Assert(granted, qt.DeepEquals, f.expected(""))
	c.Assert(changes, qt.HasLen, 2*len(f.objects), qt.Commentf("%v", changes))
	c.Assert(hostPlanned(c, f.directory, "yaml", changed), qt.HasLen, 0)
	c.Assert(f.grants(c), qt.DeepEquals, f.expected("ydb.generic.read"))
}

// Ported from the host's TestYDBObjectAccess_DropsAnObjectHoldingAGrant: a
// source that drops every object while the user holds a permission on each
// revokes none of those entries, since a REVOKE after the drop would name an
// object that is gone; the plan applies and leaves nothing to plan.
func TestHostDropsObjectsHoldingAGrant_HappyPath(t *testing.T) {
	t.Setenv(objectAccessSecretEnv, "object-access-v1")
	c := testcheck.New(t)
	f := objectAccessFixture(c)
	emptied := f.document(nil, "")
	hostRun(c, f.directory, "the objects", f.document(f.objects, ""), `(?s).*Schema apply completed successfully.*`)

	drops := hostPlanned(c, f.directory, "yaml", emptied)
	hostRun(c, f.directory, "the drops", emptied, `(?s).*Schema apply completed successfully.*`)

	c.Assert(drops, qt.HasLen, len(f.objects), qt.Commentf("%v", drops))
	c.Assert(strings.Join(drops, "\n"), qt.Not(qt.Contains), "REVOKE")
	c.Assert(hostPlanned(c, f.directory, "yaml", emptied), qt.HasLen, 0)
	c.Assert(f.grants(c), qt.HasLen, 0)
}

// Ported from the host's TestYDBObjectAccess_ExternalObjects: a user's
// permissions on an external data source and an external table over it
// stand once applied, and nothing is left to plan. A data source moved to
// another location is dropped and created again with the table over it. The
// host's engine grants both entries again after it creates the objects; this
// provider restores the entries the old objects held as it creates them, so
// the plan holds no GRANT and the entries read back the same. Nothing is left
// to plan.
func TestHostRecreatesExternalObjectsWithTheirGrants_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := objectAccessFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, f.opened.DB, "DROP EXTERNAL TABLE `"+path.Join(f.database, f.directory, "files")+"`")
		_ = execFixtureSchema(ctx, f.opened.DB, "DROP EXTERNAL DATA SOURCE `"+path.Join(f.database, f.directory, "bucket")+"`")
	})
	declaration := func(location string) string {
		return "external_data_sources:\n  bucket:\n    schema: " + f.directory + "\n    source_type: ObjectStorage\n    location: " + location + "\n    auth_method: NONE\n" +
			"external_tables:\n  files:\n    schema: " + f.directory + "\n    data_source: " + f.directory + "/bucket\n    location: 2026/\n    columns:\n" +
			"      - {name: id, type: Int64, not_null: true}\n      - {name: kind, type: Utf8}\n    options:\n      FORMAT: csv_with_names\n" +
			f.document(nil, "") +
			"  bucket:\n    role: " + f.names.user + "\n    privileges: [SELECT ROW]\n    on_external_data_source: " + f.directory + ".bucket\n" +
			"  files:\n    role: " + f.names.user + "\n    privileges: [SELECT ROW]\n    on_external_table: " + f.directory + ".files\n"
	}
	want := []grantEntry{
		{Role: f.names.user, Privilege: "ydb.granular.select_row", Kind: "external data source", Name: f.directory + "/bucket"},
		{Role: f.names.user, Privilege: "ydb.granular.select_row", Kind: "external table", Name: f.directory + "/files"},
	}
	externalGrants := func() []grantEntry {
		c.Helper()
		return grantsIn(c, f.client, f.session, f.directory, []string{"external data source", "external table"}, []string{f.names.user})
	}
	declared, moved := declaration("https://storage.example.test/events/"), declaration("https://storage.example.test/moved/")

	hostRun(c, f.directory, "the objects", declared, `(?s).*Schema apply completed successfully.*`)
	settled := hostPlanned(c, f.directory, "yaml", declared)
	created := externalGrants()
	recreation := hostPlanned(c, f.directory, "yaml", moved)
	hostRun(c, f.directory, "the move", moved, `(?s).*Schema apply completed successfully.*`)

	c.Assert(settled, qt.HasLen, 0)
	c.Assert(created, qt.DeepEquals, want)
	c.Assert(strings.Join(recreation, "\n"), qt.Not(qt.Contains), "GRANT")
	c.Assert(recreation, qt.Not(qt.HasLen), 0)
	c.Assert(hostPlanned(c, f.directory, "yaml", moved), qt.HasLen, 0)
	c.Assert(externalGrants(), qt.DeepEquals, want)
}

// sortGrantEntries sorts entries by object, subject and permission, as
// grantsIn reports them.
func sortGrantEntries(entries []grantEntry) {
	slices.SortFunc(entries, func(a, b grantEntry) int {
		return strings.Compare(a.Name+" "+a.Role+" "+a.Privilege, b.Name+" "+b.Role+" "+b.Privilege)
	})
}
