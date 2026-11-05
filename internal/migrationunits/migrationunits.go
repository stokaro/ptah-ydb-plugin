// Package migrationunits splits a YDB migration body into the units the host
// runs it as under the contract's units.migration-split feature (ADR 0054).
//
// A unit is one query internal/yqlquery cuts the body into: its text with the
// definitions and settings it needs carried in, the source of its own
// statements, and where the host runs it. A data query changes rows only and
// runs in a serializable transaction of the host's together with the
// checkpoint that records it. A scheme query and a BATCH query run only
// outside a transaction, so the host marks each in flight, runs it and
// checkpoints it. A block or an action call is the kind of what it runs:
// measured on YDB 26.2.1.14, `DO BEGIN UPSERT ...; END DO`, `DO $a(1)` over a
// data action and `EVALUATE FOR ... DO BEGIN UPSERT ...; END DO` run and commit
// in an interactive serializable transaction, while one that runs a scheme
// statement is refused there (`Scheme operations cannot be executed inside
// transaction`).
//
// What the Ptah host's embedded YDB engine refuses before a run touches
// anything is refused here, with the bytes of the body it refuses: a
// translation setting other than --!syntax_v1, a client delimiter directive,
// a statement that controls a transaction, and a block or an action call that
// runs a scheme statement and a statement that reads or writes a table, which
// YDB refuses whole.
package migrationunits

import (
	"errors"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
)

// Reasons the split gives for a refused body. None quotes the body: the host
// quotes the refused range itself.
const (
	// MixedReason refuses a block or an action call that runs a scheme
	// statement and a statement that reads or writes a table. A scheme
	// statement runs outside any transaction, so the units before it would
	// stay applied when YDB refuses it.
	MixedReason = "the statement runs a scheme statement and a statement that reads or writes a table in one query, " +
		"which YDB refuses whole (Queries with mixed data and scheme operations are not supported); " +
		"move the scheme statement out of the block or action into a statement of its own"
	// TransactionControlReason refuses a statement that opens, ends or marks
	// a transaction. YQL refuses COMMIT inside a query (`COMMIT not supported
	// inside YDB query`, measured on 26.2.1.14), and the host decides where
	// each transaction begins and ends.
	TransactionControlReason = "the statement controls a transaction, and the host runs each data query " +
		"in a transaction of its own; remove it"
)

// Split returns the units body runs as, in order, or the refusal of the whole
// body. A body with no statement has neither.
func Split(body string) ([]*protocol.MigrationUnit, *protocol.SplitRefusal) {
	queries, err := yqlquery.Split(body)
	if refusal, ok := errors.AsType[*yqlquery.Refusal](err); ok {
		return nil, refused(yqlquery.Span{Start: refusal.Offset, End: refusal.End}, refusal.Error())
	}
	if err != nil {
		// Split refuses with a *Refusal only; anything else is refused whole.
		return nil, refused(yqlquery.Span{Start: 0, End: len(body)}, err.Error())
	}
	units := make([]*protocol.MigrationUnit, 0, len(queries))
	for _, query := range queries {
		if query.Mixed {
			return nil, refused(query.Spans[0], MixedReason)
		}
		for i, statement := range query.Statements {
			if ControlsTransaction(statement) {
				return nil, refused(query.Spans[i], TransactionControlReason)
			}
		}
		units = append(units, &protocol.MigrationUnit{Sql: query.Text, Source: query.Source, Placement: placement(query.Kind)})
	}
	return units, nil
}

// placement is where the host runs a query of kind: a data query in a
// transaction with its checkpoint, a scheme or BATCH query outside any.
func placement(kind yqlquery.Kind) protocol.MigrationUnit_Placement {
	if kind == yqlquery.Data {
		return protocol.MigrationUnit_WITH_CHECKPOINT
	}
	return protocol.MigrationUnit_OUTSIDE
}

func refused(span yqlquery.Span, reason string) *protocol.SplitRefusal {
	// #nosec G115 -- the span is a range of the request's migration text, and runtime.MaxMessageBytes bounds a request to 4 MiB
	return &protocol.SplitRefusal{Start: uint32(span.Start), End: uint32(span.End), Reason: reason}
}

// ControlsTransaction reports a statement, read as YQL, that opens, ends or
// marks a transaction, as the host's migrator recognizes one on an engine
// whose BEGIN does not open a block: COMMIT, ROLLBACK, SAVEPOINT, ABORT, END,
// XA and BEGIN in any form; SAVE TRAN[SACTION], PREPARE TRANSACTION, RELEASE
// SAVEPOINT and START TRANSACTION; and a SET that names TRANSACTION or
// IMPLICIT_TRANSACTIONS. A block that holds one runs it inside the block, and
// YDB refuses it there.
func ControlsTransaction(statement string) bool {
	var words []yql.Token
	for _, token := range yql.Tokens(statement) {
		switch token.Type {
		case yql.TokenWhitespace, yql.TokenComment, yql.TokenUnknown:
			continue
		}
		words = append(words, token)
	}
	if len(words) == 0 {
		return false
	}
	first := words[0]
	second := func(values ...string) bool {
		return len(words) > 1 && slices.ContainsFunc(values, words[1].IsWord)
	}
	switch {
	case slices.ContainsFunc([]string{"COMMIT", "ROLLBACK", "SAVEPOINT", "ABORT", "END", "XA", "BEGIN"}, first.IsWord):
		return true
	case first.IsWord("SAVE"):
		return second("TRAN", "TRANSACTION")
	case first.IsWord("PREPARE"):
		return second("TRANSACTION")
	case first.IsWord("RELEASE"):
		return second("SAVEPOINT")
	case first.IsWord("START"):
		return second("TRANSACTION")
	case first.IsWord("SET"):
		return slices.ContainsFunc(words, func(token yql.Token) bool {
			return token.IsWord("TRANSACTION") || token.IsWord("IMPLICIT_TRANSACTIONS")
		})
	}
	return false
}
