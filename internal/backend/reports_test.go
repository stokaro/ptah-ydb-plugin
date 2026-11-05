package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// reportedTopic is an observed topic of two consumers.
func reportedTopic(c *qt.C) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: schema.TopicProperties, Name: "events"}
	value, err := schema.CaptureObject(owner, &native.DescribeTopicResult{PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 1},
		Consumers: []*native.Consumer{{Name: "one"}, {Name: "two"}}})
	c.Assert(err, qt.IsNil)
	return value
}

// The description reports the models it counts, and a request for them
// passes the SDK's checks against the description a host that lists the
// feature negotiates.
func TestDescriptionReportsTheCountedModels(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	c.Assert(description.Features, qt.Contains, provider.ReportCountsFeature)
	c.Assert(description.Engines[0].Reports, qt.HasLen, len(schema.Reports()))
	negotiated := provider.NegotiatedDescription(description, []string{provider.ReportCountsFeature})
	request := &protocol.ReportValuesRequest{Engine: "ydb", TargetFeatures: []string{provider.ReportCountsFeature},
		Values: []*protocol.ValueInput{{Id: "0", Representation: protocol.ValueInput_OBSERVED, Value: reportedTopic(c)}}}
	c.Assert(provider.ValidateReportValuesRequest(negotiated, request), qt.IsNil)
	c.Assert(provider.NegotiatedDescription(description, nil).Engines[0].Reports, qt.HasLen, 0)
}

func TestReportValues_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.ReportValuesRequest{Engine: "ydb", TargetFeatures: []string{provider.ReportCountsFeature},
		Values: []*protocol.ValueInput{{Id: "0", Representation: protocol.ValueInput_OBSERVED, Value: reportedTopic(c)}}}

	reply, err := backend.New().ReportValues(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateValueReports(provider.NegotiatedDescription(backend.Description(), []string{provider.ReportCountsFeature}), request, reply), qt.IsNil)
	c.Assert(reply.Values[0].Id, qt.Equals, "0")
	c.Assert(reply.Values[0].Counts[1].Name, qt.Equals, "topic_consumers")
	c.Assert(reply.Values[0].Counts[1].Value, qt.Equals, uint64(2))
}

func TestReportValues_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	column, err := schema.CaptureColumn(&protocol.Reference{Engine: "ydb", Database: "/local", Kind: "column", Path: []string{"t"}, Name: "c"},
		&native.ColumnMeta{Name: "c", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}})
	c.Assert(err, qt.IsNil)
	request := &protocol.ReportValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "0", Representation: protocol.ValueInput_OBSERVED, Value: column}}}

	reply, err := backend.New().ReportValues(c.Context(), request)

	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(reply, qt.IsNil)
}
