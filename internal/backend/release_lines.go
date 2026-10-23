package backend

import protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

// unprobedReason is why four of the six lines run no job in the plugin's own
// suite. YDB publishes no support period for its open-source lines, and the
// suite runs the newest and the oldest certified line.
const unprobedReason = "YDB publishes no support period for its open-source lines, and the test suite runs only " +
	"the newest and the oldest certified line; the others were measured by hand once."

// releaseLines declares the YDB release lines the plugin has a launch recipe
// for, newest first. Certified lines run the live suite; the others are
// best-effort and say why.
func releaseLines() []*protocol.ReleaseLine {
	certified := func(line, note string) *protocol.ReleaseLine {
		return &protocol.ReleaseLine{Line: line, Label: "YDB " + line, Support: protocol.ReleaseLine_CERTIFIED,
			Image: "ydbplatform/local-ydb:" + line, Note: note}
	}
	bestEffort := func(line, note string) *protocol.ReleaseLine {
		return &protocol.ReleaseLine{Line: line, Label: "YDB " + line, Support: protocol.ReleaseLine_BEST_EFFORT,
			Image: "ydbplatform/local-ydb:" + line, Unprobed: unprobedReason, Note: note}
	}
	return []*protocol.ReleaseLine{
		certified("26.2", "The current release, and the first line that sets and drops a column default in place."),
		bestEffort("26.1", "The first line that adds a column with a default to a table holding rows."),
		bestEffort("25.4", "The first line whose topic consumer takes availability_period."),
		bestEffort("25.3", "The first line that takes a literal default on JsonDocument and DyNumber."),
		bestEffort("25.2", "The first line with the 64-bit date and time types and a Decimal of any precision."),
		certified("25.1", "The oldest line the plugin supports: no 64-bit date types and Decimal(22,9) only."),
	}
}
