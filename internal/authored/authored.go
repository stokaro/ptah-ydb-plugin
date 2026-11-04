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

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
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
