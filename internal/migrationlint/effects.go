package migrationlint

import (
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The reasons a safety effect gives, in the words the host's safety report
// gives a planned change of the same object, so a written statement and a
// planned one read alike.
const (
	keepReplicaTablesReason = "DROP ASYNC REPLICATION without CASCADE ends the replication and keeps its tables, " +
		"read-only for good unless it was failed over first"
	dropReplicationReason      = "DROP ASYNC REPLICATION ... CASCADE drops the replica tables with the replication"
	checkpointLossReason       = "the operation removes or resets streaming-query checkpoint state; a rollback restores the declaration, not the discarded state"
	streamingExecutionReason   = "ALTER STREAMING QUERY changes continuous query execution"
	dropTopicReason            = "DROP TOPIC removes the topic, every message it holds and every consumer's position in it"
	dropConsumerReason         = "DROP CONSUMER removes a topic consumer and its position in the topic"
	dropTransferReason         = "DROP TRANSFER stops the transfer and drops the topic consumer YDB created for it, with its position in the topic"
	dropCoordinationNodeReason = "DROP COORDINATION NODE removes the node with its semaphores and rate limiter resources, even while a session holds a lock on it"
	dropSecretReason           = "DROP SECRET removes a YDB secret whose value nothing can read back"
	dropExternalSourceReason   = "DROP EXTERNAL DATA SOURCE removes what YDB reads another system through; no data YDB stores is lost"
	dropExternalTableReason    = "DROP EXTERNAL TABLE removes the columns YDB reads files through; the files stay"
	replaceExternalReason      = "CREATE OR REPLACE changes what queries reading the external object read"
)

// statefulDrop reads a drop of a YDB object whose state a new object of the
// same name does not get back, and returns its effect.
//
// A topic holds every message written to it and every consumer's position in
// it. Dropping a transfer stops it and drops the topic consumer YDB created for
// it, with its position; an async replication dropped with CASCADE drops its
// replica tables, and one dropped without it is YD115's question. Measured on
// 26.2.1.14 and 25.1.4.7: DropNode succeeds while a session holds a semaphore
// on a coordination node, and a node created again under the same path holds
// none of the persistent semaphores the dropped one held. A streaming query
// keeps its checkpoint.
func statefulDrop(stmt *Statement) (Effect, bool) {
	w := stmt.Words
	if len(w) < 2 || w[0] != "DROP" {
		return Effect{}, false
	}
	var kind []string
	switch {
	case w[1] == "TOPIC" || w[1] == "TRANSFER":
		kind = w[1:2]
	case w[1] == "ASYNC" && len(w) > 2 && w[2] == "REPLICATION" && w[len(w)-1] == "CASCADE":
		kind = w[1:3]
	case w[1] == "COORDINATION" && len(w) > 2 && w[2] == "NODE", w[1] == "STREAMING" && len(w) > 2 && w[2] == "QUERY":
		kind = w[1:3]
	default:
		return Effect{}, false
	}
	object, _ := tableRefAt(w, stmt.SourceWords, skipIfExists(w, 1+len(kind)))
	return Effect{Fact: protocol.LintFact_LINT_FACT_STATEFUL_DROP, Object: object.Name, Kind: strings.Join(kind, " ")}, true
}

// uniqueIndexes reads YQL's unique index, which only YQL writes with UNIQUE
// after the index name: ALTER TABLE t ADD INDEX name [GLOBAL] UNIQUE [SYNC]
// [USING kind] ON (columns), at each clause of an ALTER TABLE.
//
// YDB words a failed build its own way: measured on 26.2.1.14 with
// EnableAddUniqueIndex on, ADD INDEX ... GLOBAL UNIQUE SYNC over two rows
// holding the same value answers `Duplicate key found: (v=a)`.
func uniqueIndexes(stmt *Statement) []Effect {
	w := stmt.Words
	if !isAlterTable(w) {
		return nil
	}
	table := alterTableReference(w, stmt.SourceWords)
	var effects []Effect
	for _, i := range clauseStarts(w) {
		end := clauseEnd(w, i)
		if i+2 >= end || w[i] != "ADD" || w[i+1] != "INDEX" || !identLike(w[i+2]) {
			continue
		}
		if columns, ok := uniqueKey(w[:end], stmt.SourceWords, i+3); ok {
			effects = append(effects, Effect{
				Fact: protocol.LintFact_LINT_FACT_UNIQUE_INDEX, Object: sourceWordAt(w, stmt.SourceWords, i+2), Table: table.Name,
				Columns: columns, Clause: "ADD INDEX", Failure: "YDB: Duplicate key found",
			})
		}
	}
	return effects
}

// uniqueKey reads the rest of an ADD INDEX clause from j: whether it is
// UNIQUE, and the columns its ON names.
func uniqueKey(w, source []string, j int) ([]string, bool) {
	unique := false
	for ; j < len(w); j++ {
		switch w[j] {
		case "UNIQUE":
			unique = true
		case "ON":
			columns, _, ok := keyColumnList(w, source, j+1)
			return columns, ok && unique
		}
	}
	return nil, false
}

// safetyEffect reads the judgment of a YQL statement whose effect only this
// owner knows: the drops that lose what a new object of the same name does not
// get back, the drops and replacements that change what YDB reads from another
// system, and the changes that reset or change a streaming query's continuous
// work. A host's safety assessment takes the judgment in place of its own
// reading of the words, and a statement this reads nothing of is left to that
// reading.
//
// An async replication dropped with CASCADE drops its replica tables, and one
// dropped without it leaves them read-only for good unless it was failed over
// first, which a statement read alone cannot tell.
func safetyEffect(stmt *Statement) (Effect, bool) {
	w := stmt.Words
	switch {
	case hasWordPrefix(w, "DROP", "ASYNC", "REPLICATION") && !slices.Contains(w, "CASCADE"):
		return judgment(protocol.LintImpact_LINT_IMPACT_BEHAVIORAL, keepReplicaTablesReason)
	case hasWordPrefix(w, "DROP", "ASYNC", "REPLICATION"):
		return judgment(protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE, dropReplicationReason)
	case losesCheckpoint(w):
		return judgment(protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE, checkpointLossReason)
	case hasWordPrefix(w, "ALTER", "STREAMING", "QUERY"):
		return judgment(protocol.LintImpact_LINT_IMPACT_BEHAVIORAL, streamingExecutionReason)
	case hasWordPrefix(w, "DROP", "TOPIC"):
		return judgment(protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE, dropTopicReason)
	case hasWordPrefix(w, "ALTER", "TOPIC") && hasWordSeq(w, "DROP", "CONSUMER"):
		return judgment(protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE, dropConsumerReason)
	case hasWordPrefix(w, "DROP", "TRANSFER"):
		return judgment(protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE, dropTransferReason)
	case hasWordPrefix(w, "DROP", "COORDINATION", "NODE"):
		return judgment(protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE, dropCoordinationNodeReason)
	case hasWordPrefix(w, "DROP", "SECRET"):
		return judgment(protocol.LintImpact_LINT_IMPACT_DESTRUCTIVE, dropSecretReason)
	case hasWordPrefix(w, "DROP", "EXTERNAL", "DATA", "SOURCE"):
		return judgment(protocol.LintImpact_LINT_IMPACT_BEHAVIORAL, dropExternalSourceReason)
	case hasWordPrefix(w, "DROP", "EXTERNAL", "TABLE"):
		return judgment(protocol.LintImpact_LINT_IMPACT_BEHAVIORAL, dropExternalTableReason)
	case hasWordPrefix(w, "CREATE", "OR", "REPLACE", "EXTERNAL"):
		return judgment(protocol.LintImpact_LINT_IMPACT_BEHAVIORAL, replaceExternalReason)
	default:
		return Effect{}, false
	}
}

// judgment is a safety effect of impact, for reason.
func judgment(impact protocol.LintImpact, reason string) (Effect, bool) {
	return Effect{Fact: protocol.LintFact_LINT_FACT_SAFETY_EFFECT, Impact: impact, Reason: reason}, true
}

// losesCheckpoint reports a statement that discards a streaming query's
// state: DROP STREAMING QUERY, CREATE OR REPLACE STREAMING QUERY without IF
// NOT EXISTS, which takes precedence over OR REPLACE, and an ALTER STREAMING
// QUERY that changes the body (AS DO BEGIN). Measured on YDB 26.2: replacing a
// query or changing its body keeps topic offsets and resets aggregation state.
func losesCheckpoint(w []string) bool {
	if hasWordPrefix(w, "DROP", "STREAMING", "QUERY") {
		return true
	}
	if hasWordPrefix(w, "CREATE", "OR", "REPLACE", "STREAMING", "QUERY") {
		return !hasWordPrefix(w[5:], "IF", "NOT", "EXISTS")
	}
	if !hasWordPrefix(w, "ALTER", "STREAMING", "QUERY") {
		return false
	}
	for i := 3; i < len(w); i++ {
		if hasWordPrefix(w[i:], "AS", "DO", "BEGIN") {
			return true
		}
	}
	return false
}
