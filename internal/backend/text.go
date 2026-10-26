package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// renderText writes every change of a request in the TEXT form as one YQL
// statement, the one the host's embedded engine writes for the same change,
// which this provider's Execute runs read back from a migration file. A
// change only a native call can make refuses the whole batch with
// NO_TEXT_FORM and no unit. operations are the changes' normalized
// operations, in order, and nil for a common object, whose statement is in
// statements.
func renderText(ctx context.Context, changes []*protocol.Change, operations []*protocol.Envelope, statements []string) (*protocol.RenderResult, error) {
	result := &protocol.RenderResult{Complete: true}
	for i, change := range changes {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		if operations[i] == nil {
			result.Units = append(result.Units, &protocol.Unit{ChangeId: change.GetId(), Body: &protocol.Unit_Sql{Sql: statements[i]}})
			continue
		}
		statement, err := textOf(operations[i])
		if errors.Is(err, tablecreate.ErrNoTextForm) {
			return &protocol.RenderResult{Refusal: &protocol.RenderRefusal{ChangeId: change.GetId(), Reason: protocol.RenderRefusal_NO_TEXT_FORM,
				Message: rowdata.OneLine(err.Error())}}, nil
		}
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		result.Units = append(result.Units, &protocol.Unit{ChangeId: change.GetId(), Body: &protocol.Unit_Sql{Sql: statement}})
	}
	return result, nil
}

// textOf writes one native operation as its YQL statement, or an error
// wrapping [tablecreate.ErrNoTextForm] for an operation YQL cannot spell.
func textOf(value *protocol.Envelope) (string, error) {
	owner := value.GetOwner()
	path := tablecreate.TextPath(owner.GetSchema(), owner.GetName())
	switch value.GetKind() {
	case tablecreate.OperationKind:
		request, err := tablecreate.Request(value)
		if err != nil {
			return "", err
		}
		return tablecreate.RowTableStatement(path, request)
	case tablecreate.AlterOperationKind:
		alteration, err := tablecreate.DecodeAlteration(value)
		if err != nil {
			return "", err
		}
		return tablecreate.AlterationStatement(path, alteration)
	case tablecreate.ColumnOperationKind:
		creation, err := tablecreate.ColumnTable(value)
		if err != nil {
			return "", err
		}
		return tablecreate.ColumnTableStatement(path, creation), nil
	case tablecreate.ColumnTTLOperationKind:
		alteration, err := tablecreate.ColumnTTLAlteration(value)
		if err != nil {
			return "", err
		}
		return tablecreate.ColumnTTLStatement(path, alteration), nil
	case tablecreate.DropOperationKind:
		if _, err := tablecreate.DecodeDrop(value); err != nil {
			return "", err
		}
		return tablecreate.DropStatement(path), nil
	case tablecreate.CopyOperationKind:
		copied, err := tablecreate.DecodeCopy(value)
		if err != nil {
			return "", err
		}
		return tablecreate.CopyStatement(path, copied)
	case tablecreate.RenameOperationKind:
		rename, err := tablecreate.DecodeRename(value)
		if err != nil {
			return "", err
		}
		return tablecreate.RenameStatement(path, rename), nil
	case schema.SecretOperation:
		return schema.SecretStatement(value)
	case schema.CoordinationOperation:
		return coordinationText(path, value)
	case schema.ExternalSourceOperation, schema.ExternalTableOperation:
		return externalText(path, value)
	case schema.StreamingQueryOperation:
		change, err := schema.StreamingChangeOf(schema.StreamingQueryOperationChange(value))
		if err != nil {
			return "", err
		}
		statements, err := streaming.Statements(change)
		if err != nil {
			return "", err
		}
		if len(statements) != 1 {
			return "", fmt.Errorf("%w: a running streaming query alteration needs separate stop and restart operations", tablecreate.ErrNoTextForm)
		}
		return statements[0], nil
	case schema.WorkloadOperation:
		return workloadText(value)
	default:
		return "", fmt.Errorf("%w: the native operation %s has no YQL statement", tablecreate.ErrNoTextForm, value.GetKind())
	}
}

// coordinationText writes a coordination node operation as the statement
// the host's embedded engine writes for it: a CREATE with the settings the
// declaration states, an ALTER with the settings that change, or a DROP.
func coordinationText(path string, value *protocol.Envelope) (string, error) {
	before, after, err := schema.CoordinationOperands(schema.CoordinationOperationChange(value))
	if err != nil {
		return "", err
	}
	statement := coordinationsql.Statement{Path: path}
	var desired schema.CoordinationIntent
	if after != nil {
		if desired, err = schema.CoordinationConfiguration(after, protocol.ValueInput_DESIRED); err != nil {
			return "", err
		}
		if desired.Access != nil {
			return "", fmt.Errorf("%w: a coordination node that declares its access is checked by a native call", tablecreate.ErrNoTextForm)
		}
	}
	switch {
	case before == nil:
		statement.Verb, statement.Settings = coordinationsql.Create, nodeSettings(desired)
	case after == nil:
		statement.Verb = coordinationsql.Drop
	default:
		current, err := schema.CoordinationConfiguration(before, protocol.ValueInput_OBSERVED)
		if err != nil {
			return "", err
		}
		statement.Verb, statement.Settings = coordinationsql.Alter, coordinationsql.Changes(nodeSettings(desired), nodeSettings(current))
	}
	return statement.Text()
}

// externalText writes an external data source or external table operation
// as the one statement the host's embedded engine writes for it: a CREATE, a
// CREATE OR REPLACE or a DROP, with the paths and the options as the
// declaration writes them. An object dropped and created again around the
// statements of one plan has no text form: the native call gives the new
// object the owner and the access entries the old one held, which no single
// statement does.
func externalText(path string, value *protocol.Envelope) (string, error) {
	change, operation, err := schema.ExternalOperationChange(value)
	if err != nil {
		return "", err
	}
	source := value.GetKind() == schema.ExternalSourceOperation
	switch operation {
	case schema.ExternalDrop:
		if source {
			return externals.DropDataSourceStatement(path), nil
		}
		return externals.DropTableStatement(path), nil
	case schema.ExternalRecreate, schema.ExternalRestore:
		return "", fmt.Errorf("%w: an external object dropped and created again is given the owner and the access entries of the old one by a native call", tablecreate.ErrNoTextForm)
	}
	creation := externals.Create
	if operation == schema.ExternalReplace {
		creation = externals.Replace
	}
	_, after, _, err := schema.ExternalOperands(change)
	if err != nil {
		return "", err
	}
	// An empty root keeps every path as the declaration writes it.
	if source {
		_, spec, err := externals.SourcePair(nil, after, "")
		if err != nil {
			return "", err
		}
		return externals.CreateDataSourceStatement(path, *spec, creation), nil
	}
	_, spec, err := externals.TablePair(nil, after, "")
	if err != nil {
		return "", err
	}
	return externals.CreateTableStatement(path, *spec, creation), nil
}

// nodeSettings is a coordination intent as the settings a statement names.
func nodeSettings(intent schema.CoordinationIntent) ydbdecl.CoordinationNode {
	var settings ydbdecl.CoordinationNode
	if intent.SelfCheckPeriodMillis != nil {
		settings.SelfCheckPeriodMillis = *intent.SelfCheckPeriodMillis
	}
	if intent.SessionGracePeriodMillis != nil {
		settings.SessionGracePeriodMillis = *intent.SessionGracePeriodMillis
	}
	if intent.ReadConsistencyMode != nil {
		settings.ReadConsistencyMode = *intent.ReadConsistencyMode
	}
	if intent.AttachConsistencyMode != nil {
		settings.AttachConsistencyMode = *intent.AttachConsistencyMode
	}
	if intent.RateLimiterCountersMode != nil {
		settings.RateLimiterCountersMode = *intent.RateLimiterCountersMode
	}
	return settings
}

// workloadText writes a workload operation as the statements the host's
// embedded engine writes for it, in the order they run. One query takes them
// all: measured on 25.1.4.7 and 26.2.1.14, a query that drops a classifier,
// moves another into its rank and creates the first again leaves both as
// declared.
func workloadText(value *protocol.Envelope) (string, error) {
	changes, err := schema.DecodeWorkloadOperation(value)
	if err != nil {
		return "", err
	}
	statements, err := workload.Statements(changes.Pools, changes.Classifiers)
	if err != nil {
		return "", err
	}
	return strings.Join(statements, "\n"), nil
}
