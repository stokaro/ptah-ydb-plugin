package replication

import (
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// Path quotes one exact scheme path. A dot is part of its leaf name;
// directories are separated by slashes.
func Path(name string) string { return ydbtype.QuoteIdentifier(name) }

// CreateReplicationStatement writes what creates the replication name as
// spec declares it: its items in the order declared, its connection, its
// credential and the consistency settings it names. A setting spec leaves out
// is left to YDB, which gives it the value [CompareReplication] reads the
// declaration with.
func CreateReplicationStatement(name string, spec ReplicationSpec) string {
	items := make([]string, len(spec.Items))
	for i, item := range spec.Items {
		items[i] = quotePath(item.Source) + " AS " + quotePath(item.Target)
	}
	settings := append(connectionStringSettings(spec.Connection), credentialSettings(spec.Connection)...)
	if spec.ConsistencyLevel != "" {
		settings = append(settings, "CONSISTENCY_LEVEL = "+quoteString(strings.ToUpper(spec.ConsistencyLevel)))
	}
	if spec.CommitInterval != "" {
		millis, _ := CommitIntervalMillis(spec.CommitInterval)
		settings = append(settings, "COMMIT_INTERVAL = "+intervalLiteral(FormatMillis(millis)))
	}
	return "CREATE ASYNC REPLICATION " + Path(name) + " FOR " + strings.Join(items, ", ") +
		" WITH (" + strings.Join(settings, ", ") + ");"
}

// AlterReplicationStatement writes what moves the connection and the
// credential of the paused replication name from previous to desired, and ""
// when they are the same.
//
// It names the connection string when that differs, and the whole credential
// whenever any part of it differs: the user with the password secret, or the
// token secret. Measured on 25.1.4.7 and 26.2.1.14, a SET naming one of them
// keeps the others, so naming the whole group is not needed for the change to
// take, and it keeps the statement saying what the replication holds after
// it.
func AlterReplicationStatement(name string, desired, previous ReplicationSpec) string {
	settings := alteredConnection(desired.Connection, previous.Connection)
	if len(settings) == 0 {
		return ""
	}
	return "ALTER ASYNC REPLICATION " + Path(name) + " SET (" + strings.Join(settings, ", ") + ");"
}

// DropReplicationStatement writes what drops the replication name. With
// cascade it drops the replica tables too; without it they stay, writable
// only where the replication was failed over first: measured on 25.1.4.7 and
// 26.2.1.14, the replica of a running replication dropped without CASCADE
// refuses every write and every ALTER for good (`path is an async replica
// table`).
func DropReplicationStatement(name string, cascade bool) string {
	return "DROP ASYNC REPLICATION " + Path(name) + cascadeClause[cascade] + ";"
}

// cascadeClause spells a drop's choice about the replica tables. It is a
// lookup rather than a branch because the choice selects a word, not a code
// path.
var cascadeClause = map[bool]string{true: " CASCADE", false: ""}

// CreateTransferStatement writes what creates the transfer name as spec
// declares it, the lambda as written.
func CreateTransferStatement(name string, spec TransferSpec) string {
	var b strings.Builder
	b.WriteString("CREATE TRANSFER " + Path(name) + " FROM " + quotePath(spec.Source) + " TO " +
		quotePath(spec.Target) + " USING " + strings.TrimSpace(spec.Lambda))
	settings := append(connectionStringSettings(spec.Connection), credentialSettings(spec.Connection)...)
	if spec.Consumer != "" {
		settings = append(settings, "CONSUMER = "+quoteString(spec.Consumer))
	}
	if spec.BatchSizeBytes != 0 {
		settings = append(settings, "BATCH_SIZE_BYTES = "+strconv.FormatUint(spec.BatchSizeBytes, 10))
	}
	if spec.FlushInterval != "" {
		seconds, _ := FlushIntervalSeconds(spec.FlushInterval)
		settings = append(settings, "FLUSH_INTERVAL = "+intervalLiteral(FormatMillis(seconds*1000)))
	}
	if len(settings) > 0 {
		b.WriteString(" WITH (" + strings.Join(settings, ", ") + ")")
	}
	return b.String() + ";"
}

// AlterTransferStatement writes what moves the transfer name from previous to
// desired in place, and "" when nothing it can change differs: the lambda,
// both batch settings whenever either differs, and the connection and the
// credential as [AlterReplicationStatement] writes them. Measured on 26.2.1.14,
// one ALTER TRANSFER takes `SET USING ..., SET (...)`, and a SET naming one
// batch setting keeps the other.
func AlterTransferStatement(name string, desired, previous TransferSpec) string {
	changes := CompareTransfer(desired, previous)
	var actions []string
	if changes.Lambda {
		actions = append(actions, "SET USING "+strings.TrimSpace(desired.Lambda))
	}
	settings := alteredConnection(desired.Connection, previous.Connection)
	if changes.Batch {
		settings = append(settings,
			"BATCH_SIZE_BYTES = "+strconv.FormatUint(batchSize(desired), 10),
			"FLUSH_INTERVAL = "+intervalLiteral(FormatMillis(flushSeconds(desired)*1000)))
	}
	if len(settings) > 0 {
		actions = append(actions, "SET ("+strings.Join(settings, ", ")+")")
	}
	if len(actions) == 0 {
		return ""
	}
	return "ALTER TRANSFER " + Path(name) + " " + strings.Join(actions, ", ") + ";"
}

// DropTransferStatement writes what drops the transfer name. YDB drops the
// consumer it created for the transfer and keeps one the transfer was given,
// measured on 26.2.1.14; the topic and the table stay.
func DropTransferStatement(name string) string {
	return "DROP TRANSFER " + Path(name) + ";"
}

// alteredConnection writes the SET settings that move a connection from
// previous to desired.
func alteredConnection(desired, previous Connection) []string {
	var settings []string
	if CanonicalConnectionString(desired.ConnectionString) != CanonicalConnectionString(previous.ConnectionString) {
		settings = append(settings, connectionStringSettings(desired)...)
	}
	if !credentialsEqual(desired, previous) {
		settings = append(settings, credentialSettings(desired)...)
	}
	return settings
}

// connectionStringSettings writes a connection's string as a setting, or none
// where the connection names none.
func connectionStringSettings(connection Connection) []string {
	if connection.ConnectionString == "" {
		return nil
	}
	return []string{"CONNECTION_STRING = " + quoteString(connection.ConnectionString)}
}

// credentialSettings writes a connection's credential as its settings: the
// token secret, or the user with the password secret, each by name or by path.
func credentialSettings(connection Connection) []string {
	var settings []string
	switch {
	case connection.TokenSecretName != "":
		settings = append(settings, "TOKEN_SECRET_NAME = "+quoteString(connection.TokenSecretName))
	case connection.TokenSecretPath != "":
		settings = append(settings, "TOKEN_SECRET_PATH = "+quoteString(connection.TokenSecretPath))
	case connection.PasswordSecretName != "":
		settings = append(settings, "USER = "+quoteString(connection.User),
			"PASSWORD_SECRET_NAME = "+quoteString(connection.PasswordSecretName))
	case connection.PasswordSecretPath != "":
		settings = append(settings, "USER = "+quoteString(connection.User),
			"PASSWORD_SECRET_PATH = "+quoteString(connection.PasswordSecretPath))
	}
	return settings
}

// quotePath quotes a path as one YQL identifier, a leading slash included.
func quotePath(value string) string {
	return ydbtype.QuoteIdentifier(value)
}

// quoteString writes a YQL string literal.
func quoteString(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`) + "'"
}

// intervalLiteral writes an ISO 8601 duration as YDB's Interval literal.
func intervalLiteral(text string) string {
	return "Interval(" + quoteString(text) + ")"
}

// AbsoluteReplication is spec with every path of this database written
// under root: each item's target and a secret named by its path. A source
// belongs to the source database and keeps its spelling.
func AbsoluteReplication(root string, spec ReplicationSpec) ReplicationSpec {
	absolute := spec.Clone()
	for i := range absolute.Items {
		absolute.Items[i].Target = under(root, absolute.Items[i].Target)
	}
	absolute.Connection = absoluteSecrets(root, absolute.Connection)
	return absolute
}

// AbsoluteTransfer is spec with the table and a secret named by its path
// written under root. The topic keeps its spelling: 25.1.4.7 resolves a
// transfer's source against the database root and cannot find a topic of
// its own database named absolutely (`Discovery error: /local/t/feed:
// SCHEME_ERROR`, where the relative `t/feed` runs), so a topic of this
// database is named relative to the root the session reads, which is the
// database's.
func AbsoluteTransfer(root string, spec TransferSpec) TransferSpec {
	absolute := spec
	absolute.Target = under(root, spec.Target)
	absolute.Connection = absoluteSecrets(root, spec.Connection)
	return absolute
}

func absoluteSecrets(root string, connection Connection) Connection {
	if connection.TokenSecretPath != "" {
		connection.TokenSecretPath = under(root, connection.TokenSecretPath)
	}
	if connection.PasswordSecretPath != "" {
		connection.PasswordSecretPath = under(root, connection.PasswordSecretPath)
	}
	return connection
}

// under writes a path relative to root under it, and keeps an absolute one.
func under(root, value string) string {
	if strings.HasPrefix(value, "/") {
		return value
	}
	return strings.TrimRight(root, "/") + "/" + cleanRelative(value)
}
