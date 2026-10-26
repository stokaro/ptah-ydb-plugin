package inspect

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
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
	result.Coverage.Kinds = []*protocol.ModelKindCoverage{{Model: model, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Streaming inventory is limited to the selected database"}, Regions: []*protocol.ModelNamespaceKnowledge{{Scope: &protocol.ModelNamespace{Database: &protocol.NamespaceComponent{Source: database, Normalized: database}}, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}}}}
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
		children := slices.Clone(listing.GetChildren())
		slices.SortFunc(children, func(a, b *Ydb_Scheme.Entry) int { return strings.Compare(a.GetName(), b.GetName()) })
		names := make(map[string]bool)
		for _, entry := range children {
			name := entry.GetName()
			if !component(name) || names[name] {
				return nil, fmt.Errorf("invalid or duplicate streaming inventory entry")
			}
			names[name] = true
			if entry.GetType() == Ydb_Scheme.Entry_DIRECTORY {
				pending = append(pending, path.Join(directory, name))
				continue
			}
			if entry.GetType() != streamingQueryEntry {
				continue
			}
			owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Database: database, Schema: directory, Name: name}
			var observed *protocol.Envelope
			readErr := fmt.Errorf("the streaming query capability was not established")
			if read != nil {
				spec, err := read(ctx, path.Join(absolute, name))
				readErr = err
				if err == nil && spec == nil {
					readErr = fmt.Errorf("the listed streaming query has no stored settings")
				}
				if readErr == nil {
					observed, readErr = schema.CaptureObject(owner, &native.StreamingQuerySettings{Text: spec.Text, Run: spec.Run, ResourcePool: spec.ResourcePool})
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if readErr != nil {
				result.Coverage.Subjects = append(result.Coverage.Subjects, &protocol.ModelSubjectCoverage{Kind: owner.Kind, Subject: proto.CloneOf(owner), NormalizedSubject: proto.CloneOf(owner), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: readErr.Error()}})
			} else {
				result.NativeObjects = append(result.NativeObjects, observed)
				observe(result, []*protocol.Envelope{observed})
			}
			if len(result.NativeObjects)+len(result.Coverage.Subjects) > 4096 || proto.Size(result) > 3<<20 {
				return nil, fmt.Errorf("streaming inventory exceeds capture limit")
			}
		}
	}
	return result, nil
}

func replaceStreamingInventory(snapshot, inventory *protocol.Schema) {
	snapshot.NativeObjects = slices.DeleteFunc(snapshot.NativeObjects, func(value *protocol.Envelope) bool { return value.Kind == schema.StreamingQueryProperties })
	snapshot.NativeObjects = append(snapshot.NativeObjects, inventory.NativeObjects...)
	snapshot.Coverage.Kinds = slices.DeleteFunc(snapshot.Coverage.Kinds, func(value *protocol.ModelKindCoverage) bool {
		return value.Model.Kind == schema.StreamingQueryProperties
	})
	snapshot.Coverage.Kinds = append(snapshot.Coverage.Kinds, inventory.Coverage.Kinds...)
	snapshot.Coverage.Subjects = slices.DeleteFunc(snapshot.Coverage.Subjects, func(value *protocol.ModelSubjectCoverage) bool { return value.Kind == schema.StreamingQueryProperties })
	snapshot.Coverage.Subjects = append(snapshot.Coverage.Subjects, inventory.Coverage.Subjects...)
}
