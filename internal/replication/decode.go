package replication

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_Replication"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ReplicaAttribute is the attribute YDB gives a table an async replication
// writes, measured on 25.1.4.7 and 26.2.1.14: `__async_replica = true` while
// the replication runs, kept when the replication is dropped without being
// failed over, and removed by the failover (`SET (STATE = 'DONE')`), after
// which the table is an ordinary one.
const ReplicaAttribute = "__async_replica"

// IsReplica reports a table an async replication writes or wrote without
// failing over, which YDB keeps read-only: every write answers `Can't execute
// write tx at replicated table` and every ALTER `path is an async replica
// table`.
func IsReplica(attributes map[string]string) bool {
	return attributes[ReplicaAttribute] == "true"
}

// DecodeReplication reads a replication's description as the connection,
// items and consistency it holds, and its state, with every path of this
// database read relative to root, the database root the reader reads.
//
// A field the pinned protocol buffers do not know is refused by number, and
// so is a credential kind other than a token secret or a user's password
// secret: read as no credential, it would be left out of a declaration made
// from the read. Each item's source is read relative to the source database
// and its target relative to root, where each lies under it.
func DecodeReplication(root string, described *Ydb_Replication.DescribeReplicationResult) (ReplicationSpec, string, error) {
	if err := refuseUnknownFields(map[string]protoreflect.ProtoMessage{
		"its description":     described,
		"its connection":      described.GetConnectionParams(),
		"its global settings": described.GetGlobalConsistency(),
	}); err != nil {
		return ReplicationSpec{}, "", err
	}
	connection, err := decodeConnection(root, described.GetConnectionParams())
	if err != nil {
		return ReplicationSpec{}, "", err
	}
	spec := ReplicationSpec{Connection: connection}
	database := described.GetConnectionParams().GetDatabase()
	for _, item := range described.GetItems() {
		spec.Items = append(spec.Items, Item{
			Source: SourceKey(item.GetSourcePath(), database),
			Target: relative(root, item.GetDestinationPath()),
		})
	}
	if global := described.GetGlobalConsistency(); global != nil {
		spec.ConsistencyLevel = ConsistencyGlobal
		millis, whole := durationMillis(global.GetCommitInterval())
		if !whole {
			return ReplicationSpec{}, "", errors.New("its commit interval holds a fraction of a millisecond, " +
				"which no declaration reads")
		}
		if millis != DefaultCommitIntervalMillis {
			spec.CommitInterval = FormatMillis(millis)
		}
	}
	state := replicationState(described)
	if err := ValidateObservedReplication(spec, state); err != nil {
		return ReplicationSpec{}, "", err
	}
	return spec, state, nil
}

// DecodeTransfer reads a transfer's description as its connection, source,
// target, lambda, consumer and batch settings, and its state.
//
// The lambda is read as written: YDB stores it as `$__ydb_transfer_lambda =
// <lambda>;`, measured on 25.1.4.7 with the flag on and 26.2.1.14 alike, and
// a lambda in that form reads back as the text between. One stored in any
// other form -- written through a named lambda, or under a PRAGMA, each of
// which YDB keeps in the text before it -- is read whole, so it compares
// unequal to any inline declaration and a plan sets the declared one.
func DecodeTransfer(root string, described *Ydb_Replication.DescribeTransferResult) (TransferSpec, string, error) {
	if err := refuseUnknownFields(map[string]protoreflect.ProtoMessage{
		"its description":    described,
		"its connection":     described.GetConnectionParams(),
		"its batch settings": described.GetBatchSettings(),
	}); err != nil {
		return TransferSpec{}, "", err
	}
	connection, err := decodeConnection(root, described.GetConnectionParams())
	if err != nil {
		return TransferSpec{}, "", err
	}
	spec := TransferSpec{
		Connection: connection,
		Target:     relative(root, described.GetDestinationPath()),
		Lambda:     storedLambda(described.GetTransformationLambda()),
		Consumer:   described.GetConsumerName(),
	}
	if connection.ConnectionString != "" {
		spec.Source = SourceKey(described.GetSourcePath(), described.GetConnectionParams().GetDatabase())
	} else {
		spec.Source = localSource(root, described.GetSourcePath())
	}
	batch := described.GetBatchSettings()
	if size := batch.GetSizeBytes(); size != 0 && size != DefaultBatchSizeBytes {
		spec.BatchSizeBytes = size
	}
	if flush := batch.GetFlushInterval(); flush != nil {
		millis, whole := durationMillis(flush)
		if !whole || millis%1000 != 0 {
			return TransferSpec{}, "", errors.New("its flush interval holds a fraction of a second, which no " +
				"declaration reads")
		}
		if seconds := millis / 1000; seconds != DefaultFlushIntervalSeconds {
			spec.FlushInterval = FormatMillis(millis)
		}
	}
	state := transferState(described)
	if err := ValidateObservedTransfer(spec, state); err != nil {
		return TransferSpec{}, "", err
	}
	return spec, state, nil
}

// decodeConnection reads a connection: its string rebuilt from the endpoint,
// the database and TLS, as YDB reports them, and its credential by the secret
// it names. A connection with no endpoint is a transfer's of a topic in its
// own database, which names none (measured: `grpc:///?database=`).
//
// A secret named by its path reads back as the secret's absolute path, and a
// secret named by name as written, so a leading slash tells the two apart: a
// path under root is read relative to it, as a declaration writes it.
func decodeConnection(root string, params *Ydb_Replication.ConnectionParams) (Connection, error) {
	var connection Connection
	if params.GetEndpoint() != "" {
		connection.ConnectionString = Endpoint{
			Secure:   params.GetEnableSsl(),
			Address:  params.GetEndpoint(),
			Database: params.GetDatabase(),
		}.String()
	}
	for _, credential := range []protoreflect.ProtoMessage{params.GetStaticCredentials(), params.GetOauth()} {
		if err := refuseUnknownFields(map[string]protoreflect.ProtoMessage{"its credential": credential}); err != nil {
			return Connection{}, err
		}
	}
	switch credentials := params.GetCredentials().(type) {
	case nil:
	case *Ydb_Replication.ConnectionParams_Oauth:
		secret := credentials.Oauth.GetTokenSecretName()
		if strings.HasPrefix(secret, "/") {
			connection.TokenSecretPath = relative(root, secret)
		} else {
			connection.TokenSecretName = secret
		}
	case *Ydb_Replication.ConnectionParams_StaticCredentials_:
		connection.User = credentials.StaticCredentials.GetUser()
		secret := credentials.StaticCredentials.GetPasswordSecretName()
		switch {
		case strings.HasPrefix(secret, "/"):
			connection.PasswordSecretPath = relative(root, secret)
		case secret != "":
			connection.PasswordSecretName = secret
		}
	default:
		return Connection{}, fmt.Errorf("its connection signs in with %T, which the provider does not read", credentials)
	}
	return connection, nil
}

// transferLambda is the form YDB stores an inline lambda in.
var transferLambda = regexp.MustCompile(`(?s)\A\$__ydb_transfer_lambda = (.*);\n\z`)

// storedLambda reads the lambda out of the text YDB stores, or returns the
// text whole when it holds more than one inline lambda.
func storedLambda(stored string) string {
	if match := transferLambda.FindStringSubmatch(stored); match != nil && !strings.Contains(match[1], "$__ydb_transfer_lambda") {
		return match[1]
	}
	return stored
}

// localSource reads the source of a transfer of a topic in its own database
// relative to root.
//
// 26.1.1.22 and 26.2.1.14 keep a relative source as written, and 25.2.1.24 to
// 25.4.1.15 (and 25.1.4.7 with the flag on) keep it behind a slash: `tp`
// reads back as `/tp`. A source under root is read relative to it; any other
// with a leading slash is the older lines' spelling of a relative one.
func localSource(root, source string) string {
	if rest := relative(root, source); rest != source {
		return rest
	}
	return strings.Trim(source, "/")
}

// relative is an absolute path relative to root where it lies under it, and
// the path unchanged otherwise.
func relative(root, absolute string) string {
	trimmed := strings.TrimRight(root, "/")
	if rest, under := strings.CutPrefix(absolute, trimmed+"/"); under {
		return path.Clean(rest)
	}
	return absolute
}

// replicationState is the state a replication reports.
func replicationState(described *Ydb_Replication.DescribeReplicationResult) string {
	switch {
	case described.GetDone() != nil:
		return StateDone
	case described.GetPaused() != nil:
		return StatePaused
	case described.GetError() != nil:
		return StateError
	case described.GetRunning() != nil:
		return StateRunning
	default:
		return ""
	}
}

// transferState is the state a transfer reports.
func transferState(described *Ydb_Replication.DescribeTransferResult) string {
	switch {
	case described.GetDone() != nil:
		return StateDone
	case described.GetPaused() != nil:
		return StatePaused
	case described.GetError() != nil:
		return StateError
	case described.GetRunning() != nil:
		return StateRunning
	default:
		return ""
	}
}

// durationMillis reads a duration as whole milliseconds, and reports false for
// one holding a fraction of a millisecond.
func durationMillis(duration *durationpb.Duration) (uint64, bool) {
	if duration == nil {
		return 0, true
	}
	value := duration.AsDuration()
	if value < 0 || value%time.Millisecond != 0 {
		return 0, false
	}
	return uint64(value / time.Millisecond), true
}

// refuseUnknownFields refuses a description whose messages carry a field the
// pinned protocol buffers do not know, naming the message and the field
// numbers. A nil message carries none.
func refuseUnknownFields(messages map[string]protoreflect.ProtoMessage) error {
	subjects := make([]string, 0, len(messages))
	for subject := range messages {
		subjects = append(subjects, subject)
	}
	slices.Sort(subjects)
	for _, subject := range subjects {
		message := messages[subject]
		if message == nil || !message.ProtoReflect().IsValid() {
			continue
		}
		if numbers := unknownNumbers(message.ProtoReflect().GetUnknown()); len(numbers) > 0 {
			return fmt.Errorf("%s holds fields %s, which the provider does not read", subject, strings.Join(numbers, ", "))
		}
	}
	return nil
}

// unknownNumbers lists the field numbers raw unknown bytes hold, each once,
// in the order they appear. Bytes that do not parse are reported as such.
func unknownNumbers(raw protoreflect.RawFields) []string {
	var numbers []string
	seen := map[protowire.Number]bool{}
	for len(raw) > 0 {
		number, _, length := protowire.ConsumeField(raw)
		if length < 0 {
			return append(numbers, "that do not parse")
		}
		if !seen[number] {
			seen[number] = true
			numbers = append(numbers, strconv.Itoa(int(number)))
		}
		raw = raw[length:]
	}
	return numbers
}
