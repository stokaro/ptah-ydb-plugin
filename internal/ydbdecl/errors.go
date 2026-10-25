package ydbdecl

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// errInvalidValue is the class of a value no model of the host's YDB owner
// holds, spelled as the host spells it.
var errInvalidValue = errors.New("invalid feature value")

// The host's model kinds, which its refusal of a value names.
const (
	kindTTL               = "ptah.run/ydb/ttl"
	kindColumnFamilies    = "ptah.run/ydb/column-families"
	kindColumnStore       = "ptah.run/ydb/column-store"
	kindTablePartitioning = "ptah.run/ydb/table-partitioning"
	kindIndexPartitioning = "ptah.run/ydb/index-partitioning"
	kindVectorIndex       = "ptah.run/ydb/vector-index"
	kindChangefeed        = "ptah.run/ydb/changefeed"
	kindTopic             = "ptah.run/ydb/topic"
	kindCoordinationNode  = "ptah.run/ydb/coordination-node"
	kindExternalSource    = "ptah.run/ydb/external-data-source"
	kindExternalTable     = "ptah.run/ydb/external-table"
)

// modelError is the host's refusal of a desired value of a model.
type modelError struct {
	kind    string
	message string
}

func (e *modelError) Error() string {
	return fmt.Sprintf("desired model %q: %s", e.kind, e.message)
}

// invalidModel wraps err as the host's refusal of a desired value of kind,
// and is nil for a nil err.
func invalidModel(kind string, err error) error {
	if err == nil {
		return nil
	}
	return &modelError{kind: kind, message: err.Error()}
}

// validText refuses invalid UTF-8 and a NUL byte, naming the value by field.
// An empty value is valid text.
func validText(field, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", errInvalidValue, field)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%w: %s contains a NUL byte", errInvalidValue, field)
	}
	return nil
}
