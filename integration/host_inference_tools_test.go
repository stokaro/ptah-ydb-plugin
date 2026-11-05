//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// inferenceToolReply is what the host's MCP inference tools answer: the plan
// of a specification, or the status of a run, and whether the call failed.
type inferenceToolReply struct {
	Result struct {
		IsError           bool `json:"isError"`
		StructuredContent struct {
			Plan struct {
				Desired    string            `json:"desired"`
				Steps      []json.RawMessage `json:"steps"`
				Disclosure struct {
					Fields      []string `json:"fields"`
					RowsInScope int64    `json:"rows_in_scope"`
				} `json:"disclosure"`
			} `json:"plan"`
			Status struct {
				RunID    string `json:"run_id"`
				Progress struct {
					RowsEmbedded int64 `json:"rows_embedded"`
				} `json:"progress"`
			} `json:"status"`
		} `json:"structuredContent"`
	} `json:"result"`
}

// Ported from the host's TestInferenceTools_AnswerWithoutTheSourceContentOnYDBLive:
// the agent surface's inference tools, `ptah mcp` with the specification's
// directory as a source root, answer through this provider's embedding store.
// The plan names the columns whose text would be sent and the rows in scope,
// and the status reads the run's progress. Neither answer carries the source
// text, a stored vector component or the source key value the run's cursor
// holds, which an agent reading the answers must not see.
func TestHostInferenceToolsAnswerWithoutTheSourceContent_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	f := newEmbeddingFixture(c)
	f.exec(c, "CREATE TABLE articles (id Utf8 NOT NULL, title Utf8, body Utf8, updated_at Timestamp, embedding String, PRIMARY KEY (id))",
		fmt.Sprintf("UPSERT INTO articles (id, title, body, updated_at, embedding) VALUES ('row-key-7', %q, %q, CurrentUtcTimestamp(), "+
			"Untag(Knn::ToBinaryStringFloat([0.125f, 0.25f, 0.375f, 0.5f]), \"FloatVector\"))", theSecret, theSecret))
	spec := writeInferenceSpec(c, "http://127.0.0.1:9", inferenceTarget{column: "embedding_v2", revision: "1", noIndex: true})
	through := providerInference{host: host, manifest: manifest, address: f.address}
	through.run(c, "prepare", spec, "--run-id", "agent-run")
	// The run's state holds a source key value, as a backfill's cursor does.
	state := f.read(c, runScope("agent-run"))
	run := proto.CloneOf(state.Records.Runs[0])
	run.Cursor, run.Progress.RowsScanned, run.Progress.RowsEmbedded = []string{"row-key-7"}, 41, 41
	result, err := f.mutate(c, runScope("agent-run"), state.Token, &protocol.EmbeddingRecords{Runs: []*protocol.EmbeddingRun{run}})
	c.Assert(err, qt.IsNil, qt.Commentf("%s", result.GetMessage()))
	roots := []string{"--schema-source-root", filepath.Dir(spec)}

	plan := mcpCallOn(c, f.address, roots, "inference_plan", map[string]any{"spec": spec})
	status := mcpCallOn(c, f.address, roots, "inference_status", map[string]any{"run_id": "agent-run"})

	var planned, reported inferenceToolReply
	c.Assert(json.Unmarshal([]byte(plan), &planned), qt.IsNil, qt.Commentf("%s", plan))
	c.Assert(json.Unmarshal([]byte(status), &reported), qt.IsNil, qt.Commentf("%s", status))
	c.Assert(planned.Result.IsError, qt.IsFalse, qt.Commentf("%s", plan))
	c.Assert(planned.Result.StructuredContent.Plan.Desired, qt.Not(qt.Equals), "", qt.Commentf("%s", plan))
	c.Assert(planned.Result.StructuredContent.Plan.Steps, qt.Not(qt.HasLen), 0, qt.Commentf("%s", plan))
	c.Assert(planned.Result.StructuredContent.Plan.Disclosure.Fields, qt.DeepEquals, []string{"title", "body"})
	c.Assert(planned.Result.StructuredContent.Plan.Disclosure.RowsInScope, qt.Equals, int64(1))
	c.Assert(reported.Result.IsError, qt.IsFalse, qt.Commentf("%s", status))
	c.Assert(reported.Result.StructuredContent.Status.RunID, qt.Equals, "agent-run")
	c.Assert(reported.Result.StructuredContent.Status.Progress.RowsEmbedded, qt.Equals, int64(41))
	for _, answer := range []string{plan, status} {
		c.Assert(answer, qt.Not(qt.Contains), theSecret, qt.Commentf("the source text reached the answer"))
		c.Assert(answer, qt.Not(qt.Contains), "0.125", qt.Commentf("a stored vector component reached the answer"))
		c.Assert(answer, qt.Not(qt.Contains), "row-key-7", qt.Commentf("a source key value reached the answer"))
	}
}
