package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A model's definition names its native field grammar by digest, so it
// stays a few kilobytes: the host repeats each definition in every coverage
// record it sends, once per table of a declaration. With the grammar inline
// the largest definitions were 31 to 37 KB.
func TestModelDefinitionsStaySmall_HappyPath(t *testing.T) {
	models := append(schema.ObservedModels(), schema.TableDesiredModel(), schema.ColumnDesiredModel())
	for _, kind := range []string{schema.IndexProperties, schema.ChangefeedProperties, schema.ColumnStorageProperties, schema.IndexSettingsProperties, schema.TopicProperties} {
		models = append(models, schema.NativeDesiredModel(kind))
	}
	c := testcheck.New(t)
	for _, model := range models {
		c.Assert(len(model.GetDefinitionJson()) <= 4096, qt.IsTrue, qt.Commentf("%s %s: %d bytes", model.GetRepresentation(), model.GetKind(), len(model.GetDefinitionJson())))
	}
}
