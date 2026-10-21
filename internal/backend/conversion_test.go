package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func columnConversion(c *qt.C, columns ...*native.ColumnMeta) (*backend.Provider, *protocol.ConvertValuesRequest) {
	c.Helper()
	p := backend.New()
	request := &protocol.NormalizeValuesRequest{Engine: "ydb"}
	for i, column := range columns {
		owner := &protocol.Reference{Engine: "ydb", Kind: "column", Database: "/local", Schema: "App", Path: []string{"Orders"}, Name: string(rune('a' + i))}
		value, err := schema.CaptureColumn(owner, column)
		c.Assert(err, qt.IsNil)
		request.Values = append(request.Values, &protocol.ValueInput{Id: owner.Name, Representation: protocol.ValueInput_OBSERVED, Value: value})
	}
	values, err := p.NormalizeValues(context.Background(), request)
	c.Assert(err, qt.IsNil)
	return p, &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_OBSERVED, To: protocol.ValueInput_DESIRED, Values: values.Values}
}

func TestColumnConversionBatchRetainsOrderAndIdentity(t *testing.T) {
	c := qt.New(t)
	p, request := columnConversion(c, &native.ColumnMeta{Family: "hot"}, &native.ColumnMeta{Family: "cold"})
	before := proto.CloneOf(request)
	result, err := p.ConvertValues(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateConvertedValues(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.Values, qt.HasLen, 2)
	c.Assert(string(result.Values[0].Value.Payload), qt.Equals, `{"family":"hot"}`)
	c.Assert(string(result.Values[1].Value.Payload), qt.Equals, `{"family":"cold"}`)
	c.Assert(proto.Equal(request, before), qt.IsTrue)
	back, err := p.ConvertValues(context.Background(), &protocol.ConvertValuesRequest{Engine: "ydb", From: protocol.ValueInput_DESIRED, To: protocol.ValueInput_OBSERVED, Values: result.Values})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(back, &protocol.NormalizedValues{Complete: true, Values: request.Values}), qt.IsTrue)
}

func TestColumnConversionRefusesForgedProjection(t *testing.T) {
	c := qt.New(t)
	p, request := columnConversion(c, &native.ColumnMeta{Family: "cold"})
	request.Values[0].CanonicalProjection = []byte(`{"family":"forged"}`)
	result, err := p.ConvertValues(context.Background(), request)
	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
}

func TestColumnConversionDiscardsPrefixOnFailure(t *testing.T) {
	c := qt.New(t)
	p, request := columnConversion(c, &native.ColumnMeta{Family: "cold"}, &native.ColumnMeta{DefaultValue: &native.ColumnMeta_FromSequence{FromSequence: &native.SequenceDescription{SetVal: &native.SequenceDescription_SetVal{NextValue: proto.Int64(11)}}}})
	result, err := p.ConvertValues(context.Background(), request)
	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
}

func TestColumnConversionCancellationDiscardsBatch(t *testing.T) {
	c := qt.New(t)
	p, request := columnConversion(c, &native.ColumnMeta{Family: "cold"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := p.ConvertValues(ctx, request)
	c.Assert(result, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
}
