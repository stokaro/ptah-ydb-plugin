package nativeapi

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// External owns the precondition, the dispatch and the confirmation of one
// external data source or external table operation. Both objects change
// through YQL, as the host's embedded engine changes them, and are read
// through the table service.
type External struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	Table  Ydb_Table_V1.TableServiceClient
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
}

// NewExternal binds the session's native clients and statement runner.
func NewExternal(driver *ydb.Driver, execute func(context.Context, string) error) *External {
	conn := ydb.GRPCConn(driver)
	return &External{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(conn), Table: Ydb_Table_V1.NewTableServiceClient(conn), Execute: execute}
}

// Apply verifies that the object is as the plan read it, or absent for a
// creation and a restore, runs the operation's statements one query each,
// and reads the result back. An object dropped and created again is given
// the owner and the access entries the old one held. Dispatched is true as
// soon as a statement was sent. A failed statement or an unconfirmed read leaves the outcome
// uncertain; nothing is retried. YDB has no atomic schema compare-and-set, so
// another writer can change the object between the check and the statement.
func (e *External) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	normalized, err := schema.NormalizeExternalOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(normalized.NormalizedOwner)
	change, operation, err := schema.ExternalOperationChange(normalized.Value)
	if err != nil {
		return false, err
	}
	before, after, named, err := schema.ExternalOperands(change)
	if err != nil {
		return false, err
	}
	if (owner.Database != "" && owner.Database != database) || (named != "" && named != database) {
		return false, fmt.Errorf("external object operation targets another database")
	}
	owner.Database = database
	if err := schema.ValidateNativeOwner(owner); err != nil {
		return false, err
	}
	display := externals.Display(owner)
	current, err := e.read(ctx, owner)
	if err != nil {
		return false, err
	}
	absent := before == nil || operation == schema.ExternalRestore
	if absent != (current == nil) || (!absent && !sameObservation(before, current)) {
		return false, fmt.Errorf("%s %s changed since the plan read it", noun(owner.Kind), display)
	}
	statements, err := externalStatements(owner, database, operation, after)
	if err != nil {
		return false, err
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, statement := range statements {
		if err := e.Execute(ctx, statement); err != nil {
			return true, err
		}
	}
	observed, err := e.read(ctx, owner)
	if err != nil {
		return true, err
	}
	if operation == schema.ExternalRecreate || operation == schema.ExternalRestore {
		if observed, err = e.restoreAccess(ctx, owner, before, observed); err != nil {
			return true, err
		}
	}
	return true, confirmExternal(owner, database, operation, before, after, observed)
}

func noun(kind string) string {
	if kind == schema.ExternalSourceProperties {
		return "external data source"
	}
	return "external table"
}

// externalStatements are the statements that carry out operation on the
// object owner names, at its absolute path.
func externalStatements(owner *protocol.Reference, database, operation string, after *protocol.Envelope) ([]string, error) {
	absolute := path.Join(database, owner.Schema, owner.Name)
	source := owner.Kind == schema.ExternalSourceProperties
	drop := externals.DropTableStatement(absolute)
	if source {
		drop = externals.DropDataSourceStatement(absolute)
	}
	if operation == schema.ExternalDrop {
		return []string{drop}, nil
	}
	creation := externals.Create
	if operation == schema.ExternalReplace {
		creation = externals.Replace
	}
	var create string
	if source {
		_, spec, err := externals.SourcePair(nil, after, database)
		if err != nil {
			return nil, err
		}
		create = externals.CreateDataSourceStatement(absolute, *spec, creation)
	} else {
		_, spec, err := externals.TablePair(nil, after, database)
		if err != nil {
			return nil, err
		}
		// The data source is written as the declaration writes it: a
		// relative path names one of the database, as the host renders it.
		intent, err := schema.DesiredExternalTable(after)
		if err != nil {
			return nil, err
		}
		spec.DataSource = intent.GetDataSourcePath()
		create = externals.CreateTableStatement(absolute, *spec, creation)
	}
	if operation == schema.ExternalRecreate {
		return []string{drop, create}, nil
	}
	return []string{create}, nil
}

// restoreAccess gives the object created again, current as read after its
// creation, the owner and the access entries the one the plan read held:
// DROP takes them, CREATE gives the new one none, and the user who runs the
// plan owns it. It returns the object as read after.
func (e *External) restoreAccess(ctx context.Context, owner *protocol.Reference, before, current *protocol.Envelope) (*protocol.Envelope, error) {
	old, err := selfOf(before)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("%s result is absent", noun(owner.Kind))
	}
	now, err := selfOf(current)
	if err != nil {
		return nil, err
	}
	var actions []*Ydb_Scheme.PermissionsAction
	for _, entry := range old.GetPermissions() {
		actions = append(actions, &Ydb_Scheme.PermissionsAction{Action: &Ydb_Scheme.PermissionsAction_Grant{Grant: proto.CloneOf(entry)}})
	}
	if old.GetOwner() != "" && old.GetOwner() != now.GetOwner() {
		actions = append(actions, &Ydb_Scheme.PermissionsAction{Action: &Ydb_Scheme.PermissionsAction_ChangeOwner{ChangeOwner: old.GetOwner()}})
	}
	if len(actions) == 0 {
		return current, nil
	}
	response, err := e.Scheme.ModifyPermissions(ctx, &Ydb_Scheme.ModifyPermissionsRequest{Path: path.Join(owner.Database, owner.Schema, owner.Name), Actions: actions})
	if err != nil {
		return nil, fmt.Errorf("restore the access of YDB %s: transport failed", noun(owner.Kind))
	}
	if err := Status(response.GetOperation()); err != nil {
		return nil, err
	}
	return e.read(ctx, owner)
}

// read lists the object's directory and describes the object, or returns
// nil where the directory holds no entry of its name.
func (e *External) read(ctx context.Context, owner *protocol.Reference) (*protocol.Envelope, error) {
	directory := path.Join(owner.Database, owner.Schema)
	response, err := e.Scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: directory})
	if err != nil {
		return nil, fmt.Errorf("read YDB external object namespace: transport failed")
	}
	var listing Ydb_Scheme.ListDirectoryResult
	if err := Operation(response.GetOperation(), &listing); err != nil {
		return nil, err
	}
	if listing.GetSelf().GetName() != path.Base(directory) || (listing.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE && listing.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY) || len(listing.Children) > 4096 {
		return nil, fmt.Errorf("invalid YDB external object namespace")
	}
	expected := Ydb_Scheme.Entry_EXTERNAL_TABLE
	if owner.Kind == schema.ExternalSourceProperties {
		expected = Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE
	}
	found := false
	for _, entry := range listing.Children {
		if entry.GetName() != owner.Name {
			continue
		}
		if found || entry.GetType() != expected {
			return nil, fmt.Errorf("%s path has a different or ambiguous occupant", noun(owner.Kind))
		}
		found = true
	}
	if !found {
		return nil, nil
	}
	absolute := path.Join(directory, owner.Name)
	var described proto.Message
	var self *Ydb_Scheme.Entry
	if owner.Kind == schema.ExternalSourceProperties {
		response, err := e.Table.DescribeExternalDataSource(ctx, &Ydb_Table.DescribeExternalDataSourceRequest{Path: absolute})
		if err != nil {
			return nil, fmt.Errorf("describe YDB external data source: transport failed")
		}
		var result Ydb_Table.DescribeExternalDataSourceResult
		if err := Operation(response.GetOperation(), &result); err != nil {
			return nil, err
		}
		described, self = &result, result.GetSelf()
	} else {
		response, err := e.Table.DescribeExternalTable(ctx, &Ydb_Table.DescribeExternalTableRequest{Path: absolute})
		if err != nil {
			return nil, fmt.Errorf("describe YDB external table: transport failed")
		}
		if err := Status(response.GetOperation()); err != nil {
			return nil, err
		}
		payload := response.GetOperation().GetResult()
		if !payload.MessageIs(&Ydb_Table.DescribeExternalTableResult{}) {
			return nil, fmt.Errorf("invalid YDB external table response type")
		}
		var result native.DescribeExternalTableResult
		if err := proto.Unmarshal(payload.GetValue(), &result); err != nil {
			return nil, fmt.Errorf("invalid YDB external table response")
		}
		described, self = &result, result.GetSelf()
	}
	if self.GetName() != owner.Name || self.GetType() != expected {
		return nil, fmt.Errorf("YDB external object observation has a different identity")
	}
	value, err := schema.CaptureObject(owner, described)
	if err != nil {
		return nil, err
	}
	value.Context = &protocol.DatabaseContext{Database: owner.Database, SourceDatabase: owner.Database}
	return value, nil
}

// sameObservation reports whether the object read now is the one the plan
// read. The REFERENCES a data source lists are left out: the server keeps
// them, and an external table the same plan dropped or created changes them.
func sameObservation(planned, current *protocol.Envelope) bool {
	a, b := withoutReferences(planned), withoutReferences(current)
	return a != nil && b != nil && string(a) == string(b)
}

func withoutReferences(value *protocol.Envelope) []byte {
	if value.GetKind() != schema.ExternalSourceProperties {
		return value.GetPayload()
	}
	observed, err := schema.ObservedExternalSource(value)
	if err != nil {
		return nil
	}
	for name := range observed.Properties {
		if strings.EqualFold(name, "REFERENCES") {
			delete(observed.Properties, name)
		}
	}
	encoded, err := protojson.Marshal(observed)
	if err != nil {
		return nil
	}
	canonical, err := modeljson.Canonical(encoded)
	if err != nil {
		return nil
	}
	return canonical
}

// confirmExternal requires the object read back to be the one the
// operation asked for: absent after a drop, and otherwise holding the
// declaration, with the owner and the access entries the object held where
// CREATE OR REPLACE kept them or the operation gave them again.
func confirmExternal(owner *protocol.Reference, database, operation string, before, after, current *protocol.Envelope) error {
	if operation == schema.ExternalDrop {
		if current != nil {
			return fmt.Errorf("%s removal was not confirmed", noun(owner.Kind))
		}
		return nil
	}
	if current == nil {
		return fmt.Errorf("%s result is absent", noun(owner.Kind))
	}
	held, err := externals.Equal(current, after, database)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("%s result differs from the declaration", noun(owner.Kind))
	}
	if before == nil {
		return nil
	}
	old, err := selfOf(before)
	if err != nil {
		return err
	}
	now, err := selfOf(current)
	if err != nil {
		return err
	}
	if !samePermissions(old.GetPermissions(), now.GetPermissions()) || old.GetOwner() != now.GetOwner() {
		return fmt.Errorf("%s access entries changed during the operation", noun(owner.Kind))
	}
	return nil
}

func selfOf(value *protocol.Envelope) (*Ydb_Scheme.Entry, error) {
	if value.GetKind() == schema.ExternalSourceProperties {
		observed, err := schema.ObservedExternalSource(value)
		return observed.GetSelf(), err
	}
	observed, err := schema.ObservedExternalTable(value)
	return observed.GetSelf(), err
}

// samePermissions compares two lists of direct access entries as sets of a
// subject and a permission.
func samePermissions(a, b []*Ydb_Scheme.Permissions) bool {
	flatten := func(entries []*Ydb_Scheme.Permissions) []string {
		var result []string
		for _, entry := range entries {
			for _, name := range entry.GetPermissionNames() {
				result = append(result, entry.GetSubject()+"\x00"+name)
			}
		}
		slices.Sort(result)
		return slices.Compact(result)
	}
	return slices.Equal(flatten(a), flatten(b))
}
