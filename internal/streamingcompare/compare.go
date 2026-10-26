// Package streamingcompare compares captured streaming query declarations.
package streamingcompare

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// ServiceID identifies streaming query comparison.
const ServiceID = "ydb-streaming-queries"

// Compare requires complete stored settings and preserves omitted declarations
// where the desired source does not establish absence.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, objectcompare.Family{
		ServiceID: ServiceID, Kind: schema.StreamingQueryProperties, Capability: streaming.Capability, Noun: "streaming query",
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(schema.StreamingQueryProperties) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.StreamingQueryProperties, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize, RequiredObjects: schema.RequiredObjects,
		Reserved:  func(*protocol.ComparisonIdentity) string { return "" },
		Unsettled: func(*protocol.Envelope, *protocol.Envelope) error { return nil },
		Equal: func(current, desired *protocol.Envelope) (bool, error) {
			before, err := schema.ObservedStreamingQuery(current)
			if err != nil {
				return false, err
			}
			after, _, err := schema.DesiredStreamingQuery(desired)
			return err == nil && streaming.Equal(before, after), err
		},
		Change: schema.CaptureStreamingQueryChange,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			spec, err := schema.ObservedStreamingQuery(current)
			if err != nil {
				return nil, err
			}
			return schema.CaptureStreamingQueryIntent(owner, spec, false)
		},
	})
}
