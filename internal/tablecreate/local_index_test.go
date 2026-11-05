package tablecreate_test

import (
	"maps"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// localIndexCapabilities are the capabilities of a target that builds every
// kind of local index.
var localIndexCapabilities = map[string]bool{tablecreate.LocalBloomIndexesCapability: true, tablecreate.LocalNgramIndexesCapability: true,
	tablecreate.LocalMinMaxIndexesCapability: true}

// The local indexes of app.records as a declaration states them and as a
// read of local-ydb 26.2.1.14 describes them: an index created with its table
// reads with storage __DEFAULT and no inherited portion storage, one added
// later with it, and an n-gram index read with its defaults leaves them out.
const (
	declaredBloom  = `{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`
	declaredNgram  = `{"name":"ng","columns":["body"],"ngram":{}}`
	declaredMinMax = `{"name":"mm","columns":["id"],"minMax":{}}`
	heldLocalBloom = `{"name":"bf","columns":["body"],"storageId":"__DEFAULT","bloom":{"dataExtractor":"DEFAULT","bitsStorage":"SIMPLE_STRING","falsePositiveProbability":0.01}}`
	heldLocalNgram = `{"name":"ng","columns":["body"],"storageId":"__DEFAULT","inheritPortionStorage":true,` +
		`"ngram":{"dataExtractor":"DEFAULT","bitsStorage":"SIMPLE_STRING","caseSensitive":true}}`
)

// localIndex reads one local index from its JSON.
func localIndex(c *qt.C, text string) *native.LocalIndexObservation {
	c.Helper()
	var index native.LocalIndexObservation
	c.Assert(protojson.Unmarshal([]byte(text), &index), qt.IsNil)
	return &index
}

// withLocalIndexes is a column storage that states the given local indexes
// beside the daily TTL.
func withLocalIndexes(indexes ...string) string {
	return `{"ttl":` + deleteDaily + `,"localIndexes":[` + strings.Join(indexes, ",") + `]}`
}

// heldWithLocalIndexes is heldStorage holding the given local indexes.
func heldWithLocalIndexes(indexes ...string) string {
	return strings.TrimSuffix(heldStorage, "}") + `,"localIndexes":[` + strings.Join(indexes, ",") + `]}`
}

func TestLocalIndexEqual(t *testing.T) {
	for _, test := range []struct {
		name string
		a, b string
		want bool
	}{
		{"a read of the declared bloom index", declaredBloom, heldLocalBloom, true},
		{"a bloom index stating the default probability", `{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.1}}`,
			`{"name":"bf","columns":["body"],"bloom":{}}`, true},
		{"a read of an n-gram index with its defaults", declaredNgram, heldLocalNgram, true},
		{"an n-gram index stating its defaults", `{"name":"ng","columns":["body"],"ngram":{"falsePositiveProbability":0.1,"ngramSize":"3","caseSensitive":true}}`,
			declaredNgram, true},
		{"another probability", `{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.02}}`, heldLocalBloom, false},
		{"another n-gram size", `{"name":"ng","columns":["body"],"ngram":{"ngramSize":"4"}}`, heldLocalNgram, false},
		{"case-insensitive matching", `{"name":"ng","columns":["body"],"ngram":{"caseSensitive":false}}`, heldLocalNgram, false},
		{"another column", `{"name":"bf","columns":["count"],"bloom":{"falsePositiveProbability":0.01}}`, heldLocalBloom, false},
		{"another kind", `{"name":"bf","columns":["body"],"ngram":{"falsePositiveProbability":0.01}}`, heldLocalBloom, false},
		{"another name", `{"name":"bf2","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`, heldLocalBloom, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(tablecreate.LocalIndexEqual(localIndex(c, test.a), localIndex(c, test.b)), qt.Equals, test.want)
		})
	}
}

// TestCompileColumnTableLocalIndexes_HappyPath writes a new column table's
// local indexes inside its CREATE TABLE, in name order, every setting named as
// the host's embedded engine writes it.
func TestCompileColumnTableLocalIndexes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	storage := `{"ttl":` + deleteDaily + `,"localIndexes":[` +
		declaredBloom + `,{"name":"bf_count","columns":["count"],"bloom":{}},` + declaredNgram +
		`,{"name":"ng4","columns":["body"],"ngram":{"ngramSize":"4","caseSensitive":false,"falsePositiveProbability":0.05}},` + declaredMinMax + `]}`
	created, err := tablecreate.CompileColumnTable(owner(), columnDeclaration(c, storage), localIndexCapabilities)
	c.Assert(err, qt.IsNil)
	c.Assert(tablecreate.ColumnTableStatement("/local/app/records", created), qt.Equals, "CREATE TABLE `/local/app/records` (\n"+
		"    `ts` Timestamp NOT NULL,\n    `id` Uint64 NOT NULL,\n    `count` Uint64,\n    `body` Utf8,\n    PRIMARY KEY (`ts`, `id`),\n"+
		"    INDEX `bf` LOCAL USING bloom_filter ON (`body`) WITH (false_positive_probability = 0.01),\n"+
		"    INDEX `bf_count` LOCAL USING bloom_filter ON (`count`) WITH (false_positive_probability = 0.1),\n"+
		"    INDEX `mm` LOCAL USING min_max ON (`id`),\n"+
		"    INDEX `ng` LOCAL USING bloom_ngram_filter ON (`body`) WITH (case_sensitive = true, false_positive_probability = 0.1, ngram_size = 3),\n"+
		"    INDEX `ng4` LOCAL USING bloom_ngram_filter ON (`body`) WITH (case_sensitive = false, false_positive_probability = 0.05, ngram_size = 4)\n"+
		") WITH (STORE = COLUMN, TTL = Interval(\"PT86400S\") DELETE ON `ts`);")
	// The operation round-trips through its model.
	operation, err := tablecreate.CaptureColumnTable("create", owner(), columnDeclaration(c, storage), localIndexCapabilities)
	c.Assert(err, qt.IsNil)
	decoded, err := tablecreate.ColumnTable(operation.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(decoded, created), qt.IsTrue)
}

func TestCompileColumnTableLocalIndexes_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name         string
		indexes      string
		capabilities map[string]bool
		wantErr      string
	}{
		{"a bloom index over two columns", `{"name":"bf","columns":["body","count"],"bloom":{}}`, localIndexCapabilities,
			`local index "bf": a YDB local index reads exactly one column`},
		{"a column the table does not declare", `{"name":"bf","columns":["gone"],"bloom":{}}`, localIndexCapabilities,
			`local index "bf" reads column "gone", which the table does not declare`},
		{"an n-gram index over a number", `{"name":"ng","columns":["count"],"ngram":{}}`, localIndexCapabilities,
			`local index "ng": n-gram index column "count" must be Utf8`},
		{"an n-gram size below 3", `{"name":"ng","columns":["body"],"ngram":{"ngramSize":"2"}}`, localIndexCapabilities,
			`local index "ng": ngram_size must be between 3 and 8`},
		{"an n-gram size above 8", `{"name":"ng","columns":["body"],"ngram":{"ngramSize":"9"}}`, localIndexCapabilities,
			`local index "ng": ngram_size must be between 3 and 8`},
		{"an n-gram probability that needs nine hashes", `{"name":"ng","columns":["body"],"ngram":{"falsePositiveProbability":0.001}}`, localIndexCapabilities,
			`local index "ng": false_positive_probability must produce at most 8 n-gram hashes \(greater than 1/512\)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			created, err := tablecreate.CompileColumnTable(owner(), columnDeclaration(c, `{"localIndexes":[`+test.indexes+`]}`), test.capabilities)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(created, qt.IsNil)
		})
	}
}

// TestColumnTableLocalIndexPlan_CreationNeedsTheCapability refuses a new
// column table's local index of a kind the target does not build, naming the
// capability and the feature flag behind it, before a statement is planned.
func TestColumnTableLocalIndexPlan_CreationNeedsTheCapability(t *testing.T) {
	for _, test := range []struct {
		name, index  string
		capabilities map[string]bool
		want         string
	}{
		{"a bloom index", declaredBloom, map[string]bool{tablecreate.LocalNgramIndexesCapability: true, tablecreate.LocalMinMaxIndexesCapability: true},
			`local index "bf" is a bloom_filter index, which requires target capability local_bloom_indexes, unavailable on this ydb target ` +
				`\(YDB keeps it behind the EnableLocalBloomFilterIndex feature flag; a session reads the flag when the address names the monitoring endpoint\)`},
		{"an n-gram index", declaredNgram, map[string]bool{tablecreate.LocalBloomIndexesCapability: true, tablecreate.LocalMinMaxIndexesCapability: true},
			`local index "ng" is a bloom_ngram_filter index, which requires target capability local_ngram_indexes, .*EnableLocalBloomNgramFilterIndex.*`},
		{"a min_max index", declaredMinMax, map[string]bool{tablecreate.LocalBloomIndexesCapability: true, tablecreate.LocalNgramIndexesCapability: true},
			`local index "mm" is a min_max index, which requires target capability local_min_max_indexes, .*EnableLocalMinMaxIndex.*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			capabilities := map[string]bool{"transactional_ddl": false}
			maps.Copy(capabilities, test.capabilities)
			plan, err := backend.New().PlanFeatures(t.Context(), columnCreationRequest(c, `{"localIndexes":[`+test.index+`]}`, capabilities))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.want)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// TestColumnTableLocalIndexPlan_CreationWithTheCapability plans a new column
// table's local indexes on a target that builds them, inside its creation.
func TestColumnTableLocalIndexPlan_CreationWithTheCapability(t *testing.T) {
	c := testcheck.New(t)
	capabilities := map[string]bool{"transactional_ddl": false}
	maps.Copy(capabilities, localIndexCapabilities)
	request := columnCreationRequest(c, `{"localIndexes":[`+declaredBloom+`,`+declaredNgram+`]}`, capabilities)
	plan, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	created, err := tablecreate.ColumnTable(plan.Contributions[0].Steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(created.LocalIndexes, qt.HasLen, 2)
}

// TestCompileColumnTableLocalIndexes_TTLStaysFirstInTheKey refuses a TTL on a
// later key column that a min_max index reads, which the host's embedded
// engine takes and 26.2.1.14 refuses after the statement runs.
func TestCompileColumnTableLocalIndexes_TTLStaysFirstInTheKey(t *testing.T) {
	c := testcheck.New(t)
	storage := `{"ttl":{"enabled":{"column":"id","columnUnit":"1","tiers":[{"applyAfterSeconds":"60","delete":{}}]}},"localIndexes":[` + declaredMinMax + `]}`
	created, err := tablecreate.CompileColumnTable(owner(), columnDeclaration(c, storage), localIndexCapabilities)
	c.Assert(err, qt.ErrorMatches, `TTL column "id" must be first in the primary key: YDB refuses one later in the key whatever local index reads it `+
		"\\(`Haven't MAX-index for TTL column and TTL column is not first column in primary key`\\)")
	c.Assert(created, qt.IsNil)
}

// localIndexRequest plans the ALTER of app.records from the declared and the
// held column storage on a target that builds every kind of local index.
func localIndexRequest(c *qt.C, declared, held string) *protocol.PlanFeaturesRequest {
	c.Helper()
	return columnTTLRequest(c, declared, held, localIndexCapabilities)
}

// plannedLocalIndexSteps is each step of a plan by name, with the statement it
// writes at /local/app/records.
func plannedLocalIndexSteps(c *qt.C, plan *protocol.PlannedFeatures) (names, statements []string) {
	c.Helper()
	for _, step := range plan.GetContributions()[0].GetSteps() {
		names = append(names, step.Id.Name)
		switch step.Operation.Value.Value.Kind {
		case tablecreate.ColumnLocalIndexOperationKind:
			c.Assert(step.Operation.Phase, qt.Equals, protocol.PlanningOperation_LIFECYCLE)
			c.Assert(step.Metadata.Effects[1].Subject.Kind, qt.Equals, "index")
			alteration, err := tablecreate.ColumnLocalIndexAlteration(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			statements = append(statements, tablecreate.ColumnLocalIndexStatement("/local/app/records", alteration))
		default:
			alteration, err := tablecreate.ColumnTTLAlteration(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			statements = append(statements, tablecreate.ColumnTTLStatement("/local/app/records", alteration))
		}
	}
	return names, statements
}

// dependencyNames are a plan's dependencies as before > after.
func dependencyNames(plan *protocol.PlannedFeatures) []string {
	var result []string
	for _, dependency := range plan.GetContributions()[0].GetDependencies() {
		result = append(result, dependency.Before.Name+" > "+dependency.After.Name)
	}
	return result
}

// TestColumnTableLocalIndexPlan_HappyPath changes a column table's local
// indexes in place, as the host's embedded engine plans them without
// index_rename: an index added or dropped with one statement each, an index
// whose settings change dropped and added again under its name, the removal
// first, and a renamed index dropped under its old name and added under its
// new one.
func TestColumnTableLocalIndexPlan_HappyPath(t *testing.T) {
	const bloomTwo = `{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.02}}`
	for _, test := range []struct {
		name, declared, held      string
		steps, statements, orders []string
	}{
		{"an index added", withLocalIndexes(declaredBloom, `{"name":"bf_count","columns":["count"],"bloom":{}}`), heldWithLocalIndexes(heldLocalBloom),
			[]string{"column-table-add-local-index/000000"},
			[]string{"ALTER TABLE `/local/app/records` ADD INDEX `bf_count` LOCAL USING bloom_filter ON (`count`) WITH (false_positive_probability = 0.1);"}, nil},
		{"an index dropped", withLocalIndexes(declaredBloom), heldWithLocalIndexes(heldLocalBloom, heldLocalNgram),
			[]string{"column-table-drop-local-index/000000"}, []string{"ALTER TABLE `/local/app/records` DROP INDEX `ng`;"}, nil},
		{"an index whose probability changes", withLocalIndexes(bloomTwo, declaredNgram), heldWithLocalIndexes(heldLocalBloom, heldLocalNgram),
			[]string{"column-table-drop-local-index/000000", "column-table-add-local-index/000000"},
			[]string{"ALTER TABLE `/local/app/records` DROP INDEX `bf`;",
				"ALTER TABLE `/local/app/records` ADD INDEX `bf` LOCAL USING bloom_filter ON (`body`) WITH (false_positive_probability = 0.02);"},
			[]string{"column-table-drop-local-index/000000 > column-table-add-local-index/000000"}},
		{"an index renamed", withLocalIndexes(`{"name":"renamed_bloom","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`), heldWithLocalIndexes(heldLocalBloom),
			[]string{"column-table-drop-local-index/000000", "column-table-add-local-index/000000"},
			[]string{"ALTER TABLE `/local/app/records` DROP INDEX `bf`;",
				"ALTER TABLE `/local/app/records` ADD INDEX `renamed_bloom` LOCAL USING bloom_filter ON (`body`) WITH (false_positive_probability = 0.01);"}, nil},
		{"a min_max index added beside a TTL change",
			`{"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"7200","delete":{}}]}},"localIndexes":[` + declaredMinMax + `]}`, heldStorage,
			[]string{"column-table-ttl/000000/reset", "column-table-ttl/000000/set", "column-table-add-local-index/000000"},
			[]string{"ALTER TABLE `/local/app/records` RESET (TTL);",
				"ALTER TABLE `/local/app/records` SET (TTL = Interval(\"PT7200S\") DELETE ON `ts`);",
				"ALTER TABLE `/local/app/records` ADD INDEX `mm` LOCAL USING min_max ON (`id`);"},
			[]string{"column-table-ttl/000000/reset > column-table-ttl/000000/set"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := localIndexRequest(c, test.declared, test.held)
			original := proto.CloneOf(request)
			plan, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			names, statements := plannedLocalIndexSteps(c, plan)
			c.Assert(names, qt.DeepEquals, test.steps)
			c.Assert(statements, qt.DeepEquals, test.statements)
			c.Assert(dependencyNames(plan), qt.DeepEquals, test.orders)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, len(test.steps))
			c.Assert(proto.Equal(request, original), qt.IsTrue)
		})
	}
}

// renameCapabilities are localIndexCapabilities on a target that renames an
// index in place.
var renameCapabilities = map[string]bool{tablecreate.LocalBloomIndexesCapability: true, tablecreate.LocalNgramIndexesCapability: true,
	tablecreate.LocalMinMaxIndexesCapability: true, tablecreate.IndexRenameCapability: true}

// TestColumnTableLocalIndexPlan_RenamesInPlace renames a local index whose
// name alone changes with one RENAME INDEX on a target with index_rename, as
// the host's embedded engine plans it, and keeps the drop and the add for an
// index whose settings change too. Two indexes of one definition renamed at
// once pair in name order, each new name taking the first old one.
func TestColumnTableLocalIndexPlan_RenamesInPlace(t *testing.T) {
	const (
		renamedBloom = `{"name":"renamed_bloom","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`
		otherBloom   = `{"name":"bf_other","columns":["body"],"storageId":"__DEFAULT","bloom":{"dataExtractor":"DEFAULT","bitsStorage":"SIMPLE_STRING","falsePositiveProbability":0.01}}`
	)
	for _, test := range []struct {
		name, declared, held string
		steps, statements    []string
	}{
		{"an index renamed", withLocalIndexes(renamedBloom, declaredNgram), heldWithLocalIndexes(heldLocalBloom, heldLocalNgram),
			[]string{"column-table-rename-local-index/000000"}, []string{"ALTER TABLE `/local/app/records` RENAME INDEX `bf` TO `renamed_bloom`;"}},
		{"an index renamed with another setting", withLocalIndexes(`{"name":"renamed_bloom","columns":["body"],"bloom":{"falsePositiveProbability":0.02}}`),
			heldWithLocalIndexes(heldLocalBloom), []string{"column-table-drop-local-index/000000", "column-table-add-local-index/000000"},
			[]string{"ALTER TABLE `/local/app/records` DROP INDEX `bf`;",
				"ALTER TABLE `/local/app/records` ADD INDEX `renamed_bloom` LOCAL USING bloom_filter ON (`body`) WITH (false_positive_probability = 0.02);"}},
		{"two equal indexes renamed", withLocalIndexes(renamedBloom, `{"name":"z_bloom","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`),
			heldWithLocalIndexes(heldLocalBloom, otherBloom), []string{"column-table-rename-local-index/000000", "column-table-rename-local-index/000001"},
			[]string{"ALTER TABLE `/local/app/records` RENAME INDEX `bf` TO `renamed_bloom`;", "ALTER TABLE `/local/app/records` RENAME INDEX `bf_other` TO `z_bloom`;"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := columnTTLRequest(c, test.declared, test.held, renameCapabilities)
			plan, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			names, statements := plannedLocalIndexSteps(c, plan)
			c.Assert(names, qt.DeepEquals, test.steps)
			c.Assert(statements, qt.DeepEquals, test.statements)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, len(test.steps))
		})
	}
}

// A rename reads its table, drops the index's held name and creates its
// declared one.
func TestColumnTableLocalIndexPlan_RenameEffects(t *testing.T) {
	c := testcheck.New(t)
	request := columnTTLRequest(c, withLocalIndexes(`{"name":"renamed_bloom","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`),
		heldWithLocalIndexes(heldLocalBloom), renameCapabilities)
	plan, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	var effects []string
	for _, effect := range plan.GetContributions()[0].GetSteps()[0].GetMetadata().GetEffects() {
		effects = append(effects, effect.GetAction().String()+" "+effect.GetSubject().GetKind()+" "+effect.GetSubject().GetName().GetSource())
	}
	c.Assert(effects, qt.DeepEquals, []string{"READ table records", "DROP index bf", "CREATE index renamed_bloom"})
	c.Assert(plan.GetContributions()[0].GetSteps()[0].GetMetadata().GetImpact(), qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
}

// TestColumnTableLocalIndexPlan_NoChange plans nothing where the table holds
// the declared local indexes as a read describes them, or the declaration
// states none, which leaves the table's own alone.
func TestColumnTableLocalIndexPlan_NoChange(t *testing.T) {
	for _, test := range []struct{ name, declared, held string }{
		{"the declared indexes, read with their storage and defaults", withLocalIndexes(declaredBloom, declaredNgram), heldWithLocalIndexes(heldLocalBloom, heldLocalNgram)},
		{"no local index declared", `{"ttl":` + deleteDaily + `}`, heldWithLocalIndexes(heldLocalBloom, heldLocalNgram)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(t.Context(), localIndexRequest(c, test.declared, test.held))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 0)
		})
	}
}

// TestColumnTableLocalIndexPlan_KeptIndexNeedsNoCapability changes the TTL of
// a column table on a target that no longer builds the kind of an index the
// table keeps: only an index the plan adds needs its capability.
func TestColumnTableLocalIndexPlan_KeptIndexNeedsNoCapability(t *testing.T) {
	c := testcheck.New(t)
	declared := `{"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"7200","delete":{}}]}},"localIndexes":[` + declaredBloom + `]}`
	plan, err := backend.New().PlanFeatures(t.Context(), columnTTLRequest(c, declared, heldWithLocalIndexes(heldLocalBloom), nil))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	names, _ := plannedLocalIndexSteps(c, plan)
	c.Assert(names, qt.DeepEquals, []string{"column-table-ttl/000000/reset", "column-table-ttl/000000/set"})
}

// TestColumnTableLocalIndexComparison reports a column table whose local
// indexes differ from the ones a declaration states, and none where the
// declaration states none.
func TestColumnTableLocalIndexComparison(t *testing.T) {
	for _, test := range []struct {
		name, declared, held string
		want                 bool
	}{
		{"the same indexes", withLocalIndexes(declaredBloom, declaredNgram), heldWithLocalIndexes(heldLocalBloom, heldLocalNgram), false},
		{"an index the table holds and the declaration leaves out", withLocalIndexes(declaredBloom), heldWithLocalIndexes(heldLocalBloom, heldLocalNgram), true},
		{"an index the declaration adds", withLocalIndexes(declaredBloom, declaredNgram), heldWithLocalIndexes(heldLocalBloom), true},
		{"another probability", withLocalIndexes(`{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.5}}`), heldWithLocalIndexes(heldLocalBloom), true},
		{"no local index declared", `{"ttl":` + deleteDaily + `}`, heldWithLocalIndexes(heldLocalBloom, heldLocalNgram), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			differs, err := tablecreate.StorageFacetDiffers(storageFacet(c, protocol.ValueInput_DESIRED, test.declared).Value.Value,
				storageFacet(c, protocol.ValueInput_OBSERVED, test.held).Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(differs, qt.Equals, test.want)
		})
	}
}

// localIndexStep normalizes a local-index step of app.records with the given
// payload.
func localIndexStep(payload string) (*protocol.NormalizedValue, error) {
	model := tablecreate.ColumnLocalIndexModel()
	return tablecreate.NormalizeColumnLocalIndex(&protocol.ValueInput{Id: "step", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: schema.ProviderID, Kind: tablecreate.ColumnLocalIndexOperationKind, Owner: owner(), SchemaVersion: 1, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Payload: []byte(payload)}})
}

func TestColumnLocalIndexStep_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, payload string
		impact        protocol.ChangeSemantics_Impact
	}{
		{"an addition", `{"add":{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.1}}}`, protocol.ChangeSemantics_ADDITIVE},
		{"a removal of an index as a read describes it", `{"drop":` + heldLocalNgram + `}`, protocol.ChangeSemantics_DESTRUCTIVE},
		{"a rename of an index as a read describes it", `{"add":{"name":"bf2","columns":["body"],"bloom":{"falsePositiveProbability":0.01}},"drop":` + heldLocalBloom + `}`,
			protocol.ChangeSemantics_BEHAVIORAL},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			normalized, err := localIndexStep(test.payload)
			c.Assert(err, qt.IsNil)
			c.Assert(normalized.OperationSemantics.Impact, qt.Equals, test.impact)
		})
	}
}

func TestColumnLocalIndexStep_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, payload, wantErr string }{
		{"a rename that changes the index", `{"add":{"name":"bf2","columns":["body"],"bloom":{"falsePositiveProbability":0.1}},"drop":` + heldLocalBloom + `}`,
			`a column table local-index rename keeps every setting of the index and changes its name`},
		{"a rename onto its own name", `{"add":{"name":"bf","columns":["body"],"bloom":{"falsePositiveProbability":0.01}},"drop":` + heldLocalBloom + `}`,
			`a column table local-index rename keeps every setting of the index and changes its name`},
		{"neither", `{}`, `a column table local-index step adds, drops or renames one index`},
		{"an addition that leaves a setting to the server", `{"add":{"name":"bf","columns":["body"],"bloom":{}}}`,
			`a column table local-index step names every setting of the index it adds and no storage`},
		{"an addition that states its storage", `{"add":{"name":"bf","columns":["body"],"storageId":"__DEFAULT","bloom":{"falsePositiveProbability":0.1}}}`,
			`a column table local-index step names every setting of the index it adds and no storage`},
		{"an addition over two columns", `{"add":{"name":"bf","columns":["body","count"],"bloom":{"falsePositiveProbability":0.1}}}`,
			`a YDB local index reads exactly one column`},
		{"a removal of an index in storage YDB does not write", `{"drop":{"name":"mm","columns":["id"],"storageId":"__DEFAULT","minMax":{}}}`,
			`unsupported YDB min/max storage`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			normalized, err := localIndexStep(test.payload)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
