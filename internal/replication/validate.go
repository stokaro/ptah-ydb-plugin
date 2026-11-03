package replication

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func validText(name, value string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s must be valid UTF-8 without NUL", name)
	}
	return nil
}

// ValidateReplication holds spec to the rules a declaration is read with: its
// connection and consistency as [ParseReplication] takes them, and at least
// one item, each as [ParseItem] takes it, no two at one target.
func ValidateReplication(spec ReplicationSpec) error {
	if err := replicationText(spec); err != nil {
		return err
	}
	if _, err := ParseReplication(replicationValues(spec)); err != nil {
		return err
	}
	return ValidateReplicationItems(spec.Items)
}

// ValidateTransfer holds spec to the rules [ParseTransfer] reads a
// declaration with.
func ValidateTransfer(spec TransferSpec) error {
	if err := transferText(spec); err != nil {
		return err
	}
	_, err := ParseTransfer(transferValues(spec))
	return err
}

// validState refuses a state the reader does not report.
func validState(state string) error {
	switch state {
	case "", StateRunning, StatePaused, StateDone, StateError:
		return nil
	}
	return fmt.Errorf("unknown replication state %q", state)
}

// connectionText refuses a connection field that is not valid text.
func connectionText(connection Connection) error {
	for _, field := range []struct{ name, value string }{
		{AttributeConnectionString, connection.ConnectionString},
		{AttributeTokenSecretName, connection.TokenSecretName},
		{AttributeTokenSecretPath, connection.TokenSecretPath},
		{AttributeUser, connection.User},
		{AttributePasswordSecretName, connection.PasswordSecretName},
		{AttributePasswordSecretPath, connection.PasswordSecretPath},
	} {
		if err := validText(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

// replicationText refuses a replication field that is not valid text.
func replicationText(spec ReplicationSpec) error {
	if err := connectionText(spec.Connection); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{AttributeConsistencyLevel, spec.ConsistencyLevel},
		{AttributeCommitInterval, spec.CommitInterval},
	} {
		if err := validText(field.name, field.value); err != nil {
			return err
		}
	}
	for _, item := range spec.Items {
		if err := validText("an item's "+AttributeSource, item.Source); err != nil {
			return err
		}
		if err := validText("an item's "+AttributeTarget, item.Target); err != nil {
			return err
		}
	}
	return nil
}

// transferText refuses a transfer field that is not valid text.
func transferText(spec TransferSpec) error {
	if err := connectionText(spec.Connection); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{AttributeSource, spec.Source},
		{AttributeTarget, spec.Target},
		{AttributeUsing, spec.Lambda},
		{AttributeConsumer, spec.Consumer},
		{AttributeFlushInterval, spec.FlushInterval},
	} {
		if err := validText(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}
