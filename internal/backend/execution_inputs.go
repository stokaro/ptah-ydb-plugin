package backend

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretvalue"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

var _ provider.ExecutionInputDescriber = (*Provider)(nil)

// DescribeExecutionInputs validates the whole batch and reports only input
// names. It performs no I/O and never reads the provider process environment.
func (*Provider) DescribeExecutionInputs(ctx context.Context, request *protocol.DescribeExecutionInputsRequest) (*protocol.ExecutionInputs, error) {
	if err := provider.ValidateExecutionInputsRequest(Description(), request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	inputs, err := requiredInputs(ctx, request.Units)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result := &protocol.ExecutionInputs{Complete: true, Inputs: inputs}
	if err := provider.ValidateExecutionInputsReply(Description(), request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	return result, nil
}

func requiredInputs(ctx context.Context, units []*protocol.Unit) ([]*protocol.ExecutionInput, error) {
	names := make(map[string]bool)
	for _, unit := range units {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := provider.ValidateUnit(unit); err != nil {
			return nil, err
		}
		query := unit.GetSql()
		if native := unit.GetNative(); native != nil {
			if _, err := normalizeOperation(&protocol.ValueInput{Id: unit.ChangeId, Representation: protocol.ValueInput_OPERATION, Value: native}); err != nil {
				return nil, err
			}
			if native.Kind != schema.SecretOperation {
				continue
			}
			var err error
			query, err = schema.SecretStatement(native)
			if err != nil {
				return nil, err
			}
		}
		// Validate positions using an empty dummy value. No actual input is read.
		if _, err := secretvalue.Expand(query, func(string) (string, bool) { return "", true }); err != nil {
			return nil, err
		}
		for _, name := range secretvalue.References(query) {
			names[name] = true
		}
	}
	var result []*protocol.ExecutionInput
	for _, name := range slices.Sorted(maps.Keys(names)) {
		result = append(result, &protocol.ExecutionInput{Source: protocol.ExecutionInput_ENVIRONMENT, Name: name})
	}
	return result, ctx.Err()
}

// executionBindings rediscover references before any database write and accept
// exactly one ephemeral binding per approved input, including empty values.
func executionBindings(ctx context.Context, request *protocol.ExecuteRequest) (map[string]string, error) {
	actual, err := requiredInputs(ctx, request.Units)
	if err != nil {
		return nil, err
	}
	if err := provider.CheckExecutionInputs(request.ExpectedInputs, actual); err != nil {
		return nil, err
	}
	if len(actual)+len(request.InputBindings) != 0 && (request.Engine != "ydb" || !slices.Contains(provider.HostFeatures(ctx), provider.SensitiveInputsFeature)) {
		return nil, fmt.Errorf("secret inputs require the negotiated sensitive-input service")
	}
	names := make(map[string]bool)
	for _, input := range actual {
		names[input.Name] = true
	}
	values := make(map[string]string)
	for _, binding := range request.InputBindings {
		input := binding.GetInput()
		if input.GetSource() != protocol.ExecutionInput_ENVIRONMENT || !names[input.GetName()] {
			return nil, fmt.Errorf("execution bindings differ from approved inputs")
		}
		if _, exists := values[input.Name]; exists {
			return nil, fmt.Errorf("execution input is bound more than once")
		}
		values[input.Name] = string(binding.Value)
	}
	if len(values) != len(names) {
		return nil, fmt.Errorf("an approved execution input has no binding")
	}
	return values, ctx.Err()
}

// definesSecret reports whether query defines a secret's value, which only a
// target with schema secrets takes. A query that names a user's password
// alone needs no such target. Discovery has already checked every position,
// so a query Expand refuses here defines nothing to gate on.
func definesSecret(query string) bool {
	expansion, err := secretvalue.Expand(query, func(string) (string, bool) { return "", true })
	return err == nil && expansion.DefinesSecrets()
}

func expandInputs(query string, values map[string]string) (secretvalue.Expansion, error) {
	return secretvalue.Expand(query, func(name string) (string, bool) { value, found := values[name]; return value, found })
}

func inputError(expansion secretvalue.Expansion, err error) error {
	if err == nil || !expansion.Defines() {
		return err
	}
	// Do not retain an error chain whose message could expose expanded SQL.
	// What YDB answered, its status and its issues, is kept with the bound
	// values taken out of each issue, so the unit's outcome is still read
	// from it: a refused CREATE USER stays NOT_APPLIED rather than UNKNOWN.
	text := expansion.Redact(err.Error())
	answered, ok := errors.AsType[ydbAnswer](err)
	if !ok {
		return errors.New(text)
	}
	return redactedAnswer{text: text, code: answered.Code(), issues: redactIssues(expansion, answered.Issues())}
}

// ydbAnswer is the SDK's error for an operation status YDB returned, as
// ydbfailure reads it.
type ydbAnswer interface {
	error
	Code() int32
	Issues() []*Ydb_Issue.IssueMessage
}

// redactedAnswer is what YDB answered a statement that held bound values with,
// those values taken out of its text and its issues, and nothing else of the
// error chain.
type redactedAnswer struct {
	text   string
	code   int32
	issues []*Ydb_Issue.IssueMessage
}

func (e redactedAnswer) Error() string                     { return e.text }
func (e redactedAnswer) Code() int32                       { return e.code }
func (e redactedAnswer) Issues() []*Ydb_Issue.IssueMessage { return e.issues }

// redactIssues copies issues with the expansion's values taken out of each
// message, at every depth.
func redactIssues(expansion secretvalue.Expansion, issues []*Ydb_Issue.IssueMessage) []*Ydb_Issue.IssueMessage {
	redacted := make([]*Ydb_Issue.IssueMessage, 0, len(issues))
	for _, issue := range issues {
		copied := proto.CloneOf(issue)
		copied.Message = expansion.Redact(copied.GetMessage())
		copied.Issues = redactIssues(expansion, issue.GetIssues())
		redacted = append(redacted, copied)
	}
	return redacted
}
