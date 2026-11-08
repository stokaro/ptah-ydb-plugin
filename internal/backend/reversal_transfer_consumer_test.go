package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A transfer whose lambda the forward change replaced keeps the consumer it
// reads through, the one YDB made for it where the declaration names none, so
// what the change leaves reads that consumer and the reverse puts the lambda
// back in place. Without it the reverse compared the consumer YDB made with
// none and refused, as the host's embedded engine did on 26.2.1.14 (G233).
func TestReverseChanges_KeepsATransfersConsumer(t *testing.T) {
	for _, test := range []struct {
		name, held, declared string
	}{
		{name: "the consumer YDB made, which the declaration does not name", held: "ydb_made", declared: ""},
		{name: "a consumer the declaration names", held: "reader", declared: "reader"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read, declared := ingest(), ingest()
			read.Consumer = test.held
			declared.Consumer, declared.Lambda = test.declared, "($m) -> { return []; }"
			request := objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", declaredTransfer(c, &declared),
				readTransfer(c, &read, replication.StateRunning))

			_, reverse, reversed := reverseOf(c, request)

			left, _, err := schema.ObservedTransfer(reversed.ForwardState[0].Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(left.Consumer, qt.Equals, test.held)
			c.Assert(reverseText(c, replicationplan.TransferServiceID, reverse.Capabilities, reversed), qt.HasLen, 1)
		})
	}
}
