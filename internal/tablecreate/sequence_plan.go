package tablecreate

import (
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
)

// sequenceStep is the change of the sequence of one Serial column of a table
// that exists.
type sequenceStep struct {
	name      string
	operation *protocol.NormalizedValue
}

// observedSettings is the start and the increment a read reports of a
// column's sequence, a setting it leaves out read as 1.
func observedSettings(column *protocol.ObservedColumn) (ydbsequence.Settings, error) {
	return ydbsequence.Parse(column.GetIdentityStart(), column.GetIdentityIncrement())
}

// planSequences decides the sequence changes of an altered row table: one for
// each Serial column whose declared start or increment differs from what the
// read reports, written without a restart. It refuses, in the host's embedded
// engine's words, a change the server would break: a sequence some ALTER
// SEQUENCE restarted, whose restart YDB replays on every later ALTER SEQUENCE.
// The declaration is compiled with the creation rules first, which refuse a
// start or an increment on a 16-bit or 32-bit Serial, whose maximum any ALTER
// SEQUENCE raises to the Int64 maximum. A column that becomes or stops being a
// Serial is a type change, which rebuildNeeds finds first.
func planSequences(owner *protocol.Reference, table *protocol.PlanningTable, caps map[string]bool) ([]sequenceStep, string, error) {
	columns, err := compileDefinitionColumns(owner, table.Desired, caps)
	if err != nil {
		return nil, err.Error(), nil
	}
	observed, err := facetEnvelopes(owner, table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return nil, "", err
	}
	var result []sequenceStep
	for _, column := range columns {
		sequence := column.GetFromSequence()
		at := slices.IndexFunc(table.Current.GetColumns(), func(c *protocol.ObservedColumn) bool { return c.GetName() == column.GetName() })
		if sequence == nil || at < 0 || !table.Current.Columns[at].GetAutoIncrement() {
			continue
		}
		current := table.Current.Columns[at]
		held, err := observedSettings(current)
		if err != nil {
			return nil, err.Error(), nil
		}
		wanted := requestedSettings(sequence)
		if held == wanted {
			continue
		}
		subject := fmt.Sprintf("the sequence of column %q of table %q", column.GetName(), tableLabel(owner))
		if restart := current.GetSequenceRestart(); restart != "" {
			return nil, fmt.Sprintf("%s: the sequence was restarted at %s, and YDB replays that restart on every later ALTER SEQUENCE, "+
				"so the next insert would take that value again and fail on a key a row may hold (measured: `Conflict with existing key`)", subject, restart), nil
		}
		if !caps[ydbsequence.Options] {
			return nil, keyRefusal(ydbsequence.Options, "giving "+subject+" a start or an increment"), nil
		}
		// Any ALTER SEQUENCE raises the maximum of a 16-bit or 32-bit
		// Serial's sequence, a move back to the defaults included, so a
		// change is refused where the target does not keep it. A table that
		// already holds the declared settings needs no statement and is not
		// refused.
		settings := wanted
		if settings.IsDefault() {
			settings = held
		}
		if reason := ydbsequence.RangeRefusal(SerialTypeOf(column), settings, caps); reason != "" {
			return nil, keyRefusal(ydbsequence.KeepsRange, "giving "+subject+" a start or an increment ("+reason+")"), nil
		}
		captured, err := capturedColumn(current, observed.columns[column.GetName()])
		if err != nil {
			return nil, err.Error(), nil
		}
		if captured.GetFromSequence() == nil || captured.GetFromSequence().GetSetVal() != nil || heldSettings(captured.GetFromSequence()) != held {
			return nil, fmt.Sprintf("%s: the common and native reads of the sequence disagree", subject), nil
		}
		operation, err := CaptureSequence("pending", owner, &SequenceAlteration{Held: captured, Settings: wanted})
		if err != nil {
			return nil, err.Error(), nil
		}
		result = append(result, sequenceStep{name: column.GetName(), operation: operation})
	}
	return result, "", nil
}

// plan is the step that changes the sequence: it reads the table and alters
// the column.
func (s sequenceStep) plan(table *protocol.ComparisonIdentity, number int) *protocol.PlanningStep {
	return columnAlterStep(fmt.Sprintf("serial-sequence/%06d", number), table, s.name, s.operation)
}
