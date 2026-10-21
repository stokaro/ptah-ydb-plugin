//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestNativeIndexObservationsSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	for _, test := range []struct {
		name, clause, kind string
		tables             int
		only26             bool
	}{
		{name: "sync", clause: "GLOBAL SYNC ON (label) COVER (extra)", kind: "global_index", tables: 1},
		{name: "async", clause: "GLOBAL ASYNC ON (label) COVER (extra)", kind: "global_async_index", tables: 1},
		{name: "unique", clause: "GLOBAL UNIQUE SYNC ON (label)", kind: "global_unique_index", tables: 1},
		{name: "vector", clause: "GLOBAL USING vector_kmeans_tree ON (embedding) WITH (distance=cosine,vector_type=float,vector_dimension=3,levels=1,clusters=2)", kind: "global_vector_kmeans_tree_index", tables: 2},
		{name: "vector_prefix", clause: "GLOBAL USING vector_kmeans_tree ON (prefix,embedding) COVER (extra) WITH (distance=cosine,vector_type=float,vector_dimension=3,levels=1,clusters=2)", kind: "global_vector_kmeans_tree_index", tables: 3},
		{name: "fulltext_plain", clause: "GLOBAL USING fulltext_plain ON (label) WITH (tokenizer=standard,use_filter_lowercase=true)", kind: "global_fulltext_plain_index", tables: 1, only26: true},
		{name: "fulltext_relevance", clause: "GLOBAL USING fulltext_relevance ON (label) WITH (tokenizer=standard,use_filter_lowercase=true)", kind: "global_fulltext_relevance_index", tables: 4, only26: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			opened, directory, database := inspectionFixture(c)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var version string
			c.Assert(opened.DB.QueryRowContext(ctx, "SELECT Version()").Scan(&version), qt.IsNil)
			tablePath := path.Join(database, directory, "records")
			statement := fmt.Sprintf("CREATE TABLE `%s` (id Uint64 NOT NULL,label Utf8,extra Utf8,prefix Uint64,embedding String,PRIMARY KEY(id),INDEX idx %s)", tablePath, test.clause)
			_, err := opened.DB.ExecContext(ctx, statement)
			if err == nil {
				c.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					_, err := opened.DB.ExecContext(cleanup, "DROP TABLE `"+tablePath+"`")
					c.Check(err, qt.IsNil)
				})
			}
			if test.only26 && strings.HasPrefix(version, "25.1.") {
				c.Assert(err, qt.IsNotNil)
				return
			}
			c.Assert(err, qt.IsNil)
			if test.name == "sync" {
				_, err := opened.DB.ExecContext(ctx, "ALTER TABLE `"+tablePath+"` ALTER INDEX idx SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT=4)")
				c.Assert(err, qt.IsNil)
			}
			direct, err := inspect.Read(ctx, opened.Driver, database, []string{directory})
			c.Assert(err, qt.IsNil)
			c.Assert(direct.Tables, qt.HasLen, 1)
			session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
			request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
			snapshot, err := client.Inspect(ctx, request)
			c.Assert(err, qt.IsNil)
			table := snapshot.Tables[0]
			c.Assert(table.Facets, qt.HasLen, 3)
			c.Assert(table.Facets[1].Kind, qt.Equals, schema.IndexProperties)
			var indexes native.IndexObservations
			c.Assert(protojson.Unmarshal(table.Facets[1].Payload, &indexes), qt.IsNil)
			c.Assert(indexes.Indexes, qt.HasLen, 1)
			observed := indexes.Indexes[0]
			c.Assert(observed.Description.Name, qt.Equals, "idx")
			selected := observed.Description.ProtoReflect().WhichOneof(observed.Description.ProtoReflect().Descriptor().Oneofs().ByName("type"))
			c.Assert(string(selected.Name()), qt.Equals, test.kind)
			c.Assert(observed.Tables, qt.HasLen, test.tables)
			for _, physical := range observed.Tables {
				c.Assert(len(physical.Description.Columns) > 0, qt.IsTrue)
				c.Assert(len(physical.Description.PrimaryKey) > 0, qt.IsTrue)
			}
			if test.name == "sync" {
				c.Assert(observed.Tables[0].Description.GetPartitioningSettings().GetMinPartitionsCount(), qt.Equals, uint64(4))
				c.Assert(observed.Description.IndexColumns, qt.DeepEquals, []string{"label"})
				c.Assert(observed.Description.DataColumns, qt.DeepEquals, []string{"extra"})
			}
			if strings.HasPrefix(test.name, "vector") {
				settings := observed.Description.GetGlobalVectorKmeansTreeIndex().GetVectorSettings()
				c.Assert(settings.GetSettings().GetMetric(), qt.Equals, native.VectorIndexSettings_DISTANCE_COSINE)
				c.Assert(settings.GetSettings().GetVectorType(), qt.Equals, native.VectorIndexSettings_VECTOR_TYPE_FLOAT)
				c.Assert(settings.GetSettings().GetVectorDimension(), qt.Equals, uint32(3))
				c.Assert(settings.GetClusters(), qt.Equals, uint32(2))
				c.Assert(settings.GetLevels(), qt.Equals, uint32(1))
			}
			if test.name == "vector_prefix" {
				c.Assert(observed.Description.IndexColumns, qt.DeepEquals, []string{"prefix", "embedding"})
				c.Assert(observed.Description.DataColumns, qt.DeepEquals, []string{"extra"})
				var sequence *native.SequenceDescription
				for _, physical := range observed.Tables {
					if physical.Name == "indexImplPrefixTable" {
						for _, column := range physical.Description.Columns {
							if column.GetFromSequence() != nil {
								sequence = column.GetFromSequence()
							}
						}
					}
				}
				// The 25.1 prefix table has no sequence default. In 26.2 the
				// sequence type is an API field missing from upstream Go bindings.
				if strings.HasPrefix(version, "25.1.") {
					c.Assert(sequence, qt.IsNil)
				} else {
					c.Assert(sequence, qt.IsNotNil)
					c.Assert(sequence.GetDataType().GetTypeId(), qt.Equals, Ydb.Type_INT64)
				}
			}
			if strings.HasPrefix(test.name, "fulltext") {
				var settings *native.FulltextIndexSettings
				if test.name == "fulltext_plain" {
					settings = observed.Description.GetGlobalFulltextPlainIndex().GetFulltextSettings()
				} else {
					settings = observed.Description.GetGlobalFulltextRelevanceIndex().GetFulltextSettings()
				}
				c.Assert(settings.GetColumns(), qt.HasLen, 1)
				c.Assert(settings.Columns[0].GetColumn(), qt.Equals, "label")
				c.Assert(settings.Columns[0].GetAnalyzers().GetTokenizer(), qt.Equals, native.FulltextIndexSettings_STANDARD)
				c.Assert(settings.Columns[0].GetAnalyzers().GetUseFilterLowercase(), qt.IsTrue)
			}
			values, err := provider.InspectionValues(client.Description(), request, snapshot)
			c.Assert(err, qt.IsNil)
			normalized, err := client.NormalizeValues(ctx, values)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateNormalizedInspection(client.Description(), request, snapshot, normalized), qt.IsNil)
			c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
			convertChildProperties(c, client, values)
		})
	}
}
