package inspect

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// Workload adds the resource pools and the classifiers of database to a
// snapshot Read returned for selectors. They belong to the whole database
// rather than to a directory, and a read of them establishes the absence of
// every other one.
//
// A read of the whole database reports them, and so does one whose selectors
// include the root, `/`, whose namespace they take: the provider contract
// binds every subject of a filtered read to a selected namespace, so a read
// of other directories alone leaves them uninspected. A nil read leaves them
// uninspected too, for the reason unread gives: a target whose
// EnableResourcePools flag is off lists nothing in its system views, which
// proves no absence, and a dev realm holds none of its own.
func Workload(ctx context.Context, snapshot *protocol.Schema, database string, selectors []string, read func(context.Context) (nativeapi.HeldWorkload, error), unread string) error {
	var records []*protocol.ModelKindCoverage
	for _, record := range snapshot.GetCoverage().GetKinds() {
		if schema.IsWorkloadObject(record.GetModel().GetKind()) {
			records = append(records, record)
		}
	}
	root := len(selectors) == 0 || slices.ContainsFunc(selectors, func(selector string) bool { return strings.Trim(selector, "/") == "" })
	if read != nil && !root {
		read, unread = nil, "A read of selected directories does not report the resource pools and classifiers of the whole database; select / to read them"
	}
	if read == nil {
		for _, record := range records {
			record.Knowledge = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: unread}
		}
		return nil
	}
	held, err := read(ctx)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(held.Pools)) {
		value, err := schema.CaptureWorkloadObservation(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Database: database, Name: name},
			schema.PoolSettings(held.Pools[name]))
		if err != nil {
			return err
		}
		snapshot.NativeObjects = append(snapshot.NativeObjects, value)
		observe(snapshot, []*protocol.Envelope{value})
	}
	for _, name := range slices.Sorted(maps.Keys(held.Classifiers)) {
		value, err := schema.CaptureWorkloadObservation(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolClassifierProperties, Database: database, Name: name},
			schema.ClassifierSettings(held.Classifiers[name]))
		if err != nil {
			return err
		}
		snapshot.NativeObjects = append(snapshot.NativeObjects, value)
		observe(snapshot, []*protocol.Envelope{value})
	}
	for _, record := range records {
		// Even an unfiltered read establishes absence only in this database.
		// The host cannot use global model knowledge as a session-bound claim.
		record.Knowledge = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Workload observations cover only the selected database root"}
		record.Regions = append(record.Regions, &protocol.ModelNamespaceKnowledge{
			Scope:     &protocol.ModelNamespace{Database: &protocol.NamespaceComponent{Source: database, Normalized: database}, Catalog: &protocol.NamespaceComponent{}, Schema: &protocol.NamespaceComponent{}},
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
		})
	}
	slices.SortStableFunc(snapshot.NativeObjects, func(a, b *protocol.Envelope) int {
		if order := strings.Compare(a.Owner.Schema, b.Owner.Schema); order != 0 {
			return order
		}
		return strings.Compare(a.Owner.Name, b.Owner.Name)
	})
	return provider.ValidateSchema(snapshot)
}
