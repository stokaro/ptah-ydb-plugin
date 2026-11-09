package inspect

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ReadStreamingInventory lists every directory inside one exact database or
// realm, including dot-prefixed user directories. It does not inspect other
// object definitions. A listed query whose settings cannot be read stays
// explicitly unknown; a failed directory listing returns no inventory.
func ReadStreamingInventory(ctx context.Context, database string, list func(context.Context, string) (*Ydb_Scheme.ListDirectoryResult, error), read StreamingQuery) (*protocol.Schema, error) {
	if err := schema.ValidateDatabaseContext(&protocol.DatabaseContext{Database: database, SourceDatabase: database}); err != nil {
		return nil, err
	}
	if list == nil {
		return nil, fmt.Errorf("streaming inventory needs a directory reader")
	}
	result := &protocol.Schema{Complete: true, Engine: "ydb", Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED}}
	var model *protocol.ValueModel
	for _, candidate := range schema.ObservedModels() {
		if candidate.Kind == schema.StreamingQueryProperties {
			model = candidate
		}
	}
	result.Coverage.Kinds = []*protocol.ModelKindCoverage{{
		Model:     model,
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Streaming inventory is limited to the selected database"},
		Regions: []*protocol.ModelNamespaceKnowledge{{
			Scope:     &protocol.ModelNamespace{Database: &protocol.NamespaceComponent{Source: database, Normalized: database}},
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
		}},
	}}
	pending := []string{""}
	visited := make(map[string]bool)
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := pending[0]
		pending = pending[1:]
		if visited[directory] || len(visited) >= 4096 {
			return nil, fmt.Errorf("invalid or excessive streaming directory traversal")
		}
		visited[directory] = true
		absolute := path.Join(database, directory)
		listing, err := list(ctx, absolute)
		if err != nil {
			return nil, fmt.Errorf("read streaming inventory directory %q: %w", absolute, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if listing.GetSelf().GetName() != path.Base(absolute) || (listing.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY && (directory != "" || listing.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE)) {
			return nil, fmt.Errorf("streaming inventory directory identity disagrees")
		}
		directories, err := inventoryEntries(ctx, result, database, directory, listing.GetChildren(), read)
		if err != nil {
			return nil, err
		}
		pending = append(pending, directories...)
	}
	return result, nil
}

// inventoryEntries captures the streaming queries among the entries of one
// listed directory into result, in name order, and returns the directory's
// subdirectories in the same order. A query whose settings cannot be read is
// recorded as uninspected.
func inventoryEntries(ctx context.Context, result *protocol.Schema, database, directory string, entries []*Ydb_Scheme.Entry, read StreamingQuery) ([]string, error) {
	absolute := path.Join(database, directory)
	children := slices.Clone(entries)
	slices.SortFunc(children, func(a, b *Ydb_Scheme.Entry) int { return strings.Compare(a.GetName(), b.GetName()) })
	names := make(map[string]bool)
	var directories []string
	for _, entry := range children {
		name := entry.GetName()
		if !component(name) || names[name] {
			return nil, fmt.Errorf("invalid or duplicate streaming inventory entry")
		}
		names[name] = true
		if entry.GetType() == Ydb_Scheme.Entry_DIRECTORY {
			directories = append(directories, path.Join(directory, name))
			continue
		}
		if entry.GetType() != streamingQueryEntry {
			continue
		}
		owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Database: database, Schema: directory, Name: name}
		observed, readErr := inventoryQuery(ctx, owner, path.Join(absolute, name), read)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil {
			result.Coverage.Subjects = append(result.Coverage.Subjects, &protocol.ModelSubjectCoverage{
				Kind: owner.Kind, Subject: proto.CloneOf(owner), NormalizedSubject: proto.CloneOf(owner),
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: readErr.Error()},
			})
		} else {
			result.NativeObjects = append(result.NativeObjects, observed)
			observe(result, []*protocol.Envelope{observed})
		}
		if len(result.NativeObjects)+len(result.Coverage.Subjects) > 4096 || proto.Size(result) > 3<<20 {
			return nil, fmt.Errorf("streaming inventory exceeds capture limit")
		}
	}
	return directories, nil
}

// inventoryQuery captures the stored settings of the streaming query owner
// names, read at queryPath. Its error is the reason the settings stay unknown:
// no reader, a failed read, or a query with no stored settings.
func inventoryQuery(ctx context.Context, owner *protocol.Reference, queryPath string, read StreamingQuery) (*protocol.Envelope, error) {
	if read == nil {
		return nil, fmt.Errorf("the streaming query capability was not established")
	}
	spec, err := read(ctx, queryPath)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, fmt.Errorf("the listed streaming query has no stored settings")
	}
	return schema.CaptureObject(owner, &native.StreamingQuerySettings{Text: spec.Text, Run: spec.Run, ResourcePool: spec.ResourcePool})
}

// RealmStreamingInventory gives a read that left out the dev realm
// directories realms the streaming query coverage that inventory, a read of
// every directory in the database, establishes, when no query lives in a
// realm. Absence there is what lets a query the read left out be no input
// reader, so a change to an input proceeds. A query in a realm keeps the
// narrower coverage, and a change to an input it might read is refused.
func RealmStreamingInventory(snapshot, inventory *protocol.Schema, realms []string) {
	inRealm := func(directory string) bool {
		return slices.ContainsFunc(realms, func(realm string) bool { return directory == realm || strings.HasPrefix(directory, realm+"/") })
	}
	if slices.ContainsFunc(inventory.NativeObjects, func(value *protocol.Envelope) bool { return inRealm(value.GetOwner().GetSchema()) }) ||
		slices.ContainsFunc(inventory.Coverage.GetSubjects(), func(value *protocol.ModelSubjectCoverage) bool { return inRealm(value.GetSubject().GetSchema()) }) {
		return
	}
	replaceStreamingInventory(snapshot, inventory)
}

// OutsideStreamingInventory is the planning context of a read filtered to the
// directories selected: the streaming queries inventory, a read of every
// directory in the database, finds outside them, with the inventory's
// database-wide coverage (ADR 0053). A query inside them is the snapshot's
// own native object. A query whose settings could not be read stays an
// unknown subject, which refuses a write of any input.
func OutsideStreamingInventory(inventory *protocol.Schema, selected []string) *protocol.PlanningContextInventory {
	outside := func(directory string) bool { return !slices.Contains(selected, directory) }
	result := &protocol.PlanningContextInventory{Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED,
		Kinds: slices.Clone(inventory.GetCoverage().GetKinds())}}
	for _, value := range inventory.GetNativeObjects() {
		if outside(value.GetOwner().GetSchema()) {
			result.Objects = append(result.Objects, value)
		}
	}
	for _, subject := range inventory.GetCoverage().GetSubjects() {
		if outside(subject.GetSubject().GetSchema()) {
			result.Coverage.Subjects = append(result.Coverage.Subjects, subject)
		}
	}
	return result
}

func replaceStreamingInventory(snapshot, inventory *protocol.Schema) {
	snapshot.NativeObjects = slices.DeleteFunc(snapshot.NativeObjects, func(value *protocol.Envelope) bool { return value.Kind == schema.StreamingQueryProperties })
	snapshot.NativeObjects = append(snapshot.NativeObjects, inventory.NativeObjects...)
	snapshot.Coverage.Kinds = slices.DeleteFunc(snapshot.Coverage.Kinds, func(value *protocol.ModelKindCoverage) bool {
		return value.Model.Kind == schema.StreamingQueryProperties
	})
	snapshot.Coverage.Kinds = append(snapshot.Coverage.Kinds, inventory.Coverage.Kinds...)
	// The inventory reads streaming queries alone. A name the snapshot reserved
	// for provider-owned storage keeps the unrepresentable subject reserve
	// wrote for this kind, since the reservation still lists the kind as a
	// conflict and a reply without that subject is refused.
	snapshot.Coverage.Subjects = slices.DeleteFunc(snapshot.Coverage.Subjects, func(value *protocol.ModelSubjectCoverage) bool {
		return value.Kind == schema.StreamingQueryProperties && !reservedSubject(snapshot, value)
	})
	snapshot.Coverage.Subjects = append(snapshot.Coverage.Subjects, inventory.Coverage.Subjects...)
}

// reservedSubject reports a subject reserve wrote: the identity of a reserved
// object under the subject's kind, known to be unrepresentable.
func reservedSubject(snapshot *protocol.Schema, value *protocol.ModelSubjectCoverage) bool {
	if value.GetKnowledge().GetState() != protocol.ModelKnowledge_UNREPRESENTABLE {
		return false
	}
	return slices.ContainsFunc(snapshot.ReservedObjects, func(object *protocol.ReservedObject) bool {
		subject := proto.CloneOf(object.GetObject())
		subject.Kind = value.Kind
		return proto.Equal(subject, value.GetSubject())
	})
}
