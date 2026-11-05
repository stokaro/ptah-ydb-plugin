package nativeapi

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Topic_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Topic owns the precondition, the single dispatch and the confirmation of
// one topic operation. A topic changes through YQL, as the host's embedded
// engine changes one, and is read through the topic service.
type Topic struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	Topics Ydb_Topic_V1.TopicServiceClient
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
}

// NewTopic binds the session's native clients and statement runner.
func NewTopic(driver *ydb.Driver, execute func(context.Context, string) error) *Topic {
	conn := ydb.GRPCConn(driver)
	return &Topic{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(conn), Topics: Ydb_Topic_V1.NewTopicServiceClient(conn), Execute: execute}
}

// Apply verifies that the topic is as the plan read it, or absent for a
// creation, runs the operation's statements one query each, and reads the
// result back. Dispatched is true as soon as a statement was sent. A failed
// statement or an unconfirmed read leaves the outcome uncertain; nothing is
// retried. YDB has no atomic schema compare-and-set, so another writer can
// change the topic between the check and the statement.
func (t *Topic) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	normalized, err := schema.NormalizeTopicOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(normalized.NormalizedOwner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("topic operation targets another database")
	}
	owner.Database = database
	if err := schema.ValidateNativeOwner(owner); err != nil {
		return false, err
	}
	change := schema.TopicOperationChange(normalized.Value)
	before, _, err := schema.TopicOperands(change)
	if err != nil {
		return false, err
	}
	// The consumers a transfer reads the topic through are the transfer's,
	// and the operation neither checks nor confirms them: the transfer's drop,
	// which the plan may run first, takes the one YDB made for it.
	var transfers []string
	if before != nil {
		observed, err := schema.ObservedTopic(before)
		if err != nil {
			return false, err
		}
		transfers = topics.TransferConsumers(observed)
		if before, err = withoutConsumers(before, database, transfers); err != nil {
			return false, err
		}
	}
	previous, desired, err := schema.TopicSpecs(change)
	if err != nil {
		return false, err
	}
	display := topics.Display(owner.Schema, owner.Name)
	current, err := t.read(ctx, owner)
	if err != nil {
		return false, refused("read topic "+display, err)
	}
	if current, err = withoutConsumers(current, database, transfers); err != nil {
		return false, err
	}
	if (before == nil) != (current == nil) || (before != nil && !bytes.Equal(before.Payload, current.Payload)) {
		return false, &Refusal{Err: fmt.Errorf("topic %s changed since the plan read it", display)}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	absolute := path.Join(database, owner.Schema, owner.Name)
	for _, statement := range topics.Statements(absolute, previous, desired) {
		if err := t.Execute(ctx, statement); err != nil {
			return true, err
		}
	}
	observed, err := t.read(ctx, owner)
	if err != nil {
		return true, err
	}
	if observed, err = withoutConsumers(observed, database, transfers); err != nil {
		return true, err
	}
	if desired != nil {
		kept := *desired
		kept.Consumers = slices.DeleteFunc(slices.Clone(desired.Consumers), func(consumer ydbdecl.Consumer) bool { return slices.Contains(transfers, consumer.Name) })
		desired = &kept
	}
	return true, confirmTopic(before, desired, observed)
}

// withoutConsumers is value, a topic read in database or nil for none,
// without the consumers names names and without a transfer's mark on the rest;
// see [topics.WithoutConsumers]. A plan names the topic without its database.
func withoutConsumers(value *protocol.Envelope, database string, names []string) (*protocol.Envelope, error) {
	if value == nil {
		return nil, nil
	}
	observed, err := schema.ObservedTopic(value)
	if err != nil {
		return nil, err
	}
	owner := proto.CloneOf(value.GetOwner())
	if owner.Database == "" {
		owner.Database = database
	}
	return schema.CaptureObject(owner, topics.WithoutConsumers(observed, names))
}

// read lists the topic's directory and describes the topic, or returns nil
// where the directory holds no entry of its name, or does not exist; see
// [ListDirectory].
func (t *Topic) read(ctx context.Context, owner *protocol.Reference) (*protocol.Envelope, error) {
	directory := path.Join(owner.Database, owner.Schema)
	listing, err := ListDirectory(ctx, SchemeLister(t.Scheme), owner.Database, owner.Schema)
	if err != nil {
		return nil, err
	}
	if listing.GetSelf().GetName() != path.Base(directory) || (listing.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE && listing.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY) || len(listing.Children) > 4096 {
		return nil, fmt.Errorf("invalid YDB topic namespace")
	}
	found := false
	for _, entry := range listing.Children {
		if entry.GetName() != owner.Name {
			continue
		}
		if found || entry.GetType() != Ydb_Scheme.Entry_TOPIC {
			return nil, fmt.Errorf("topic path has a different or ambiguous occupant")
		}
		found = true
	}
	if !found {
		return nil, nil
	}
	described, err := t.Topics.DescribeTopic(ctx, &Ydb_Topic.DescribeTopicRequest{Path: path.Join(directory, owner.Name)})
	if err != nil {
		return nil, fmt.Errorf("describe YDB topic: transport failed")
	}
	if err := Status(described.GetOperation()); err != nil {
		return nil, err
	}
	payload := described.GetOperation().GetResult()
	if !payload.MessageIs(&Ydb_Topic.DescribeTopicResult{}) {
		return nil, fmt.Errorf("invalid YDB topic response type")
	}
	var result native.DescribeTopicResult
	if err := proto.Unmarshal(payload.GetValue(), &result); err != nil {
		return nil, fmt.Errorf("invalid YDB topic response")
	}
	if result.GetSelf().GetName() != owner.Name || result.GetSelf().GetType() != Ydb_Scheme.Entry_TOPIC {
		return nil, fmt.Errorf("YDB topic observation has a different identity")
	}
	return schema.CaptureObject(owner, &result)
}

// confirmTopic requires the topic read back to be the one the operation
// asked for: absent after a drop, and otherwise holding the settings and the
// consumers the declaration resolves to, with the access an alteration found.
func confirmTopic(before *protocol.Envelope, desired *ydbdecl.Topic, current *protocol.Envelope) error {
	if desired == nil {
		if current != nil {
			return fmt.Errorf("topic removal was not confirmed")
		}
		return nil
	}
	if current == nil {
		return fmt.Errorf("topic result is absent")
	}
	observed, err := schema.ObservedTopic(current)
	if err != nil {
		return err
	}
	spec, err := topics.FromObserved(observed)
	if err != nil {
		return err
	}
	if !topics.Equal(*desired, spec) {
		return fmt.Errorf("topic result differs from the requested settings or consumers")
	}
	if before != nil {
		old, err := schema.ObservedTopic(before)
		if err != nil {
			return err
		}
		if !proto.Equal(old.GetSelf(), observed.GetSelf()) {
			return fmt.Errorf("topic access metadata changed during alteration")
		}
	}
	return nil
}
