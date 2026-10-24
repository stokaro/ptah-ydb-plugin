package backend

import (
	"context"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SplitLintStatements splits YDB migrations as YQL offline and reports each
// statement's effects (see internal/migrationlint). For a host that lists
// lint.statement-facts, every statement also says what it is, which table it
// creates without a primary key and which capabilities it needs. An unusable
// request, cancellation or a reply the SDK would not accept returns an error
// and no statement.
func (*Provider) SplitLintStatements(ctx context.Context, request *protocol.SplitLintRequest) (*protocol.SplitLint, error) {
	description := lintDescription(ctx)
	if err := provider.ValidateSplitLintRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	describe := slices.Contains(description.GetEngines()[0].GetFeatures(), provider.LintStatementFactsFeature)
	result, err := migrationlint.SplitRequest(ctx, request, describe)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	if err := provider.ValidateSplitLint(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

// AnnotateLintStatements judges the statements a host split without a
// dialect, declining the ones YQL would split differently. A request may ask
// for the statement facts only where the host lists lint.statement-facts. An
// unusable request, cancellation or a reply the SDK would not accept returns
// an error and no annotation.
func (*Provider) AnnotateLintStatements(ctx context.Context, request *protocol.AnnotateLintRequest) (*protocol.AnnotatedLint, error) {
	description := lintDescription(ctx)
	if err := provider.ValidateAnnotateLintRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := migrationlint.AnnotateRequest(ctx, request)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	if err := provider.ValidateAnnotatedLint(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

// CheckLintMigrations judges every file of a run under the YD rules the
// request enables. The reply is complete or the call fails: an unusable
// request, cancellation or a reply the SDK would not accept returns an error
// and no finding.
func (*Provider) CheckLintMigrations(ctx context.Context, request *protocol.CheckLintRequest) (*protocol.CheckedLint, error) {
	description := lintDescription(ctx)
	if err := provider.ValidateCheckLintRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := migrationlint.CheckRequest(ctx, request)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	if err := provider.ValidateCheckedLint(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

// lintDescription is the description as the host of the call negotiated it,
// which lists the statement facts only for a host that lists them too.
func lintDescription(ctx context.Context) *protocol.Description {
	return provider.NegotiatedDescription(Description(), provider.HostFeatures(ctx))
}

// lintError is the status of a lint call that failed: the caller's
// cancellation, or a request the rules could not answer.
func lintError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return status.Error(codes.InvalidArgument, err.Error())
}
