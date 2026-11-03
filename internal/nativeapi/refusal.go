package nativeapi

import "fmt"

// Refusal is why a unit was refused before anything was sent: a statement and
// what the server holds disagree, the object changed since the plan read it,
// or the read its precondition needs did not succeed. Its text is Ptah's own
// and names the object by its path. It carries no credential, no declared
// value and no server text beyond the name of a status, so execution reports
// it as the unit's message.
type Refusal struct{ Err error }

func (r *Refusal) Error() string { return r.Err.Error() }
func (r *Refusal) Unwrap() error { return r.Err }

// refused is a Refusal of the object subject names, for the reason err gives.
// err must be one whose text is Ptah's own.
func refused(subject string, err error) error {
	return &Refusal{Err: fmt.Errorf("%s: %w", subject, err)}
}
