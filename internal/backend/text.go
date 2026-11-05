package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
			return nil, authored.Status(codes.InvalidArgument, err)
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
	case schema.ChangefeedOperation:
		return changefeedText(path, value)
	case schema.SecretOperation:
		return schema.SecretStatement(value)
	case schema.CoordinationOperation:
		return coordinationText(path, value)
	case schema.ExternalSourceOperation, schema.ExternalTableOperation:
		return externalText(path, value)
	case schema.StreamingQueryOperation:
		return streamingText(value)
	case schema.WorkloadOperation:
		return workloadText(value)
	case schema.ReplicationOperation:
		step, err := schema.ReplicationStepOf(schema.ReplicationOperationChange(value))
		if err != nil {
			return "", err
		}
		return replicationText(path, step), nil
	case schema.TransferOperation:
		step, err := schema.TransferStepOf(schema.TransferOperationChange(value))
		if err != nil {
			return "", err
		}
		return transferText(path, step), nil
	default:
		return rowTableText(value, owner.GetDatabase(), path)
	}
}

// rowTableText writes the creation or an alteration of a row table as its YQL
// statement in database, at path, and leaves every other operation to
// tableStatementText.
func rowTableText(value *protocol.Envelope, database, path string) (string, error) {
	switch value.GetKind() {
	case tablecreate.OperationKind:
		request, err := tablecreate.Request(value)
		if err != nil {
			return "", err
		}
		return tablecreate.RowTableStatementIn(database, path, request)
	case tablecreate.InventoryOperationKind:
		change, err := tablecreate.DecodeInventory(value)
		if err != nil {
			return "", err
		}
		return tablecreate.InventoryStatement(path, change)
	case tablecreate.DefaultOperationKind:
		change, err := tablecreate.DecodeDefault(value)
		if err != nil {
			return "", err
		}
		return tablecreate.DefaultStatement(path, change)
	case tablecreate.AlterOperationKind:
		alteration, err := tablecreate.DecodeAlteration(value)
		if err != nil {
			return "", err
		}
		return tablecreate.AlterationStatement(path, alteration)
	case tablecreate.IndexSettingsOperationKind:
		settings, err := tablecreate.DecodeIndexSettings(value)
		if err != nil {
			return "", err
		}
		return tablecreate.IndexSettingsStatement(path, settings), nil
	case tablecreate.NativeIndexOperationKind:
		addition, err := tablecreate.DecodeNativeIndex(value)
		if err != nil {
			return "", err
		}
		return tablecreate.NativeIndexStatement(path, addition), nil
	case tablecreate.SequenceOperationKind:
		change, err := tablecreate.DecodeSequence(value)
		if err != nil {
			return "", err
		}
		return tablecreate.SequenceAlterationStatement(database, path, change)
	default:
		return tableStatementText(value, path)
	}
}

// tableStatementText writes the creation or an alteration of a column table,
// and the drop, copy or rename of any table, as its YQL statement at path, or
// an error wrapping [tablecreate.ErrNoTextForm] for an operation YQL cannot
// spell.
func tableStatementText(value *protocol.Envelope, path string) (string, error) {
	switch value.GetKind() {
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
	case tablecreate.ColumnLocalIndexOperationKind:
		alteration, err := tablecreate.ColumnLocalIndexAlteration(value)
		if err != nil {
			return "", err
		}
		return tablecreate.ColumnLocalIndexStatement(path, alteration), nil
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
		return tablecreate.CopyStatement(path, value.GetOwner().GetSchema(), copied)
	case tablecreate.RenameOperationKind:
		rename, err := tablecreate.DecodeRename(value)
		if err != nil {
			return "", err
		}
		return tablecreate.RenameStatement(path, rename), nil
	default:
		return "", fmt.Errorf("%w: the native operation %s has no YQL statement", tablecreate.ErrNoTextForm, value.GetKind())
	}
}

// streamingText writes a streaming query operation as its one YQL statement.
// A running query's alteration needs a stop and a restart, which no single
// statement spells.
func streamingText(value *protocol.Envelope) (string, error) {
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
}

// changefeedText writes the changefeed changes of the table at path as the
// statements the host's embedded engine writes into a migration file for
// them, one to a line: every removal, then every addition with the consumers
// of its topic, then every change of a topic in place (see
// [changefeeds.Statements]). A migration runner splits the unit into the
// statements and runs each alone, so an ALTER TOPIC finds the topic the ADD
// CHANGEFEED above it created.
func changefeedText(path string, value *protocol.Envelope) (string, error) {
	changes, err := schema.DecodeChangefeedOperation(value)
	if err != nil {
		return "", err
	}
	statements, err := changefeeds.Statements(path, changes)
	if err != nil {
		return "", err
	}
	return strings.Join(statements, "\n"), nil
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
	parts, err := schema.ExternalOperands(change)
	if err != nil {
		return "", err
	}
	after := parts.After
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

// replicationText writes an async replication operation as the statement
// the host's embedded engine writes for it, every path relative to the
// database root as declared.
func replicationText(path string, step schema.ReplicationStep) string {
	switch {
	case step.Before == nil:
		return replication.CreateReplicationStatement(path, *step.After)
	case step.After == nil:
		return replication.DropReplicationStatement(path, step.Cascade())
	default:
		return replication.AlterReplicationStatement(path, *step.After, *step.Before)
	}
}

// transferText writes a transfer operation as the statement the host's
// embedded engine writes for it.
func transferText(path string, step schema.TransferStep) string {
	switch {
	case step.Before == nil:
		return replication.CreateTransferStatement(path, *step.After)
	case step.After == nil:
		return replication.DropTransferStatement(path)
	default:
		return replication.AlterTransferStatement(path, *step.After, *step.Before)
	}
}
