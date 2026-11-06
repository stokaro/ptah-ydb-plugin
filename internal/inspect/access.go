package inspect

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

const poolAccessDirectory = ".metadata/workload_manager/pools"

// Scope is what a read is confined to.
type Scope int

const (
	// DatabaseScope is a read of a whole database.
	DatabaseScope Scope = iota
	// RealmScope is a read of a dev realm inside a database.
	RealmScope
)

// Access reads the permissions and owners of the objects a snapshot captured,
// its listed directories, and its root. Principals belong to the whole database;
// a read of RealmScope records their existence without declaring that it owns
// them. A read failure leaves the snapshot unchanged. A specific
// principal-visibility refusal retains object access and records unknown role
// and membership coverage.
func Access(ctx context.Context, snapshot *protocol.Schema, database string, scope Scope,
	describe func(context.Context, string) (*Ydb_Scheme.Entry, error),
	principals func(context.Context) (nativeapi.Principals, error),
) error {
	objects := proto.CloneOf(snapshot.GetCommonObjects())
	if objects == nil {
		return fmt.Errorf("YDB access inspection requires common objects")
	}
	targets, err := accessTargets(snapshot, database)
	if err != nil {
		return err
	}
	describers := make(map[string]map[string]bool)
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := targetAccess(ctx, objects, target, describe, describers); err != nil {
			return err
		}
	}
	for _, kind := range []protocol.CommonObjectSupport_Kind{protocol.CommonObjectSupport_GRANT, protocol.CommonObjectSupport_OBJECT_OWNER} {
		objects.Coverage = append(objects.Coverage, accessCoverage(kind, ""))
	}
	held, err := principals(ctx)
	if err != nil && !errors.Is(err, nativeapi.ErrPrincipalsRefused) {
		return err
	}
	reason := ""
	if err != nil {
		reason = "The server refused to report its users, groups and memberships"
	}
	for _, kind := range []protocol.CommonObjectSupport_Kind{protocol.CommonObjectSupport_ROLE, protocol.CommonObjectSupport_ROLE_MEMBERSHIP} {
		objects.Coverage = append(objects.Coverage, accessCoverage(kind, reason))
	}
	if err == nil {
		recordPrincipals(objects, held, scope)
	}
	// The owners and grants stay in the order the read met them, which is the
	// host's embedded engine's: by target in readingOrder, each target's
	// grants as YDB describes its entries. The roles and the memberships are
	// sorted, as that engine sorted them (recordPrincipals). A schema render
	// writes the GRANT statements and the roles in this order. The names left
	// out of scope are sorted too: a note counts them and names none.
	slices.Sort(objects.RolesOutOfScope)
	candidate := proto.CloneOf(snapshot)
	candidate.CommonObjects = objects
	if err := provider.ValidateSchema(candidate); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot.CommonObjects = objects
	return nil
}

// targetAccess describes one target and records its owner and grants in
// objects. describers holds the subjects each directory of a secret grants
// describe access to, and is filled as the read meets those directories.
func targetAccess(ctx context.Context, objects *protocol.ObservedCommonObjects, target accessTarget,
	describe func(context.Context, string) (*Ydb_Scheme.Entry, error), describers map[string]map[string]bool,
) error {
	entry, err := describe(ctx, target.absolute)
	// A default pool can be listed before YDB creates its scheme object.
	// This exception describes no owner or grant for that path.
	if target.reference.Kind == "resource pool" && target.reference.Name == "default" && errors.Is(err, nativeapi.ErrAccessPathNotVisible) {
		return nil
	}
	// A selected directory the listing proved absent (see
	// nativeapi.ListDirectory) has no owner and no grant to describe.
	if target.reference.Kind == "schema" && errors.Is(err, nativeapi.ErrAccessPathNotVisible) {
		return nil
	}
	if err != nil {
		return err
	}
	// Nor does one whose path an object holds (see traversal.directory): the
	// access list there is the object's, read with the object.
	if target.reference.Kind == "schema" && entry.GetName() == path.Base(target.absolute) && occupant(entry.GetType()) {
		return nil
	}
	if entry.GetName() != path.Base(target.absolute) || !slices.Contains(target.types, entry.GetType()) {
		return fmt.Errorf("YDB access description disagrees with the captured object")
	}
	var implicit func(subject, permission string) bool
	if target.reference.Kind == "secret" {
		subjects, err := secretDescribers(ctx, describe, path.Dir(target.absolute), describers)
		if err != nil {
			return err
		}
		implicit = func(subject, permission string) bool {
			return permission == ydbacl.DescribeSchemaPermission && subjects[subject]
		}
	}
	return recordAccess(objects, target.reference, entry, implicit)
}

func accessCoverage(kind protocol.CommonObjectSupport_Kind, reason string) *protocol.CommonObjectCoverage {
	knowledge := &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}
	if reason != "" {
		knowledge = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: reason}
	}
	return &protocol.CommonObjectCoverage{Kind: kind, Knowledge: knowledge}
}

type accessTarget struct {
	reference *protocol.Reference
	absolute  string
	types     []Ydb_Scheme.Entry_Type
}

func accessTargets(snapshot *protocol.Schema, database string) ([]accessTarget, error) {
	if !path.IsAbs(database) || path.Clean(database) != database {
		return nil, fmt.Errorf("YDB access database is not an exact absolute path")
	}
	targets := &accessTargetSet{database: database, seen: make(map[string]*protocol.Reference)}
	if err := targets.add(&protocol.Reference{Engine: "ydb", Database: database, Kind: "database"}, database, Ydb_Scheme.Entry_DATABASE, Ydb_Scheme.Entry_DIRECTORY); err != nil {
		return nil, err
	}
	for _, table := range snapshot.GetTables() {
		ref := table.GetIdentity()
		if err := targets.add(ref, path.Join(database, ref.GetSchema(), ref.GetName()), Ydb_Scheme.Entry_TABLE, Ydb_Scheme.Entry_COLUMN_TABLE); err != nil {
			return nil, err
		}
	}
	for _, view := range snapshot.GetCommonObjects().GetViews() {
		ref := view.GetIdentity()
		if err := targets.add(ref, path.Join(database, ref.GetSchema(), ref.GetName()), Ydb_Scheme.Entry_VIEW); err != nil {
			return nil, err
		}
	}
	for _, object := range snapshot.GetNativeObjects() {
		if object.GetKind() == schema.ResourcePoolClassifierProperties {
			continue
		}
		kind, types, err := nativeAccessKind(object.GetKind())
		if err != nil {
			return nil, err
		}
		ref := proto.CloneOf(object.GetOwner())
		ref.Kind = kind
		absolute := path.Join(database, ref.Schema, ref.Name)
		if ref.Kind == "resource pool" {
			absolute = path.Join(database, poolAccessDirectory, ref.Name)
		}
		if err := targets.add(ref, absolute, types...); err != nil {
			return nil, err
		}
	}
	// The directories go last: one whose path an object of the read holds is
	// no directory, and its access list is the object's.
	if err := targets.addDirectories(snapshot.GetCoverage().GetKinds()); err != nil {
		return nil, err
	}
	slices.SortFunc(targets.result, readingOrder)
	return targets.result, nil
}

// readingOrder orders access targets as the host's embedded engine read them,
// so the grants, owners and principals of a read come in its order: the
// resource pools first, which it read from the system views before it walked
// the database, then the database and each directory before the entries in
// it, the entries of a directory by name and a directory's own entries where
// its name falls among them. Comparing the paths one component at a time is
// that walk: a whole-string comparison puts `a.b` before the entries of `a`.
func readingOrder(a, b accessTarget) int {
	pool := func(target accessTarget) int {
		if target.reference.GetKind() == "resource pool" {
			return 0
		}
		return 1
	}
	return cmp.Or(cmp.Compare(pool(a), pool(b)), slices.Compare(strings.Split(a.absolute, "/"), strings.Split(b.absolute, "/")))
}

// accessTargetSet collects the access targets of one database, one per
// physical path, in the order they are first added.
type accessTargetSet struct {
	database string
	result   []accessTarget
	seen     map[string]*protocol.Reference
}

// add records reference at the absolute path, which YDB describes as one of
// types. It refuses a reference outside the database or with an inexact path,
// and a path two references name differently; a path added again with the
// same reference is recorded once.
func (s *accessTargetSet) add(reference *protocol.Reference, absolute string, types ...Ydb_Scheme.Entry_Type) error {
	if reference.GetEngine() != "ydb" || reference.GetDatabase() != s.database || reference.GetCatalog() != "" || len(reference.GetPath())+len(reference.GetOverload()) != 0 {
		return fmt.Errorf("YDB access target is outside the captured database")
	}
	for _, component := range []string{reference.GetSchema(), reference.GetName()} {
		if component != "" && (path.IsAbs(component) || path.Clean(component) != component || component == "." || component == ".." || strings.HasPrefix(component, "../")) {
			return fmt.Errorf("YDB access target has an inexact path")
		}
	}
	if path.Clean(absolute) != absolute || (absolute != s.database && !strings.HasPrefix(absolute, strings.TrimRight(s.database, "/")+"/")) {
		return fmt.Errorf("YDB access target escapes the captured database")
	}
	if previous, exists := s.seen[absolute]; exists {
		if !proto.Equal(previous, reference) {
			return fmt.Errorf("YDB access targets disagree about one physical path")
		}
		return nil
	}
	s.seen[absolute] = proto.CloneOf(reference)
	s.result = append(s.result, accessTarget{stripped(reference), absolute, types})
	return nil
}

// addDirectories adds every directory below the root that a kind's coverage
// knows completely, in the order the coverage lists them, except one whose
// path an object target already added holds.
func (s *accessTargetSet) addDirectories(kinds []*protocol.ModelKindCoverage) error {
	for _, kind := range kinds {
		for _, region := range kind.GetRegions() {
			if region.GetKnowledge().GetState() != protocol.ModelKnowledge_COMPLETE {
				continue
			}
			directory := region.GetScope().GetSchema().GetSource()
			// The root of a nested database, and a directory below one, is no
			// directory of this database: the access list there is the
			// nested database's (see traversal.nestedDirectory).
			if _, nested := schema.NestedDatabase(region.GetKnowledge()); nested || directory == "" {
				continue
			}
			if region.GetScope().GetDatabase().GetSource() != s.database {
				return fmt.Errorf("YDB directory knowledge is outside the captured database")
			}
			if held, found := s.seen[path.Join(s.database, directory)]; found && held.GetKind() != "schema" {
				continue
			}
			if err := s.add(&protocol.Reference{Engine: "ydb", Database: s.database, Kind: "schema", Name: directory}, path.Join(s.database, directory), Ydb_Scheme.Entry_DIRECTORY); err != nil {
				return err
			}
		}
	}
	return nil
}

// nativeAccessKind returns the access kind of a native object kind and the
// entry types YDB describes it as.
func nativeAccessKind(kind string) (string, []Ydb_Scheme.Entry_Type, error) {
	switch kind {
	case schema.SecretProperties:
		return "secret", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_SECRET}, nil
	case schema.TopicProperties:
		return "topic", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_TOPIC, Ydb_Scheme.Entry_PERS_QUEUE_GROUP}, nil
	case schema.StreamingQueryProperties:
		return "streaming query", []Ydb_Scheme.Entry_Type{streamingQueryEntry}, nil
	case schema.ReplicationProperties:
		return "async replication", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_REPLICATION}, nil
	case schema.TransferProperties:
		return "transfer", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_TRANSFER}, nil
	case schema.CoordinationProperties:
		return "coordination node", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_COORDINATION_NODE}, nil
	case schema.ExternalSourceProperties:
		return "external data source", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE}, nil
	case schema.ExternalTableProperties:
		return "external table", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_EXTERNAL_TABLE}, nil
	case schema.ViewProperties:
		return "view", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_VIEW}, nil
	case schema.ResourcePoolProperties:
		return "resource pool", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_RESOURCE_POOL}, nil
	default:
		return "", nil, fmt.Errorf("YDB access reader does not recognize native object kind %q", kind)
	}
}

// secretDescribers returns the subjects whose effective permissions on the
// directory at absolute carry ydbacl.DescribeSchemaPermission, which are the
// subjects YDB gives a secret it creates there an entry for. Each directory is
// described once per read.
func secretDescribers(ctx context.Context, describe func(context.Context, string) (*Ydb_Scheme.Entry, error), absolute string,
	known map[string]map[string]bool,
) (map[string]bool, error) {
	if subjects, found := known[absolute]; found {
		return subjects, nil
	}
	entry, err := describe(ctx, absolute)
	if err != nil {
		return nil, err
	}
	if entry.GetName() != path.Base(absolute) || (entry.GetType() != Ydb_Scheme.Entry_DIRECTORY && entry.GetType() != Ydb_Scheme.Entry_DATABASE) {
		return nil, fmt.Errorf("YDB access description disagrees with the directory of a captured secret")
	}
	subjects := make(map[string]bool)
	for _, permission := range entry.GetEffectivePermissions() {
		if slices.ContainsFunc(permission.GetPermissionNames(), ydbacl.DescribesSchema) {
			subjects[permission.GetSubject()] = true
		}
	}
	known[absolute] = subjects
	return subjects, nil
}

// recordAccess records one object's owner and one grant per permission each
// of its entries names. implicit, when not nil, marks the grants the object
// holds because YDB created it rather than because a GRANT made them.
func recordAccess(objects *protocol.ObservedCommonObjects, reference *protocol.Reference, entry *Ydb_Scheme.Entry, implicit func(subject, permission string) bool) error {
	if entry.GetOwner() != "" {
		objects.ObjectOwners = append(objects.ObjectOwners, &protocol.ObjectOwner{Object: proto.CloneOf(reference), Owner: entry.GetOwner()})
	}
	target := proto.CloneOf(reference)
	if target.Kind == "view" {
		target.Kind = "table"
	}
	seen := make(map[string]bool)
	for _, permission := range entry.GetPermissions() {
		if strings.TrimSpace(permission.GetSubject()) == "" {
			return fmt.Errorf("YDB reports a permission without a principal")
		}
		pool := target.Kind == "resource pool" && target.Name == "default" && defaultPoolEntry(permission.GetPermissionNames())
		for _, name := range permission.GetPermissionNames() {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("YDB reports an empty permission")
			}
			key := permission.GetSubject() + "\x00" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			objects.Grants = append(objects.Grants, &protocol.ObservedGrant{Role: permission.GetSubject(), Privilege: name, Target: proto.CloneOf(target),
				Implicit: pool || (implicit != nil && implicit(permission.GetSubject(), name))})
		}
	}
	return nil
}

// YDB gives the default pool a combined permission entry. Explicit GRANT
// statements produce separate entries, even when they name both permissions.
func defaultPoolEntry(names []string) bool {
	return len(names) == 2 && slices.Contains(names, "ydb.granular.select_row") && slices.Contains(names, "ydb.granular.describe_schema")
}

// RolesOutOfScopeReason is why a read leaves the users and groups
// recordPrincipals names out of scope out of the description, in the words
// of the host's embedded engine, which the host prints after "because".
const RolesOutOfScopeReason = "the cluster made them or they own the database, or the read was of a dev realm, which shares its database's users and groups"

func recordPrincipals(objects *protocol.ObservedCommonObjects, held nativeapi.Principals, scope Scope) {
	owner := ""
	for _, entry := range objects.ObjectOwners {
		if entry.Object.Kind == "database" {
			owner = entry.Owner
		}
	}
	canLogin := make(map[string]bool)
	add := func(role *protocol.ObservedRole) {
		if scope == RealmScope || role.Name == owner || ydbacl.CheckName(role.Name) != nil {
			objects.RolesOutOfScope = append(objects.RolesOutOfScope, role.Name)
			return
		}
		objects.Roles = append(objects.Roles, role)
	}
	for _, user := range held.Users {
		canLogin[user.Name] = user.Enabled
		add(&protocol.ObservedRole{Name: user.Name, Login: user.Enabled, Inherit: true})
	}
	for _, group := range held.Groups {
		canLogin[group] = false
		add(&protocol.ObservedRole{Name: group, Group: true, Inherit: true})
	}
	for _, membership := range held.Memberships {
		objects.Memberships = append(objects.Memberships, &protocol.RoleMembership{Group: membership.Group, Member: membership.Member})
	}
	// The YDB engine built into the host at 74de0b65b sorted the users and
	// the groups together by name, and the memberships by group and then
	// member, so `schema inspect --format sql` wrote CREATE GROUP readers
	// ahead of CREATE USER u. Kept in the order the system views list them,
	// every user came before every group.
	slices.SortStableFunc(objects.Roles, func(a, b *protocol.ObservedRole) int { return strings.Compare(a.GetName(), b.GetName()) })
	slices.SortStableFunc(objects.Memberships, func(a, b *protocol.RoleMembership) int {
		return cmp.Or(strings.Compare(a.GetGroup(), b.GetGroup()), strings.Compare(a.GetMember(), b.GetMember()))
	})
	for _, entry := range objects.ObjectOwners {
		if login, known := canLogin[entry.Owner]; known {
			entry.OwnerCanLogin = new(login)
		}
	}
}
