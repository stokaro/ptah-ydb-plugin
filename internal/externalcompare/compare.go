// Package externalcompare compares the declared and the read external data
// sources and external tables offline, in one batch, as Ptah's embedded YDB
// engine compares them: a data source the plan drops and creates again takes
// the external tables over it along, and a plan that drops a data source a
// declared table reads is refused.
package externalcompare

import (
	"context"
	"fmt"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the owner of external object comparisons.
const ServiceID = "ydb-external-objects"

// Kinds are the object models the service compares, in order.
func Kinds() []string {
	return []string{schema.ExternalSourceProperties, schema.ExternalTableProperties}
}

// ChangeKinds are the change models the service reports.
func ChangeKinds() []string { return []string{schema.ExternalSourceChange, schema.ExternalTableChange} }

// Compare evaluates complete operands and coverage without database access.
// Two objects of one kind are equal when externals.SameDataSource or
// externals.SameTable says so, read against the database the current side
// was read from. A declaration on a target without the external_data_sources
// capability is refused with the host's words, as the host's schema
// validation refuses it, and an object a read of such a target found is left
// undecided, as the host's reader leaves it unread.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	root, err := objectcompare.SessionDatabase(request)
	if err != nil {
		return nil, err
	}
	declared, err := declarations(request, root)
	if err != nil {
		return nil, err
	}
	for _, object := range declared {
		if err := object.check(request.Capabilities); err != nil {
			return nil, err
		}
	}
	result, decided, err := objectcompare.Families(ctx, request, []objectcompare.Family{Family(schema.ExternalSourceProperties, root), Family(schema.ExternalTableProperties, root)})
	if err != nil {
		return nil, err
	}
	if err := refuseOrphanedTables(decided, declared, root); err != nil {
		return nil, err
	}
	extra, err := tablesOverRecreatedSources(decided, request.Capabilities[externals.CapabilityReplace], root)
	if err != nil {
		return nil, err
	}
	result.Changes = append(result.Changes, extra...)
	slices.SortStableFunc(result.Changes, func(a, b *protocol.ComparedChange) int {
		if a.Kind != b.Kind {
			return slices.Index(Kinds(), a.Kind) - slices.Index(Kinds(), b.Kind)
		}
		return provider.CompareIdentities(a.Subject, b.Subject)
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// Family is one kind's part of the comparison, its paths read against root.
func Family(kind, root string) objectcompare.Family {
	noun := "external data source"
	if kind == schema.ExternalTableProperties {
		noun = "external table"
	}
	return objectcompare.Family{
		ServiceID: ServiceID, Kind: kind, Noun: noun,
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(kind) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(kind, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize, RequiredObjects: schema.RequiredObjects,
		Reserved: func(*protocol.ComparisonIdentity) string { return "" },
		Unread: func(capabilities map[string]bool) string {
			if capabilities[externals.CapabilityExternalDataSources] {
				return ""
			}
			return externals.UnsupportedReason
		},
		Unsettled: schema.ValidateExternalAccess,
		Equal: func(current, desired *protocol.Envelope) (bool, error) {
			return externals.Equal(current, desired, root)
		},
		Change: func(id string, owner *protocol.Reference, current, desired *protocol.Envelope) (*protocol.NormalizedValue, error) {
			return schema.CaptureExternalChange(kind, id, owner, root, current, desired)
		},
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			return externals.Keep(owner, current, root)
		},
	}
}

// declared is one object a desired record states.
type declared struct {
	kind    string
	subject *protocol.ComparisonIdentity
	display string
	source  *ydbdecl.ExternalDataSource
	table   *ydbdecl.ExternalTable
}

func (d declared) check(capabilities map[string]bool) error {
	if d.source != nil {
		return externals.CheckDataSource(d.display, *d.source, capabilities)
	}
	return externals.CheckTable(d.display, *d.table, capabilities)
}

// declarations reads every desired record of the service's kinds, in
// identity order.
func declarations(request *protocol.CompareModelsRequest, root string) ([]declared, error) {
	var result []declared
	for _, record := range request.GetDesired().GetRecords() {
		if !slices.Contains(Kinds(), record.GetKind()) {
			continue
		}
		value := record.GetValue().GetValue()
		object := declared{kind: record.Kind, subject: record.Subject,
			display: ydbdecl.ExternalDisplay(record.GetSubject().GetSchema().GetSource(), record.GetSubject().GetName().GetSource())}
		if record.Kind == schema.ExternalSourceProperties {
			intent, err := schema.DesiredExternalSource(value)
			if err != nil {
				return nil, err
			}
			spec, err := externals.SourceFromIntent(intent, schema.InterpretationRoot(value))
			if err != nil {
				return nil, fmt.Errorf("external data source %s: %w", object.display, err)
			}
			object.source = &spec
		} else {
			intent, err := schema.DesiredExternalTable(value)
			if err != nil {
				return nil, err
			}
			spec, err := externals.TableFromIntent(intent, schema.InterpretationRoot(value))
			if err != nil {
				return nil, fmt.Errorf("external table %s: %w", object.display, err)
			}
			object.table = &spec
		}
		result = append(result, object)
	}
	slices.SortStableFunc(result, func(a, b declared) int { return provider.CompareIdentities(a.subject, b.subject) })
	return result, nil
}

// pathOf is the path an object's owner names, relative to the database root.
func pathOf(owner *protocol.Reference) ydbdecl.Path {
	return ydbdecl.Path{Schema: owner.GetSchema(), Name: owner.GetName()}
}

// refuseOrphanedTables refuses a plan that drops a data source a declared
// external table reads. The plan would keep the table over a source that is
// gone: 25.4.1.15 and later refuse the drop, and 25.1.4.7 to 25.3.1.25 take
// it and leave a table nothing can drop (`path hasn't been resolved`), as the
// host measured.
func refuseOrphanedTables(decided []objectcompare.Decided, tables []declared, root string) error {
	dropped := map[ydbdecl.Path]string{}
	for _, object := range decided {
		if object.Kind == schema.ExternalSourceProperties && object.Change != nil && object.Desired == nil {
			dropped[pathOf(object.Owner)] = ydbdecl.ExternalDisplay(object.Owner.GetSchema(), object.Owner.GetName())
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	for _, table := range tables {
		if table.table == nil {
			continue
		}
		source, pathErr := externalpath.SourcePath(root, table.table.DataSource)
		if name, gone := dropped[source]; pathErr == nil && gone {
			return fmt.Errorf("external table %s reads data source %s, which the plan drops; declare the data source or move the table to one the plan keeps", table.display, name)
		}
	}
	return nil
}

// tablesOverRecreatedSources asks for each declared table the database holds
// over a data source the plan drops and creates again, or replaces with a new
// source type, and that holds what is declared: YDB keeps no table over a
// dropped data source, and CREATE OR REPLACE refuses a new source type while
// a table reads the source (measured on 26.2.1.14 by the host). The change's
// operands may describe the same table.
func tablesOverRecreatedSources(decided []objectcompare.Decided, replace bool, root string) ([]*protocol.ComparedChange, error) {
	recreated := map[ydbdecl.Path]bool{}
	for _, object := range decided {
		if object.Kind != schema.ExternalSourceProperties || object.Change == nil || object.Current == nil || object.Desired == nil {
			continue
		}
		current, desired, err := externals.SourcePair(object.Current, object.Desired, root)
		if err != nil {
			return nil, err
		}
		if externals.Displaces(current, desired, replace) {
			recreated[pathOf(object.Owner)] = true
		}
	}
	if len(recreated) == 0 {
		return nil, nil
	}
	var extra []*protocol.ComparedChange
	for _, object := range decided {
		if object.Kind != schema.ExternalTableProperties || object.Change != nil || object.Current == nil || object.Desired == nil {
			continue
		}
		_, desired, err := externals.TablePair(object.Current, object.Desired, root)
		if err != nil {
			return nil, err
		}
		source, pathErr := externalpath.SourcePath(root, desired.DataSource)
		if pathErr != nil || !recreated[source] {
			continue
		}
		change, err := schema.CaptureExternalChange(schema.ExternalTableProperties, fmt.Sprintf("change-together-%d", len(extra)), object.Owner, root, object.Current, object.Desired)
		if err != nil {
			return nil, err
		}
		extra = append(extra, &protocol.ComparedChange{Kind: schema.ExternalTableProperties, Subject: proto.CloneOf(object.Subject), Value: change})
	}
	return extra, nil
}
