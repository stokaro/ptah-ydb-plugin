package schema

import (
	"fmt"
	"path"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"
)

const (
	nestedDatabasePrefix = "Directory "
	nestedDatabaseSuffix = " is the root of another YDB database, nested in this one"
)

// NestedDatabaseReason is the reason a read gives for a directory that is, or
// lies below, the root of another database nested in the one it reads, such as
// db2 when /local/db2 sits below /local. root is that database's directory,
// relative to the read's database. The read leaves what the nested database
// holds out and claims the directory complete with this reason, and a planner
// reads root back with [NestedDatabase] to refuse a creation there.
func NestedDatabaseReason(root string) string {
	return nestedDatabasePrefix + root + nestedDatabaseSuffix
}

// NestedDatabase returns the root of the nested database whose directory
// knowledge describes, as [NestedDatabaseReason] wrote it, and whether
// knowledge is such a claim. The claim is complete: the read's database
// holds nothing there.
func NestedDatabase(knowledge *protocol.ModelKnowledge) (string, bool) {
	if knowledge.GetState() != protocol.ModelKnowledge_COMPLETE {
		return "", false
	}
	root, prefixed := strings.CutPrefix(knowledge.GetReason(), nestedDatabasePrefix)
	root, suffixed := strings.CutSuffix(root, nestedDatabaseSuffix)
	if !prefixed || !suffixed || root == "" {
		return "", false
	}
	return root, true
}

// NestedDatabaseCreation is the refusal of a plan that creates the object
// noun at display, a path relative to database, inside the nested database
// whose root is root. YDB runs such a creation through a session of database
// and writes the object into the nested database's scheme, where no later
// read of database sees it and no drop through that session finds it.
func NestedDatabaseCreation(noun, display, root, database string) string {
	outer, nested := database, path.Join(database, root)
	if database == "" {
		outer, nested = "this database", root
	}
	return fmt.Sprintf("this plan creates the %s %s inside %s, which is another YDB database nested in %s: "+
		"a session of %s neither reads nor drops what it holds. Declare the %s in a source for %s and apply that source there",
		noun, display, nested, outer, outer, noun, nested)
}
