package backend

import (
	"context"
	"errors"
	"net/url"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/embedstore"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consistencyModes are the modes a YDB generation can be built with. The
// outbox mode records each source change with a row trigger in the
// transaction that makes it, and YDB has no triggers, so a generation is
// immutable, as the host's embedded engine builds one.
var consistencyModes = []string{"immutable"}

// embeddingStore returns the embedding store of a session, made on the
// session's first embedding call: the server's line, with the database's
// feature flags where the address names the monitoring endpoint, decides
// what the store does with vectors. The session lock is held only to find
// the store, so a long call such as an index build does not hold up the
// session's other calls, and no embedding call joins the session's
// transaction.
func (p *Provider) embeddingStore(ctx context.Context, id string) (*embedstore.Store, error) {
	s, err := p.lookup(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.FailedPrecondition, "YDB session is closed")
	}
	if s.embeddings != nil {
		return s.embeddings, nil
	}
	var flags featureflags.Flags
	if s.connection.HasMonitoring() {
		body, err := s.connection.ReadMonitoring(ctx, "feature flags", featureflags.Path, url.Values{"database": {s.connection.Database()}})
		if err != nil {
			return nil, embeddingError(ctx, err)
		}
		if flags, err = featureflags.Decode(body, s.connection.Database()); err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
	}
	s.embeddings = embedstore.New(s.connection.DB, s.connection.Driver, s.database, embedstore.LineCapabilities(s.version, flags))
	return s.embeddings, nil
}

// embeddingDescription is the description as the host of the call negotiated
// it, which lists the store only for a host that lists it too.
func embeddingDescription(ctx context.Context) *protocol.Description {
	return provider.NegotiatedDescription(Description(), provider.HostFeatures(ctx))
}

// embeddingSession validates a session request and returns its store.
func (p *Provider) embeddingSession(ctx context.Context, id string, validate error) (*embedstore.Store, error) {
	if validate != nil {
		return nil, status.Error(codes.InvalidArgument, validate.Error())
	}
	if !slices.Contains(embeddingDescription(ctx).GetFeatures(), provider.EmbeddingStoreFeature) {
		return nil, status.Error(codes.FailedPrecondition, "the host did not list "+provider.EmbeddingStoreFeature)
	}
	return p.embeddingStore(ctx, id)
}

// embeddingError is the status of a read the store could not answer: the
// caller's cancellation, a refusal of what the request names, or an engine
// that did not answer.
func embeddingError(ctx context.Context, err error) error {
	var refusal *embedstore.Refusal
	switch {
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	case errors.As(err, &refusal):
		return status.Error(codes.FailedPrecondition, refusal.Message)
	case errors.Is(err, embedstore.ErrStoreAbsent):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Unavailable, err.Error())
	}
}

// PrepareEmbeddingStore creates the run-state tables that are missing and
// verifies the others; a layout the provider does not recognize is REFUSED
// with NOT_OWNED.
func (p *Provider) PrepareEmbeddingStore(ctx context.Context, request *protocol.PrepareEmbeddingStoreRequest) (*protocol.EmbeddingWriteResult, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidatePrepareEmbeddingStoreRequest(request))
	if err != nil {
		return nil, err
	}
	return store.PrepareStore(ctx, request.GetRequestId()), nil
}

// ReadEmbeddingState reads one scope as one snapshot with its token.
func (p *Provider) ReadEmbeddingState(ctx context.Context, request *protocol.ReadEmbeddingStateRequest) (*protocol.EmbeddingState, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateReadEmbeddingStateRequest(request))
	if err != nil {
		return nil, err
	}
	state, err := store.ReadState(ctx, request.GetScope(), request.GetMaxRecords())
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return state, nil
}

// MutateEmbeddingState writes the records of one decision when the scope's
// token is the one it was read with.
func (p *Provider) MutateEmbeddingState(ctx context.Context, request *protocol.MutateEmbeddingStateRequest) (*protocol.EmbeddingWriteResult, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateMutateEmbeddingStateRequest(request))
	if err != nil {
		return nil, err
	}
	return store.MutateState(ctx, request), nil
}

// AppendEmbeddingEvent records an event of an existing run at its next
// sequence.
func (p *Provider) AppendEmbeddingEvent(ctx context.Context, request *protocol.AppendEmbeddingEventRequest) (*protocol.EmbeddingWriteResult, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateAppendEmbeddingEventRequest(request))
	if err != nil {
		return nil, err
	}
	return store.AppendEvent(ctx, request.GetRequestId(), request.GetEvent()), nil
}

// ReadEmbeddingEvents pages a run's events in sequence order.
func (p *Provider) ReadEmbeddingEvents(ctx context.Context, request *protocol.ReadEmbeddingEventsRequest) (*protocol.EmbeddingEventPage, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateReadEmbeddingEventsRequest(request))
	if err != nil {
		return nil, err
	}
	page, err := store.ReadEvents(ctx, request)
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return page, nil
}

// DescribeEmbeddingTarget answers offline what a generation needs on YDB, or
// why YDB cannot hold the specification.
func (p *Provider) DescribeEmbeddingTarget(ctx context.Context, request *protocol.DescribeEmbeddingTargetRequest) (*protocol.EmbeddingTargetObjects, error) {
	if err := provider.ValidateDescribeEmbeddingTargetRequest(embeddingDescription(ctx), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return embedstore.DescribeTarget(request.GetSpec()), nil
}

// ResolveEmbeddingRelations answers each relation as YDB names it.
func (p *Provider) ResolveEmbeddingRelations(ctx context.Context, request *protocol.ResolveEmbeddingRelationsRequest) (*protocol.EmbeddingRelations, error) {
	if _, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateResolveEmbeddingRelationsRequest(request)); err != nil {
		return nil, err
	}
	return &protocol.EmbeddingRelations{Relations: embedstore.ResolveRelations(request.GetRelations())}, nil
}

// MeasureEmbedding answers what a plan reports about the database.
func (p *Provider) MeasureEmbedding(ctx context.Context, request *protocol.MeasureEmbeddingRequest) (*protocol.EmbeddingMeasurement, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateMeasureEmbeddingRequest(request))
	if err != nil {
		return nil, err
	}
	measured, err := store.Measure(ctx, request.GetSpec())
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return measured, nil
}

// PrepareEmbeddingTarget creates a generation's storage or verifies it.
func (p *Provider) PrepareEmbeddingTarget(ctx context.Context, request *protocol.PrepareEmbeddingTargetRequest) (*protocol.EmbeddingWriteResult, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidatePrepareEmbeddingTargetRequest(request))
	if err != nil {
		return nil, err
	}
	return store.PrepareTarget(ctx, request), nil
}

// BuildEmbeddingIndex builds the declared index, or rebuilds one whose build
// did not finish.
func (p *Provider) BuildEmbeddingIndex(ctx context.Context, request *protocol.BuildEmbeddingIndexRequest) (*protocol.EmbeddingWriteResult, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateBuildEmbeddingIndexRequest(request))
	if err != nil {
		return nil, err
	}
	return store.BuildIndex(ctx, request), nil
}

// ReadEmbeddingStructure reads a registered generation's storage.
func (p *Provider) ReadEmbeddingStructure(ctx context.Context, request *protocol.ReadEmbeddingStructureRequest) (*protocol.EmbeddingStructure, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateReadEmbeddingStructureRequest(request))
	if err != nil {
		return nil, err
	}
	structure, err := store.ReadStructure(ctx, request)
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return structure, nil
}

// DropEmbeddingGeneration drops a retired generation's storage once its
// ownership is proven.
func (p *Provider) DropEmbeddingGeneration(ctx context.Context, request *protocol.DropEmbeddingGenerationRequest) (*protocol.EmbeddingWriteResult, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateDropEmbeddingGenerationRequest(request))
	if err != nil {
		return nil, err
	}
	return store.DropGeneration(ctx, request), nil
}

// ScanEmbeddingSource reads one keyset page of a source.
func (p *Provider) ScanEmbeddingSource(ctx context.Context, request *protocol.ScanEmbeddingSourceRequest) (*protocol.EmbeddingSourcePage, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateScanEmbeddingSourceRequest(request))
	if err != nil {
		return nil, err
	}
	page, err := store.ScanSource(ctx, request)
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return page, nil
}

// ReadEmbeddingSourceRows rereads source rows by key.
func (p *Provider) ReadEmbeddingSourceRows(ctx context.Context, request *protocol.ReadEmbeddingSourceRowsRequest) (*protocol.EmbeddingSourceRows, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateReadEmbeddingSourceRowsRequest(request))
	if err != nil {
		return nil, err
	}
	rows, err := store.ReadSourceRows(ctx, request)
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return rows, nil
}

// ReadEmbeddingBatchState reads the run and the held state of a batch's keys.
func (p *Provider) ReadEmbeddingBatchState(ctx context.Context, request *protocol.ReadEmbeddingBatchStateRequest) (*protocol.EmbeddingBatchState, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateReadEmbeddingBatchStateRequest(request))
	if err != nil {
		return nil, err
	}
	state, err := store.ReadBatchState(ctx, request)
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return state, nil
}

// CommitEmbeddingBatch writes a batch's sets and the checkpointed run in one
// transaction when the batch state's token still holds.
func (p *Provider) CommitEmbeddingBatch(ctx context.Context, request *protocol.CommitEmbeddingBatchRequest) (*protocol.EmbeddingWriteResult, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateCommitEmbeddingBatchRequest(request))
	if err != nil {
		return nil, err
	}
	return store.CommitBatch(ctx, request), nil
}

// ReadEmbeddingVerification pages the walk verification folds over.
func (p *Provider) ReadEmbeddingVerification(ctx context.Context, request *protocol.ReadEmbeddingVerificationRequest) (*protocol.EmbeddingVerificationPage, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateReadEmbeddingVerificationRequest(request))
	if err != nil {
		return nil, err
	}
	page, err := store.ReadVerification(ctx, request)
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return page, nil
}

// SearchEmbeddings answers the nearest keys through the index and by scan.
func (p *Provider) SearchEmbeddings(ctx context.Context, request *protocol.SearchEmbeddingsRequest) (*protocol.EmbeddingSearchResult, error) {
	store, err := p.embeddingSession(ctx, request.GetSessionId(), provider.ValidateSearchEmbeddingsRequest(request))
	if err != nil {
		return nil, err
	}
	result, err := store.Search(ctx, request)
	if err != nil {
		return nil, embeddingError(ctx, err)
	}
	return result, nil
}
