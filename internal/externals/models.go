package externals

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Display names the external object an owner names by its path, for
// messages.
func Display(owner *protocol.Reference) string {
	return ydbdecl.ExternalDisplay(owner.GetSchema(), owner.GetName())
}

// SourcePair reads a read data source and its declaration, either nil, as
// the host's specifications, with paths relative to root.
func SourcePair(current, desired *protocol.Envelope, root string) (before, after *ydbdecl.ExternalDataSource, err error) {
	before, err = readSpec(current, "external data source", schema.ObservedExternalSource, SourceFromObserved)
	if err != nil {
		return nil, nil, err
	}
	after, err = readSpec(desired, "external data source", schema.DesiredExternalSource, SourceFromIntent)
	if err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

// TablePair reads a read external table and its declaration, either nil, as
// the host's specifications, with paths relative to root.
func TablePair(current, desired *protocol.Envelope, root string) (before, after *ydbdecl.ExternalTable, err error) {
	before, err = readSpec(current, "external table", schema.ObservedExternalTable, TableFromObserved)
	if err != nil {
		return nil, nil, err
	}
	after, err = readSpec(desired, "external table", schema.DesiredExternalTable, TableFromIntent)
	if err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

// readSpec reads one side of a pair, or nil for a nil value: decode reads
// the envelope, and convert turns the result into a specification against
// the value's interpretation root. A decode error is returned as it is; a
// conversion error names the object as noun and its owner.
func readSpec[V, S any](
	value *protocol.Envelope,
	noun string,
	decode func(*protocol.Envelope) (V, error),
	convert func(V, string) (S, error),
) (*S, error) {
	if value == nil {
		return nil, nil
	}
	decoded, err := decode(value)
	if err != nil {
		return nil, err
	}
	spec, err := convert(decoded, schema.InterpretationRoot(value))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", noun, Display(value.GetOwner()), err)
	}
	return &spec, nil
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
	parts, err := schema.ExternalOperands(change)
	if err != nil {
		return ChangeSpecs{}, err
	}
	result := ChangeSpecs{Database: parts.Database, Before: parts.Before, After: parts.After}
	if change.GetKind() == schema.ExternalSourceChange {
		result.SourceBefore, result.SourceAfter, err = SourcePair(parts.Before, parts.After, parts.Database)
	} else {
		result.TableBefore, result.TableAfter, err = TablePair(parts.Before, parts.After, parts.Database)
	}
	return result, err
}
