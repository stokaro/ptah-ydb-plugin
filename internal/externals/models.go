package externals

import (
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Display names the external object an owner names by its path, for
// messages.
func Display(owner *protocol.Reference) string {
	return ydbdecl.ExternalDisplay(owner.GetSchema(), owner.GetName())
}

// SourcePair reads a read data source and its declaration, either nil, as
// the host's specifications, with paths relative to root.
func SourcePair(current, desired *protocol.Envelope, root string) (*ydbdecl.ExternalDataSource, *ydbdecl.ExternalDataSource, error) {
	var before, after *ydbdecl.ExternalDataSource
	if current != nil {
		observed, err := schema.ObservedExternalSource(current)
		if err != nil {
			return nil, nil, err
		}
		spec, err := SourceFromObserved(observed, schema.InterpretationRoot(current))
		if err != nil {
			return nil, nil, fmt.Errorf("external data source %s: %w", Display(current.GetOwner()), err)
		}
		before = &spec
	}
	if desired != nil {
		intent, err := schema.DesiredExternalSource(desired)
		if err != nil {
			return nil, nil, err
		}
		spec, err := SourceFromIntent(intent, schema.InterpretationRoot(desired))
		if err != nil {
			return nil, nil, fmt.Errorf("external data source %s: %w", Display(desired.GetOwner()), err)
		}
		after = &spec
	}
	return before, after, nil
}

// TablePair reads a read external table and its declaration, either nil, as
// the host's specifications, with paths relative to root.
func TablePair(current, desired *protocol.Envelope, root string) (*ydbdecl.ExternalTable, *ydbdecl.ExternalTable, error) {
	var before, after *ydbdecl.ExternalTable
	if current != nil {
		observed, err := schema.ObservedExternalTable(current)
		if err != nil {
			return nil, nil, err
		}
		spec, err := TableFromObserved(observed, schema.InterpretationRoot(current))
		if err != nil {
			return nil, nil, fmt.Errorf("external table %s: %w", Display(current.GetOwner()), err)
		}
		before = &spec
	}
	if desired != nil {
		intent, err := schema.DesiredExternalTable(desired)
		if err != nil {
			return nil, nil, err
		}
		spec, err := TableFromIntent(intent, schema.InterpretationRoot(desired))
		if err != nil {
			return nil, nil, fmt.Errorf("external table %s: %w", Display(desired.GetOwner()), err)
		}
		after = &spec
	}
	return before, after, nil
}

// Equal reports whether the read object current already holds desired, its
// declaration, compared as the host compares one: SameDataSource or
// SameTable against root.
func Equal(current, desired *protocol.Envelope, root string) (bool, error) {
	if current.GetKind() == schema.ExternalSourceProperties {
		before, after, err := SourcePair(current, desired, root)
		if err != nil {
			return false, err
		}
		return SameDataSource(*after, *before, root), nil
	}
	before, after, err := TablePair(current, desired, root)
	if err != nil {
		return false, err
	}
	return SameTable(*after, *before, root), nil
}

// Keep returns the declaration that keeps the read object current as it is,
// under owner: the specification it holds, with paths relative to root, and
// no access requirement.
func Keep(owner *protocol.Reference, current *protocol.Envelope, root string) (*protocol.Envelope, error) {
	if current.GetKind() == schema.ExternalSourceProperties {
		before, _, err := SourcePair(current, nil, root)
		if err != nil {
			return nil, err
		}
		intent, err := SourceIntent(*before)
		if err != nil {
			return nil, err
		}
		return schema.CaptureExternalIntent(owner, intent)
	}
	before, _, err := TablePair(current, nil, root)
	if err != nil {
		return nil, err
	}
	intent, err := TableIntent(*before)
	if err != nil {
		return nil, err
	}
	return schema.CaptureExternalIntent(owner, intent)
}

// ChangeSpecs reads an external object change: its operands as the host's
// specifications of a data source or of a table, nil for an absent one, and
// the database it names, which paths are read against.
type ChangeSpecs struct {
	Database                  string
	SourceBefore, SourceAfter *ydbdecl.ExternalDataSource
	TableBefore, TableAfter   *ydbdecl.ExternalTable
	Before, After             *protocol.Envelope
}

// ReadChange reads a normalized external object change.
func ReadChange(change *protocol.Envelope) (ChangeSpecs, error) {
	before, after, database, err := schema.ExternalOperands(change)
	if err != nil {
		return ChangeSpecs{}, err
	}
	result := ChangeSpecs{Database: database, Before: before, After: after}
	if change.GetKind() == schema.ExternalSourceChange {
		result.SourceBefore, result.SourceAfter, err = SourcePair(before, after, database)
	} else {
		result.TableBefore, result.TableAfter, err = TablePair(before, after, database)
	}
	return result, err
}
