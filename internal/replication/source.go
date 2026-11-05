package replication

import (
	"errors"
	"fmt"
	"maps"
	"strings"
)

// SourceKind selects whose YQL settings a desired YQL schema reads: an async
// replication's or a transfer's.
type SourceKind string

const (
	// ReplicationSource selects async replication settings.
	ReplicationSource SourceKind = "replication"
	// TransferSource selects transfer settings.
	TransferSource SourceKind = "transfer"
)

// SourceSettingType names the YQL value form a setting of kind takes in a
// desired YQL schema, as the host's YQL reader names it: "String", "Uint" or
// "Interval", or "" for a setting with no desired model. ENDPOINT and
// DATABASE spell a connection string in two parts.
func SourceSettingType(name string, kind SourceKind) string {
	switch name {
	case AttributeConnectionString, "endpoint", "database", AttributeTokenSecretName,
		AttributeTokenSecretPath, AttributeUser, AttributePasswordSecretName, AttributePasswordSecretPath:
		return "String"
	case AttributeConsistencyLevel:
		if kind == ReplicationSource {
			return "String"
		}
	case AttributeCommitInterval:
		if kind == ReplicationSource {
			return "Interval"
		}
	case AttributeConsumer:
		if kind == TransferSource {
			return "String"
		}
	case AttributeBatchSizeBytes:
		if kind == TransferSource {
			return "Uint"
		}
	case AttributeFlushInterval:
		if kind == TransferSource {
			return "Interval"
		}
	}
	return ""
}

// ParseReplicationSource reads the settings of a CREATE ASYNC REPLICATION in
// a desired YQL schema, ENDPOINT and DATABASE among them, through the rules
// the Go and YAML sources read with. The items are the caller's.
func ParseReplicationSource(settings map[string]string) (ReplicationSpec, error) {
	values, err := sourceValues(nil, settings)
	if err != nil {
		return ReplicationSpec{}, err
	}
	return ParseReplication(values)
}

// ParseTransferSource reads the settings of a CREATE TRANSFER in a desired YQL
// schema, with its FROM, TO and USING clauses as source, target and using,
// through the rules the Go and YAML sources read with.
func ParseTransferSource(settings map[string]string) (TransferSpec, error) {
	values, err := sourceValues(nil, settings)
	if err != nil {
		return TransferSpec{}, err
	}
	return ParseTransfer(values)
}

// ApplyReplicationSettings folds the SET of an ALTER ASYNC REPLICATION in a
// desired YQL schema into the replication an earlier statement declared. A
// lifecycle command, such as STATE, and a setting YDB changes in no
// replication in place are no desired change and are refused.
func ApplyReplicationSettings(previous ReplicationSpec, settings map[string]string) (ReplicationSpec, error) {
	if err := mutableSourceSettings(settings, ReplicationSource); err != nil {
		return ReplicationSpec{}, err
	}
	values, err := sourceValues(replicationValues(previous), settings)
	if err != nil {
		return ReplicationSpec{}, err
	}
	spec, err := ParseReplication(values)
	spec.Items = previous.Clone().Items
	return spec, err
}

// ApplyTransferSettings folds the SET clauses of an ALTER TRANSFER in a
// desired YQL schema, a new lambda under using among them, into the transfer
// an earlier statement declared.
func ApplyTransferSettings(previous TransferSpec, settings map[string]string) (TransferSpec, error) {
	if err := mutableSourceSettings(settings, TransferSource); err != nil {
		return TransferSpec{}, err
	}
	values, err := sourceValues(transferValues(previous), settings)
	if err != nil {
		return TransferSpec{}, err
	}
	return ParseTransfer(values)
}

// mutableSourceSettings refuses an ALTER that names nothing, or a setting the
// declaration cannot change in place.
func mutableSourceSettings(settings map[string]string, kind SourceKind) error {
	if len(settings) == 0 {
		return errors.New("ALTER needs a modeled setting")
	}
	for name := range settings {
		if (kind == TransferSource && name == AttributeUsing) ||
			(SourceSettingType(name, kind) != "" && name != AttributeConsistencyLevel && name != AttributeCommitInterval && name != AttributeConsumer) {
			continue
		}
		return fmt.Errorf("setting %q cannot change in a desired ALTER declaration", name)
	}
	return nil
}

// sourceValues writes settings over previous as the attribute values the
// parsers read. YDB replaces the credential kind when a statement switches
// between a token and a password, and keeps the settings of the password
// kind a statement leaves out.
func sourceValues(previous, settings map[string]string) (map[string]string, error) {
	values := maps.Clone(previous)
	if values == nil {
		values = make(map[string]string)
	}
	if err := sourceEndpoint(values, settings); err != nil {
		return nil, err
	}
	// A value validation refuses as empty still counts as named here.
	if _, ok := settings[AttributeTokenSecretName]; ok {
		delete(values, AttributeTokenSecretPath)
		clearPassword(values)
	}
	if _, ok := settings[AttributeTokenSecretPath]; ok {
		delete(values, AttributeTokenSecretName)
		clearPassword(values)
	}
	if _, ok := settings[AttributePasswordSecretName]; ok {
		delete(values, AttributePasswordSecretPath)
		clearToken(values)
	}
	if _, ok := settings[AttributePasswordSecretPath]; ok {
		delete(values, AttributePasswordSecretName)
		clearToken(values)
	}
	if _, ok := settings[AttributeUser]; ok {
		clearToken(values)
	}
	maps.Copy(values, settings)
	delete(values, "endpoint")
	delete(values, "database")
	return values, nil
}

func clearPassword(values map[string]string) {
	delete(values, AttributeUser)
	delete(values, AttributePasswordSecretName)
	delete(values, AttributePasswordSecretPath)
}

func clearToken(values map[string]string) {
	delete(values, AttributeTokenSecretName)
	delete(values, AttributeTokenSecretPath)
}

// sourceEndpoint writes ENDPOINT and DATABASE as the connection string they
// spell, over the one values holds. Either alone changes its part of that
// string; combined with CONNECTION_STRING it is refused.
func sourceEndpoint(values, settings map[string]string) error {
	_, endpoint := settings["endpoint"]
	_, database := settings["database"]
	if !endpoint && !database {
		return nil
	}
	if _, connection := settings[AttributeConnectionString]; connection {
		return errors.New("CONNECTION_STRING cannot be combined with ENDPOINT or DATABASE")
	}
	var parsed Endpoint
	if old := values[AttributeConnectionString]; old != "" {
		var err error
		if parsed, err = ParseConnectionString(old); err != nil {
			return err
		}
	}
	if endpoint {
		raw := settings["endpoint"]
		parsed.Secure = strings.HasPrefix(raw, "grpcs://")
		scheme := "grpc://"
		if parsed.Secure {
			scheme = "grpcs://"
		}
		parsed.Address = strings.TrimPrefix(raw, scheme)
	}
	if database, exists := settings["database"]; exists {
		parsed.Database = database
	}
	values[AttributeConnectionString] = parsed.String()
	return nil
}
