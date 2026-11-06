// Package authored keeps the text the provider writes for an operator apart
// from the text of the errors it receives. A host that negotiates
// errors.reasons prints a failed call's reason after its gRPC code; it never
// prints the status message. An Error is such a reason: the provider wrote
// it, it names what failed and why, and it never quotes statement text, a
// value, an address or a credential. A driver's error is never one, since YDB
// can quote any of those, so it reaches the host as its code alone.
package authored

import (
	"errors"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error is a failure in words the provider wrote for the operator.
type Error struct {
	text  string
	cause error
}

// New returns a reason. text is one sentence the provider wrote; a name it
// carries is the name of a subject, never a value.
func New(text string) error { return &Error{text: text} }

// Wrap returns a reason that also matches cause with errors.Is, such as a
// sentinel a caller branches on. cause's text is not part of the reason.
func Wrap(cause error, text string) error { return &Error{text: text, cause: cause} }

// Error returns the reason's text.
func (e *Error) Error() string { return e.text }

// Unwrap returns the cause Wrap was given.
func (e *Error) Unwrap() error { return e.cause }

// Status returns the error a service returns for err with code. Its message
// is err's text, which no host prints. When err holds an Error, the status
// also carries that Error's text as the reason, and only that text: what an
// outer error added around it stays in the message.
func Status(code codes.Code, err error) error {
	var written *Error
	if !errors.As(err, &written) {
		return status.Error(code, err.Error())
	}
	reasoned, detailErr := status.New(code, err.Error()).WithDetails(&protocol.ProviderDiagnostic{Reason: written.text})
	if detailErr != nil {
		// WithDetails fails only for codes.OK, which no caller passes, or for
		// a message it cannot marshal, which a ProviderDiagnostic is not.
		return status.Error(code, err.Error())
	}
	return reasoned.Err()
}

// Valued is the refusal of a declaration whose text quotes a value the
// declaration wrote, such as an interval YDB does not take. Unquoted says the
// same without the value: the attribute and what it takes.
type Valued interface {
	error
	Unquoted() string
}

// Declared returns err, the refusal of a source declaration, with the reason a
// host prints. The reason is err's whole text: the words a decoder wrapped
// around the refusal, which name the declared object by its key or its path
// and never quote a value, and the refusal at the end of the chain, which is
// either an Error or a Valued error written without its value.
//
// Any other refusal, such as one that quotes a declaration's text or one
// that joins two errors, is returned as it is, and a host prints its code
// alone. A reason too long or not on one line, which a host does not show, is
// the refusal at the end of the chain alone, without the words around it.
func Declared(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	for current := err; current != nil; current = errors.Unwrap(current) {
		var leaf string
		//nolint:errorlint // the loop walks the chain one wrapping at a time; errors.As would reach into a joined error, whose other part can quote a driver's text.
		switch refusal := current.(type) {
		case *Error:
			leaf = refusal.text
		case Valued:
			quoted, unquoted := refusal.Error(), refusal.Unquoted()
			if !strings.HasSuffix(text, quoted) {
				return err
			}
			text, leaf = strings.TrimSuffix(text, quoted)+unquoted, unquoted
		default:
			continue
		}
		for _, reason := range []string{text, leaf} {
			if provider.ValidateDiagnostic(&protocol.ProviderDiagnostic{Reason: reason}) == nil {
				return &declared{err: err, reason: &Error{text: reason}}
			}
		}
		return err
	}
	return err
}

// declared is a refusal joined to the reason Declared wrote for it.
type declared struct {
	err    error
	reason *Error
}

// Error returns the refusal's own text.
func (d *declared) Error() string { return d.err.Error() }

// Unwrap returns the reason first, so that Status finds it before any reason
// the refusal holds, and then the refusal.
func (d *declared) Unwrap() []error { return []error{d.reason, d.err} }
