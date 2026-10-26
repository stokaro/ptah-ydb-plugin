package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
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
// NO_TEXT_FORM and no unit.
// operations are the changes' normalized operations, in order.
func renderText(ctx context.Context, changes []*protocol.Change, operations []*protocol.Envelope) (*protocol.RenderResult, error) {
	result := &protocol.RenderResult{Complete: true}
	for i, change := range changes {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
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
	case schema.CoordinationOperation:
		return coordinationText(path, value)
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
