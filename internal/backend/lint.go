package backend

import (
	"context"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/migrationlint"
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
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	answer := migrationlint.SplitRequest
	if slices.Contains(description.GetEngines()[0].GetFeatures(), provider.LintStatementFactsFeature) {
		answer = migrationlint.DescribedSplitRequest
	}
	result, err := answer(ctx, request)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	if err := provider.ValidateSplitLintReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
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
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result, err := migrationlint.AnnotateRequest(ctx, request)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	if err := provider.ValidateAnnotatedLintReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
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
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result, err := migrationlint.CheckRequest(ctx, request)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	if err := provider.ValidateCheckedLintReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	return result, nil
}

// lintDescription is the description as the host of the call negotiated it,
// which lists the statement facts only for a host that lists them too.
func lintDescription(ctx context.Context) *protocol.Description {
	return provider.NegotiatedDescription(described(), provider.HostFeatures(ctx))
}

// lintError is the status of a lint call that failed: the caller's
// cancellation, or a request the rules could not answer.
func lintError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return authored.Status(codes.InvalidArgument, err)
}
