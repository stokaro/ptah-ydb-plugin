package backend

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/secretvalue"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ provider.ExecutionInputDescriber = (*Provider)(nil)

// DescribeExecutionInputs validates the whole batch and reports only input
// names. It performs no I/O and never reads the provider process environment.
func (*Provider) DescribeExecutionInputs(ctx context.Context, request *protocol.DescribeExecutionInputsRequest) (*protocol.ExecutionInputs, error) {
	if err := provider.ValidateExecutionInputsRequest(Description(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	inputs, err := requiredInputs(ctx, request.Units)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result := &protocol.ExecutionInputs{Complete: true, Inputs: inputs}
	if err := provider.ValidateExecutionInputs(Description(), request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
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

func expandInputs(query string, values map[string]string) (secretvalue.Expansion, error) {
	return secretvalue.Expand(query, func(name string) (string, bool) { value, found := values[name]; return value, found })
}

func inputError(expansion secretvalue.Expansion, err error) error {
	if err == nil || !expansion.Defines() {
		return err
	}
	// Do not retain an error chain whose message could expose expanded SQL.
	return fmt.Errorf("%s", expansion.Redact(err.Error()))
}
