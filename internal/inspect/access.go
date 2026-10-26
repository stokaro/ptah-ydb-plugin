package inspect

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
)

const poolAccessDirectory = ".metadata/workload_manager/pools"

// Access reads the permissions and owners of the objects a snapshot captured,
// its listed directories, and its root. Principals belong to the whole database;
// a realm records their existence without declaring that it owns them. A read
// failure leaves the snapshot unchanged. A specific principal-visibility refusal
// retains object access and records unknown role and membership coverage.
func Access(ctx context.Context, snapshot *protocol.Schema, database string, realm bool,
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
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := describe(ctx, target.absolute)
		// A default pool can be listed before YDB creates its scheme object.
		// This exception describes no owner or grant for that path.
		if target.reference.Kind == "resource pool" && target.reference.Name == "default" && errors.Is(err, nativeapi.ErrAccessPathNotVisible) {
			continue
		}
		if err != nil {
			return err
		}
		if entry.GetName() != path.Base(target.absolute) || !slices.Contains(target.types, entry.GetType()) {
			return fmt.Errorf("YDB access description disagrees with the captured object")
		}
		if err := recordAccess(objects, target.reference, entry); err != nil {
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
		recordPrincipals(objects, held, realm)
	}
	sortAccess(objects)
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
	var result []accessTarget
	seen := map[string]*protocol.Reference{}
	add := func(reference *protocol.Reference, absolute string, types ...Ydb_Scheme.Entry_Type) error {
		if reference.GetEngine() != "ydb" || reference.GetDatabase() != database || reference.GetCatalog() != "" || len(reference.GetPath())+len(reference.GetOverload()) != 0 {
			return fmt.Errorf("YDB access target is outside the captured database")
		}
		for _, component := range []string{reference.GetSchema(), reference.GetName()} {
			if component != "" && (path.IsAbs(component) || path.Clean(component) != component || component == "." || component == ".." || strings.HasPrefix(component, "../")) {
				return fmt.Errorf("YDB access target has an inexact path")
			}
		}
		if path.Clean(absolute) != absolute || (absolute != database && !strings.HasPrefix(absolute, strings.TrimRight(database, "/")+"/")) {
			return fmt.Errorf("YDB access target escapes the captured database")
		}
		if previous, exists := seen[absolute]; exists {
			if !proto.Equal(previous, reference) {
				return fmt.Errorf("YDB access targets disagree about one physical path")
			}
			return nil
		}
		seen[absolute] = proto.CloneOf(reference)
		result = append(result, accessTarget{stripped(reference), absolute, types})
		return nil
	}
	if err := add(&protocol.Reference{Engine: "ydb", Database: database, Kind: "database"}, database, Ydb_Scheme.Entry_DATABASE, Ydb_Scheme.Entry_DIRECTORY); err != nil {
		return nil, err
	}
	for _, kind := range snapshot.GetCoverage().GetKinds() {
		for _, region := range kind.GetRegions() {
			if region.GetKnowledge().GetState() != protocol.ModelKnowledge_COMPLETE {
				continue
			}
			directory := region.GetScope().GetSchema().GetSource()
			if directory == "" {
				continue
			}
			if region.GetScope().GetDatabase().GetSource() != database {
				return nil, fmt.Errorf("YDB directory knowledge is outside the captured database")
			}
			if err := add(&protocol.Reference{Engine: "ydb", Database: database, Kind: "schema", Name: directory}, path.Join(database, directory), Ydb_Scheme.Entry_DIRECTORY); err != nil {
				return nil, err
			}
		}
	}
	for _, table := range snapshot.GetTables() {
		ref := table.GetIdentity()
		if err := add(ref, path.Join(database, ref.GetSchema(), ref.GetName()), Ydb_Scheme.Entry_TABLE, Ydb_Scheme.Entry_COLUMN_TABLE); err != nil {
			return nil, err
		}
	}
	for _, view := range snapshot.GetCommonObjects().GetViews() {
		ref := view.GetIdentity()
		if err := add(ref, path.Join(database, ref.GetSchema(), ref.GetName()), Ydb_Scheme.Entry_VIEW); err != nil {
			return nil, err
		}
	}
	for _, object := range snapshot.GetNativeObjects() {
		if object.GetKind() == schema.ResourcePoolClassifierProperties {
			continue
		}
		ref := proto.CloneOf(object.GetOwner())
		var types []Ydb_Scheme.Entry_Type
		switch object.GetKind() {
		case schema.TopicProperties:
			ref.Kind, types = "topic", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_TOPIC, Ydb_Scheme.Entry_PERS_QUEUE_GROUP}
		case schema.StreamingQueryProperties:
			ref.Kind, types = "streaming query", []Ydb_Scheme.Entry_Type{streamingQueryEntry}
		case schema.CoordinationProperties:
			ref.Kind, types = "coordination node", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_COORDINATION_NODE}
		case schema.ExternalSourceProperties:
			ref.Kind, types = "external data source", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE}
		case schema.ExternalTableProperties:
			ref.Kind, types = "external table", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_EXTERNAL_TABLE}
		case schema.ViewProperties:
			ref.Kind, types = "view", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_VIEW}
		case schema.ResourcePoolProperties:
			ref.Kind, types = "resource pool", []Ydb_Scheme.Entry_Type{Ydb_Scheme.Entry_RESOURCE_POOL}
		default:
			return nil, fmt.Errorf("YDB access reader does not recognize native object kind %q", object.GetKind())
		}
		absolute := path.Join(database, ref.Schema, ref.Name)
		if ref.Kind == "resource pool" {
			absolute = path.Join(database, poolAccessDirectory, ref.Name)
		}
		if err := add(ref, absolute, types...); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(result, func(a, b accessTarget) int { return strings.Compare(a.absolute, b.absolute) })
	return result, nil
}

func recordAccess(objects *protocol.ObservedCommonObjects, reference *protocol.Reference, entry *Ydb_Scheme.Entry) error {
	if entry.GetOwner() != "" {
		objects.ObjectOwners = append(objects.ObjectOwners, &protocol.ObjectOwner{Object: proto.CloneOf(reference), Owner: entry.GetOwner()})
	}
	target := proto.CloneOf(reference)
	if target.Kind == "view" {
		target.Kind = "table"
	}
	seen := map[string]bool{}
	for _, permission := range entry.GetPermissions() {
		if strings.TrimSpace(permission.GetSubject()) == "" {
			return fmt.Errorf("YDB reports a permission without a principal")
		}
		implicit := target.Kind == "resource pool" && target.Name == "default" && defaultPoolEntry(permission.GetPermissionNames())
		for _, name := range permission.GetPermissionNames() {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("YDB reports an empty permission")
			}
			key := permission.GetSubject() + "\x00" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			objects.Grants = append(objects.Grants, &protocol.ObservedGrant{Role: permission.GetSubject(), Privilege: name, Target: proto.CloneOf(target), Implicit: implicit})
		}
	}
	return nil
}

// YDB gives the default pool a combined permission entry. Explicit GRANT
// statements produce separate entries, even when they name both permissions.
func defaultPoolEntry(names []string) bool {
	return len(names) == 2 && slices.Contains(names, "ydb.granular.select_row") && slices.Contains(names, "ydb.granular.describe_schema")
}

func recordPrincipals(objects *protocol.ObservedCommonObjects, held nativeapi.Principals, realm bool) {
	owner := ""
	for _, entry := range objects.ObjectOwners {
		if entry.Object.Kind == "database" {
			owner = entry.Owner
		}
	}
	canLogin := map[string]bool{}
	add := func(role *protocol.ObservedRole) {
		if realm || role.Name == owner || ydbacl.CheckName(role.Name) != nil {
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
	for _, entry := range objects.ObjectOwners {
		if login, known := canLogin[entry.Owner]; known {
			entry.OwnerCanLogin = new(login)
		}
	}
}

func sortAccess(objects *protocol.ObservedCommonObjects) {
	refKey := func(ref *protocol.Reference) string { return ref.Kind + "\x00" + ref.Schema + "\x00" + ref.Name }
	slices.SortFunc(objects.ObjectOwners, func(a, b *protocol.ObjectOwner) int { return strings.Compare(refKey(a.Object), refKey(b.Object)) })
	slices.SortFunc(objects.Grants, func(a, b *protocol.ObservedGrant) int {
		return cmp.Or(strings.Compare(refKey(a.Target), refKey(b.Target)), strings.Compare(a.Role, b.Role), strings.Compare(a.Privilege, b.Privilege))
	})
	slices.SortFunc(objects.Roles, func(a, b *protocol.ObservedRole) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(objects.RolesOutOfScope)
	slices.SortFunc(objects.Memberships, func(a, b *protocol.RoleMembership) int {
		return cmp.Or(strings.Compare(a.Group, b.Group), strings.Compare(a.Member, b.Member))
	})
}
