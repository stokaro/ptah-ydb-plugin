//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The former host TestYAMLDocument_DescribesTopics says that a YAML document
// claims the topic namespace even when its topics map is empty. A topic held
// in the selected YDB directory must therefore be planned for removal.
func TestHostYAMLDocumentClaimsTopicNamespace(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	absolute := path.Join(database, directory, "undeclared")
	createSchemaObject(c, opened, "CREATE TOPIC `"+absolute+"`", "DROP TOPIC `"+absolute+"`")

	c.Assert(scopedStatements(c, directory, "topics: {}\n"), qt.DeepEquals,
		[]string{"DROP TOPIC `" + directory + "/undeclared`"})
}
