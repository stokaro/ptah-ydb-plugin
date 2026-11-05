//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// inventoryEntry is one kind of a drift report's inventory.
type inventoryEntry struct {
	Kind     string `json:"kind"`
	Desired  int    `json:"desired"`
	Observed int    `json:"observed"`
}

// nativeInventory runs `schema drift --format json` over source and returns the
// report's verdict and the entries of the provider's change kinds, in the order
// the report sorts them.
func nativeInventory(c *qt.C, directory, source string) (bool, []inventoryEntry) {
	c.Helper()
	stdout, stderr, exit := hostVerb(c, "schema", "drift", "--format", "json", "--schemas", directory, "--schema-file", writeSource(c, "desired.yaml", source))
	c.Assert(exit == 0 || exit == 1, qt.IsTrue, qt.Commentf("exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr))
	var report struct {
		Drift     bool             `json:"drift"`
		Inventory []inventoryEntry `json:"inventory"`
	}
	c.Assert(json.Unmarshal([]byte(stdout), &report), qt.IsNil, qt.Commentf("stdout:\n%s", stdout))
	var native []inventoryEntry
	for _, entry := range report.Inventory {
		if strings.HasPrefix(entry.Kind, schema.ProviderID+"/") {
			native = append(native, entry)
		}
	}
	return report.Drift, native
}

// The objects the host cannot pair itself reach its drift report through the
// provider's counts. Against a server that holds what the source declares, each
// kind counts the same on both sides, and the report says there is no drift.
// Against a source that changes one node, adds one and leaves one out, the node
// both hold counts on each side whether or not it differs, the node only the
// source declares counts as desired, and the node only the server holds counts
// as observed. The topic the second source leaves alone still counts.
func TestHostDriftInventoryCountsNativeObjects_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.ComparisonCountsFeature)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = opened.Driver.Topic().Drop(cleanup, path.Join(database, directory, "events"))
		for _, name := range []string{"same", "changed", "dropped"} {
			_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(database, directory, name))
		}
	})
	node := func(name, mode string) string {
		return "  " + name + ":\n    schema: " + directory + "\n    read_consistency_mode: " + mode + "\n"
	}
	topic := "topics:\n  events:\n    schema: " + directory + "\n"
	held := "coordination_nodes:\n" + node("same", "relaxed") + node("changed", "relaxed") + node("dropped", "relaxed") + topic
	applied, err := hostApplySource(c, directory, "yaml", held)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))

	drift, inventory := nativeInventory(c, directory, held)

	c.Assert(drift, qt.IsFalse)
	c.Assert(inventory, qt.DeepEquals, []inventoryEntry{
		{Kind: schema.CoordinationChange, Desired: 3, Observed: 3},
		{Kind: schema.TopicChange, Desired: 1, Observed: 1},
	})

	drift, inventory = nativeInventory(c, directory, "coordination_nodes:\n"+node("same", "relaxed")+node("changed", "strict")+node("created", "relaxed")+node("another", "relaxed")+topic)

	c.Assert(drift, qt.IsTrue)
	c.Assert(inventory, qt.DeepEquals, []inventoryEntry{
		{Kind: schema.CoordinationChange, Desired: 4, Observed: 3},
		{Kind: schema.TopicChange, Desired: 1, Observed: 1},
	})
}
