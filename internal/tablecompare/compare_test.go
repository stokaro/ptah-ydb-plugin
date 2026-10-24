package tablecompare_test

import (
	"context"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

const (
	plain  = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}]}`
	hourly = `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}`
	daily  = `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400}}`
)

func withTTL(ttl string) string {
	return `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],"ttlSettings":` + ttl + `}`
}

func withPartitioning(settings string) string {
	return `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],"partitioningSettings":` + settings + `}`
}

// withLayout declares the given starting layout fields beside the default family.
func withLayout(fields string) string {
	return `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],` + fields + `}`
}

func withFamily(family string) string {
	return `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},` + family + `]}`
}

// side states the facets of one side: the table properties, and the column
// properties by column name. An empty table payload states none.
type side struct {
	table   string
	columns map[string]string
	// indexes is the index set of the table; empty states none.
	indexes string
	// changefeeds is the changefeed set of the table; empty states none.
	changefeeds string
	// uninspected makes the side's knowledge of every facet model unknown, as a
	// source that could not describe them states it.
	uninspected bool
}

// declaredIndexes and heldIndexes state index sets from native index
// definitions: a name, the key columns and the native kind.
func declaredIndexes(indexes ...string) string {
	entries := make([]string, len(indexes))
	for i, index := range indexes {
		entries[i] = `{"specification":{` + index + `},"implementation":[{"name":"indexImplTable","settings":{"primaryKey":["label","id"]},` +
			`"columns":[{"name":"label","type":{"typeId":"UTF8"},"settings":{}},{"name":"id","type":{"typeId":"UINT64"},"settings":{}}]}]}`
	}
	return `{"indexes":[` + strings.Join(entries, ",") + `]}`
}

func heldIndexes(indexes ...string) string {
	entries := make([]string, len(indexes))
	for i, index := range indexes {
		entries[i] = `{"description":{` + index + `,"status":"STATUS_READY"},"tables":[{"name":"indexImplTable","description":{` +
			`"columns":[{"name":"label","type":{"typeId":"UTF8"}},{"name":"id","type":{"typeId":"UINT64"}}],"primaryKey":["label","id"]}}]}`
	}
	return `{"indexes":[` + strings.Join(entries, ",") + `]}`
}

const (
	syncByLabel   = `"name":"by_label","indexColumns":["label"],"globalIndex":{}`
	asyncByLabel  = `"name":"by_label","indexColumns":["label"],"globalAsyncIndex":{}`
	syncByNote    = `"name":"by_note","indexColumns":["note"],"globalIndex":{}`
	coveringLabel = `"name":"by_label","indexColumns":["label"],"dataColumns":["note"],"globalIndex":{}`
	uniqueByLabel = `"name":"by_label","indexColumns":["label"],"globalUniqueIndex":{}`
	byNoteAndText = `"name":"by_both","indexColumns":["note","text"],"globalIndex":{}`
	byTextAndNote = `"name":"by_both","indexColumns":["text","note"],"globalIndex":{}`
)

const (
	declaredFeed = `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`
	heldFeed     = `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{}}}]}`
)

func owner(kind, name string) *protocol.Reference {
	reference := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}
	if kind == "column" {
		reference.Kind, reference.Path, reference.Name = "column", []string{"records"}, name
	}
	return reference
}

func subject(kind, name string) *protocol.ComparisonIdentity {
	identity := &protocol.ComparisonIdentity{Kind: kind, Database: testcomparison.Part("/local"), Schema: testcomparison.Part("app"), Name: testcomparison.Part("records")}
	if kind == "column" {
		identity.Name, identity.Parent = testcomparison.Part(name), testcomparison.Part("records")
	}
	return identity
}

func record(c *qt.C, representation protocol.ValueInput_Representation, kind, name, payload string) *protocol.ComparisonRecord {
	c.Helper()
	var model *protocol.ValueModel
	models := schema.Models()
	if representation == protocol.ValueInput_OBSERVED {
		models = schema.ObservedModels()
	}
	for _, candidate := range models {
		if candidate.Kind == kind && candidate.Representation == representation {
			model = candidate
		}
	}
	subjectKind := "table"
	if kind == schema.ColumnProperties {
		subjectKind = "column"
	}
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: kind + name, Representation: representation, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: kind, SchemaVersion: 1,
			Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner(subjectKind, name), Payload: []byte(payload)}},
	}})
	c.Assert(err, qt.IsNil)
	return &protocol.ComparisonRecord{Kind: kind, Subject: subject(subjectKind, name), Value: values.Values[0]}
}

func state(c *qt.C, representation protocol.ValueInput_Representation, facets side) (*protocol.ComparisonState, []*protocol.ComparisonParent) {
	c.Helper()
	result := &protocol.ComparisonState{Representation: representation, Coverage: &protocol.ComparisonCoverage{}}
	knowledge := protocol.ModelKnowledge_COMPLETE
	if facets.uninspected {
		knowledge = protocol.ModelKnowledge_UNINSPECTED
	}
	for _, model := range []string{schema.TableProperties, schema.ColumnProperties, schema.IndexProperties, schema.ChangefeedProperties} {
		for _, candidate := range schema.Models() {
			if candidate.Kind == model && candidate.Representation == representation {
				result.Coverage.Kinds = append(result.Coverage.Kinds, &protocol.ComparisonKindCoverage{Model: candidate,
					Sources: []*protocol.ComparisonKnowledgeSource{{Outside: testcomparison.Knowledge(knowledge)}}})
			}
		}
	}
	var parents []*protocol.ComparisonParent
	if facets.table != "" {
		result.Records = append(result.Records, record(c, representation, schema.TableProperties, "", facets.table))
		parents = append(parents, &protocol.ComparisonParent{Subject: subject("table", "")})
	}
	if facets.indexes != "" {
		result.Records = append(result.Records, record(c, representation, schema.IndexProperties, "", facets.indexes))
		parents = append(parents, &protocol.ComparisonParent{Subject: subject("table", "")})
	}
	if facets.changefeeds != "" {
		result.Records = append(result.Records, record(c, representation, schema.ChangefeedProperties, "", facets.changefeeds))
		parents = append(parents, &protocol.ComparisonParent{Subject: subject("table", "")})
	}
	for _, name := range []string{"note", "title"} {
		if payload, found := facets.columns[name]; found {
			result.Records = append(result.Records, record(c, representation, schema.ColumnProperties, name, payload))
			parents = append(parents, &protocol.ComparisonParent{Subject: subject("column", name)})
		}
	}
	return result, parents
}

// request compares the facets of app.records and its columns. Every parent
// exists on both sides, as the host reports an owner that has a facet on either.
func request(c *qt.C, desired, current side) *protocol.CompareModelsRequest {
	c.Helper()
	wanted, desiredParents := state(c, protocol.ValueInput_DESIRED, desired)
	held, currentParents := state(c, protocol.ValueInput_OBSERVED, current)
	parents := map[string]*protocol.ComparisonParent{}
	for _, parent := range append(desiredParents, currentParents...) {
		key := parent.Subject.Kind + "/" + parent.Subject.Name.Normalized
		parents[key] = &protocol.ComparisonParent{Subject: parent.Subject, Desired: true, Current: true}
	}
	request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Desired: wanted, Current: held}
	for _, key := range []string{"table/records", "column/note", "column/title"} {
		if parent, found := parents[key]; found {
			request.Parents = append(request.Parents, parent)
		}
	}
	return request
}

func compare(c *qt.C, request *protocol.CompareModelsRequest) *protocol.ComparedModels {
	c.Helper()
	original := proto.CloneOf(request)
	result, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModels(backend.Description(), request, result), qt.IsNil)
	c.Assert(proto.Equal(request, original), qt.IsTrue)
	c.Assert(result.Undecided, qt.HasLen, 0)
	return result
}

func TestDescription_DeclaresTheFacetComparison(t *testing.T) {
	c := qt.New(t)
	description := backend.Description()

	c.Assert(provider.ValidateDescription(description, nil), qt.IsNil)

	var service *protocol.ModelComparison
	for _, candidate := range description.Engines[0].Comparisons {
		if candidate.Id == tablecompare.ServiceID {
			service = candidate
		}
	}
	c.Assert(service, qt.IsNotNil)
	c.Assert(service.Placement, qt.Equals, protocol.ModelComparison_FACETS)
	c.Assert(service.Kinds, qt.DeepEquals, []string{schema.TableProperties, schema.ColumnProperties, schema.IndexProperties, schema.ChangefeedProperties})
	c.Assert(service.ChangeKinds, qt.DeepEquals, []string{schema.TablePropertiesChange})
	c.Assert(service.OwnerKinds, qt.DeepEquals, []string{"table", "column"})
	c.Assert(service.Actions, qt.HasLen, 0)
}

func TestCompare_ReportsAChange(t *testing.T) {
	for _, test := range []struct {
		name    string
		desired side
		current side
		// subjects names the owners of the reported changes, in order.
		subjects []string
	}{
		{"a TTL the table lacks", side{table: withTTL(hourly)}, side{table: plain}, []string{"records"}},
		{"a TTL that differs", side{table: withTTL(daily)}, side{table: withTTL(hourly)}, []string{"records"}},
		{"a TTL the declaration leaves out", side{table: plain}, side{table: withTTL(hourly)}, []string{"records"}},
		{"a family the table lacks", side{table: withFamily(`{"name":"archive"}`)}, side{table: plain}, []string{"records"}},
		{"a uniform layout the table was not created with", side{table: withLayout(`"uniformPartitions":"4"`)}, side{table: plain}, []string{"records"}},
		{"a layout by split points the table was not created with",
			side{table: withLayout(`"partitionAtKeys":[{"values":["10"]},{"values":["20"]}]`)}, side{table: withPartitioning(`{"minPartitionsCount":"2"}`)}, []string{"records"}},
		{"partitioning by size switched on",
			side{table: withPartitioning(`{"partitioningBySize":"ENABLED"}`)}, side{table: withPartitioning(`{"partitioningBySize":"DISABLED"}`)}, []string{"records"}},
		{"partitioning by load switched on",
			side{table: withPartitioning(`{"partitioningByLoad":"ENABLED"}`)}, side{table: withPartitioning(`{"partitioningByLoad":"DISABLED"}`)}, []string{"records"}},
		{"another partition size",
			side{table: withPartitioning(`{"partitionSizeMb":"512"}`)}, side{table: withPartitioning(`{"partitionSizeMb":"2048"}`)}, []string{"records"}},
		{"another minimum partition count",
			side{table: withPartitioning(`{"minPartitionsCount":"4"}`)}, side{table: withPartitioning(`{"minPartitionsCount":"1"}`)}, []string{"records"}},
		{"another maximum partition count",
			side{table: withPartitioning(`{"maxPartitionsCount":"8"}`)}, side{table: withPartitioning(`{"maxPartitionsCount":"50"}`)}, []string{"records"}},
		{"partitioning settings the table does not report",
			side{table: withPartitioning(`{"maxPartitionsCount":"8"}`)}, side{table: plain}, []string{"records"}},
		{"one setting among several that differs",
			side{table: withPartitioning(`{"partitioningBySize":"ENABLED","partitionSizeMb":"512","maxPartitionsCount":"8"}`)},
			side{table: withPartitioning(`{"partitioningBySize":"ENABLED","partitionSizeMb":"512","maxPartitionsCount":"50"}`)}, []string{"records"}},
		{"a family with another compression",
			side{table: withFamily(`{"name":"archive","compression":"COMPRESSION_LZ4"}`)},
			side{table: withFamily(`{"name":"archive","compression":"COMPRESSION_NONE"}`)}, []string{"records"}},
		{"a family with another storage pool",
			side{table: withFamily(`{"name":"archive","data":{"media":"ssd"}}`)},
			side{table: withFamily(`{"name":"archive","data":{"media":"hdd"}}`)}, []string{"records"}},
		{"a TTL and a family in one table change",
			side{table: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"archive"}],"ttlSettings":` + hourly + `}`}, side{table: plain}, []string{"records"}},
		{"a column in another family",
			side{table: plain, columns: map[string]string{"note": `{"family":"archive"}`}},
			side{table: plain, columns: map[string]string{"note": `{}`}}, []string{"note"}},
		{"a column back in the default family",
			side{table: plain, columns: map[string]string{"note": `{}`}},
			side{table: plain, columns: map[string]string{"note": `{"family":"archive"}`}}, []string{"note"}},
		{"an index the table lacks", side{table: plain, indexes: declaredIndexes(syncByLabel)}, side{table: plain, indexes: heldIndexes()}, []string{"records"}},
		{"an index the declaration leaves out", side{table: plain, indexes: declaredIndexes()}, side{table: plain, indexes: heldIndexes(syncByLabel)}, []string{"records"}},
		{"an index with another kind", side{indexes: declaredIndexes(asyncByLabel)}, side{indexes: heldIndexes(syncByLabel)}, []string{"records"}},
		{"an index of a unique kind", side{indexes: declaredIndexes(uniqueByLabel)}, side{indexes: heldIndexes(syncByLabel)}, []string{"records"}},
		{"an index with another covered column", side{indexes: declaredIndexes(coveringLabel)}, side{indexes: heldIndexes(syncByLabel)}, []string{"records"}},
		{"an index with its key columns in another order", side{indexes: declaredIndexes(byTextAndNote)}, side{indexes: heldIndexes(byNoteAndText)}, []string{"records"}},
		{"another index in place of the held one", side{indexes: declaredIndexes(syncByNote)}, side{indexes: heldIndexes(syncByLabel)}, []string{"records"}},
		{"an index and a TTL in one table change", side{table: withTTL(hourly), indexes: declaredIndexes(syncByLabel)}, side{table: plain, indexes: heldIndexes()}, []string{"records", "records"}},
		{"a TTL on a table the source states no properties for", side{}, side{table: withTTL(hourly)}, []string{"records"}},
		{"a table and a column",
			side{table: withTTL(hourly), columns: map[string]string{"note": `{"family":"archive"}`}},
			side{table: plain, columns: map[string]string{"note": `{}`}}, []string{"note", "records"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result := compare(c, request(c, test.desired, test.current))
			var subjects []string
			for _, change := range result.Changes {
				subjects = append(subjects, change.Subject.Name.Normalized)
			}
			c.Assert(subjects, qt.DeepEquals, test.subjects)
			for _, change := range result.Changes {
				c.Assert(change.Value.Value.Kind, qt.Equals, schema.TablePropertiesChange)
				c.Assert(change.Value.ChangeSemantics.GetReplacesOwner(), qt.IsFalse)
				c.Assert(change.Value.ChangeSemantics.GetRemovesState(), qt.IsFalse)
				c.Assert(change.Value.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
				c.Assert(change.Value.ChangeSemantics.Access, qt.Equals, protocol.ChangeSemantics_UNCHANGED)
			}
		})
	}
}

func TestCompare_ReportsNoChange(t *testing.T) {
	for _, test := range []struct {
		name    string
		desired side
		current side
	}{
		{"identical tables", side{table: withTTL(hourly)}, side{table: withTTL(hourly)}},
		{"no TTL on either side", side{table: plain}, side{table: plain}},
		{"a run interval the declaration leaves out",
			side{table: withTTL(hourly)},
			side{table: withTTL(`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`)}},
		{"the same partitioning settings",
			side{table: withPartitioning(`{"partitioningBySize":"ENABLED","partitionSizeMb":"512","maxPartitionsCount":"8"}`)},
			side{table: withPartitioning(`{"partitioningBySize":"ENABLED","partitionSizeMb":"512","maxPartitionsCount":"8"}`)}},
		{"partitioning settings the declaration leaves out", side{table: plain},
			side{table: withPartitioning(`{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1","maxPartitionsCount":"50"}`)}},
		{"a declared subset of the settings the table reports as defaults",
			side{table: withPartitioning(`{"partitioningBySize":"ENABLED"}`)},
			side{table: withPartitioning(`{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1","maxPartitionsCount":"50"}`)}},
		{"a uniform layout whose minimum the table holds",
			side{table: withLayout(`"uniformPartitions":"4"`)}, side{table: withPartitioning(`{"minPartitionsCount":"4"}`)}},
		{"a layout by split points whose minimum the table holds",
			side{table: withLayout(`"partitionAtKeys":[{"values":["10"]},{"values":["20"]}]`)}, side{table: withPartitioning(`{"minPartitionsCount":"3"}`)}},
		{"a layout beside a declared minimum, which leaves no witness",
			side{table: withLayout(`"uniformPartitions":"4","partitioningSettings":{"minPartitionsCount":"1"}`)}, side{table: withPartitioning(`{"minPartitionsCount":"1"}`)}},
		{"partition columns the table holds",
			side{table: withPartitioning(`{"partitionBy":["id"]}`)}, side{table: withPartitioning(`{"partitionBy":["id"]}`)}},
		{"partition columns the comparison leaves to the planner",
			side{table: withPartitioning(`{"partitionBy":["id","ts"]}`)}, side{table: withPartitioning(`{"partitionBy":["id"]}`)}},
		{"a family the table already holds",
			side{table: withFamily(`{"name":"archive","compression":"COMPRESSION_LZ4"}`)},
			side{table: withFamily(`{"name":"archive","compression":"COMPRESSION_LZ4"}`)}},
		{"a family the declaration leaves out", side{table: plain}, side{table: withFamily(`{"name":"archive"}`)}},
		{"a declared none compression the table reads back as unspecified",
			side{table: withFamily(`{"name":"archive","compression":"COMPRESSION_NONE"}`)}, side{table: withFamily(`{"name":"archive"}`)}},
		{"a column in the family it declares",
			side{table: plain, columns: map[string]string{"note": `{"family":"archive"}`}},
			side{table: plain, columns: map[string]string{"note": `{"family":"archive"}`}}},
		{"a column the default family names two ways",
			side{table: plain, columns: map[string]string{"note": `{"family":"default"}`}},
			side{table: plain, columns: map[string]string{"note": `{}`}}},
		{"the same index", side{indexes: declaredIndexes(syncByLabel)}, side{indexes: heldIndexes(syncByLabel)}},
		{"the same indexes in another order", side{indexes: declaredIndexes(syncByNote, syncByLabel)}, side{indexes: heldIndexes(syncByLabel, syncByNote)}},
		{"the same covering index", side{indexes: declaredIndexes(coveringLabel)}, side{indexes: heldIndexes(coveringLabel)}},
		{"no index on either side", side{indexes: declaredIndexes()}, side{indexes: heldIndexes()}},
		{"an index set only the read states", side{}, side{indexes: heldIndexes(syncByLabel)}},
		{"an index set only the declaration states", side{indexes: declaredIndexes(syncByLabel)}, side{}},
		{"a table the source states no properties for and the table holds no TTL", side{}, side{table: plain}},
		{"a table a source that could not describe the properties states none for", side{uninspected: true}, side{table: withTTL(hourly)}},
		{"a table the read states no properties for", side{table: withTTL(hourly)}, side{}},
		{"a column only the declaration states", side{table: plain, columns: map[string]string{"note": `{"family":"archive"}`}}, side{table: plain}},
		{"a column only the read states", side{table: plain}, side{table: plain, columns: map[string]string{"note": `{"family":"archive"}`}}},
		{"no facets at all", side{}, side{}},
		{"changefeeds on both sides", side{changefeeds: declaredFeed}, side{changefeeds: heldFeed}},
		{"a changefeed only the read states", side{}, side{changefeeds: heldFeed}},
		{"a changefeed the declaration adds", side{changefeeds: declaredFeed}, side{changefeeds: `{"changefeeds":[]}`}},
		{"changefeeds beside a table that differs in no setting", side{table: plain, changefeeds: declaredFeed}, side{table: plain, changefeeds: heldFeed}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result := compare(c, request(c, test.desired, test.current))
			c.Assert(result.Changes, qt.HasLen, 0)
		})
	}
}

func TestCompare_ReadsFacetsByTarget(t *testing.T) {
	for _, test := range []struct {
		name    string
		targets []string
		changes int
	}{
		{"every target", nil, 1},
		{"ydb", []string{"ydb"}, 1},
		{"the ydbs alias among others", []string{"YDBS", "postgres"}, 1},
		{"another target only", []string{"postgres"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			req := request(c, side{table: withTTL(hourly)}, side{table: plain})
			req.Desired.Records[0].Targets = test.targets

			result := compare(c, req)

			c.Assert(result.Changes, qt.HasLen, test.changes)
		})
	}
}

func TestCompare_KeepsTheDesiredStateWhole(t *testing.T) {
	c := qt.New(t)
	req := request(c, side{table: withTTL(hourly)}, side{table: plain})

	result := compare(c, req)

	c.Assert(proto.Equal(result.Desired, req.Desired), qt.IsTrue)
}

func TestCompare_FailurePath(t *testing.T) {
	t.Run("a forged normalization receipt", func(t *testing.T) {
		c := qt.New(t)
		req := request(c, side{table: withTTL(hourly)}, side{table: plain})
		req.Current.Records[0].Value.CanonicalProjection = []byte(`{"forged":true}`)

		_, err := backend.New().CompareModels(c.Context(), req)

		c.Assert(err, qt.ErrorMatches, `(?s).*normalization.*`)
	})
	t.Run("a canceled context", func(t *testing.T) {
		c := qt.New(t)
		ctx, cancel := context.WithCancel(c.Context())
		cancel()

		_, err := tablecompare.Compare(ctx, request(c, side{table: withTTL(hourly)}, side{table: plain}))

		c.Assert(err, qt.ErrorIs, context.Canceled)
	})
	t.Run("another comparison", func(t *testing.T) {
		c := qt.New(t)
		req := request(c, side{table: withTTL(hourly)}, side{table: plain})
		req.ComparisonId = "other"

		_, err := tablecompare.Compare(c.Context(), req)

		c.Assert(err, qt.ErrorMatches, "unknown row-table facet comparison")
	})
}

func TestCompare_IndexChangeNamesTheIndexSets(t *testing.T) {
	c := qt.New(t)

	result := compare(c, request(c, side{indexes: declaredIndexes(syncByLabel)}, side{indexes: heldIndexes()}))

	c.Assert(result.Changes, qt.HasLen, 1)
	c.Assert(result.Changes[0].Kind, qt.Equals, schema.IndexProperties)
	c.Assert(result.Changes[0].Subject.Kind, qt.Equals, "table")
	c.Assert(string(result.Changes[0].Value.Value.Payload), qt.Contains, `"facet":"`+schema.IndexProperties+`"`)
	c.Assert(string(result.Changes[0].Value.Value.Payload), qt.Contains, `"name":"by_label"`)
}

// A source that states an absence or a request for defaults for one table
// declares that table to have no TTL, even when it could not describe the
// properties of the other tables it declares.
func TestCompare_ReportsAChangeForATableTheSourceStatesKnowledgeFor(t *testing.T) {
	for _, state := range []protocol.ModelKnowledge_State{protocol.ModelKnowledge_ABSENT, protocol.ModelKnowledge_DEFAULTED} {
		t.Run(state.String(), func(t *testing.T) {
			c := qt.New(t)
			compared := request(c, side{uninspected: true}, side{table: withTTL(hourly)})
			compared.Desired.Coverage.Subjects = []*protocol.ComparisonSubjectCoverage{{Kind: schema.TableProperties, Subject: subject("table", ""), Knowledge: testcomparison.Knowledge(state)}}

			result := compare(c, compared)

			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Changes[0].Subject.Name.Normalized, qt.Equals, "records")
		})
	}
}
