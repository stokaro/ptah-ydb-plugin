package backend_test

import (
	"bytes"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// exportedObject is the desired value of a native object of kind at the
// database root, named name, as an export's request carries it.
func exportedObject(c *qt.C, kind, name string, value *protocol.Envelope) *protocol.EncodeSourceValue {
	c.Helper()
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT, Value: &protocol.NormalizedValue{Id: kind + "/" + name, Value: value,
		NormalizedOwner: proto.CloneOf(value.Owner), CanonicalFormat: schema.NativeDesiredModel(kind).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}}
}

// A Go export writes the external data sources, then the external tables,
// then the streaming queries, whatever order the request gives them in: a
// table reads its data source, and a query may read a table. Ported from the
// host's TestEncodeAnnotations_WritesExternalTablesBeforeStreamingQueries.
func TestEncodeSource_GoWritesExternalObjectsBeforeStreamingQueries_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	query, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Name: "count"}, streaming.Spec{Text: "SELECT 1;"}, false)
	c.Assert(err, qt.IsNil)
	tableIntent, err := externals.TableIntent(ydbdecl.ExternalTable{DataSource: "s3", Location: "/logs/", Columns: []ydbdecl.ExternalColumn{{Name: "id", Type: "Int64"}},
		Options: map[string]string{"FORMAT": "csv_with_names"}})
	c.Assert(err, qt.IsNil)
	table, err := schema.CaptureExternalIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ExternalTableProperties, Name: "logs"}, tableIntent)
	c.Assert(err, qt.IsNil)
	sourceIntent, err := externals.SourceIntent(ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: "https://storage.example.com/bucket", AuthMethod: "NONE"})
	c.Assert(err, qt.IsNil)
	source, err := schema.CaptureExternalIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ExternalSourceProperties, Name: "s3"}, sourceIntent)
	c.Assert(err, qt.IsNil)
	request := encodeRequest(protocol.SourceDocument_GO_ANNOTATIONS, exportedObject(c, schema.StreamingQueryProperties, "count", query),
		exportedObject(c, schema.ExternalTableProperties, "logs", table), exportedObject(c, schema.ExternalSourceProperties, "s3", source))
	// The streaming query's value needs planning.object-context, which a
	// request names among its target features.
	request.TargetFeatures = []string{provider.PlanningObjectContextFeature}

	encoded, err := backend.New().EncodeSource(provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature,
		provider.EncodeGroupsFeature, provider.PlanningObjectContextFeature}), request)

	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	var directives []string
	for _, line := range goLines(encoded) {
		directives = append(directives, line.Directive+" "+line.Placement.String())
	}
	c.Assert(directives, qt.DeepEquals, []string{"ptah:schema:externaldatasource PLACEMENT_UNSPECIFIED", "ptah:schema:externaltable PLACEMENT_UNSPECIFIED",
		"ptah:schema:streamingquery PLACEMENT_UNSPECIFIED"})
}

// A Go export writes a global index's automatic partitioning by size and its
// largest partition count on the index's own directive, and decoding the
// directive gives the facet back. Ported from the host's
// TestEncodeAnnotations_Settings_HappyPath.
func TestEncodeSource_GoWritesIndexPartitioningOnTheIndexDirective_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	settings := `{"partitioningSettings":{"maxPartitionsCount":"16","partitioningBySize":"ENABLED"}}`
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS, indexFacet(c, "by_time", settings))

	encoded, err := backend.New().EncodeSource(groupedEncodingContext(c), request)

	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Losses, qt.HasLen, 0)
	c.Assert(goLines(encoded), qt.DeepEquals, []goLine{{Directive: "ptah:schema:table"},
		{Directive: "ptah:schema:index", Attributes: []string{"auto_partitioning_by_size=ENABLED", "auto_partitioning_max_partitions_count=16"}}})
	c.Assert(decodedProjections(c, groupedEncodingContext(c), sourceDocument(protocol.SourceDocument_GO_ANNOTATIONS, request, encoded)), qt.DeepEquals,
		map[string]string{schema.IndexSettingsProperties + " app/by_time": settings})
}
