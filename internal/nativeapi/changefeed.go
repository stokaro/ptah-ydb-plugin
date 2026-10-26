package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Topic_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// ChangefeedTable describes a row table, its changefeeds among the rest.
type ChangefeedTable interface {
	Describe(context.Context, string) (*native.DescribeTableResult, error)
	Close() error
}

// Changefeed owns the precondition, the dispatch and the confirmation of the
// changefeed operation of one table. A changefeed changes through YQL, as the
// host's embedded engine changes one, and is read through the table service
// and, for its topic, the topic service.
type Changefeed struct {
	// Open opens a table session for the reads.
	Open   func(context.Context) (ChangefeedTable, error)
	Topics Ydb_Topic_V1.TopicServiceClient
	// Execute runs one schema statement through the query service, once.
	Execute func(context.Context, string) error
}

// NewChangefeed binds the session's native clients and statement runner.
func NewChangefeed(driver *ydb.Driver, execute func(context.Context, string) error) *Changefeed {
	return &Changefeed{Open: func(ctx context.Context) (ChangefeedTable, error) { return OpenTable(ctx, driver) },
		Topics: Ydb_Topic_V1.NewTopicServiceClient(ydb.GRPCConn(driver)), Execute: execute}
}

// Apply verifies that each changefeed is as the plan read it, or absent for an
// addition, runs the operation's statements one query each, and reads the
// result back. Dispatched is true as soon as a statement was sent. A failed
// statement or an unconfirmed read leaves the outcome uncertain; nothing is
// retried. YDB has no atomic schema compare-and-set, so another writer can
// change a changefeed between the check and the statement.
func (c *Changefeed) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	normalized, err := schema.NormalizeChangefeedOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(normalized.NormalizedOwner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("changefeed operation targets another database")
	}
	owner.Database = database
	changes, err := schema.DecodeChangefeedOperation(normalized.Value)
	if err != nil {
		return false, err
	}
	if !validTablePath(owner) {
		return false, fmt.Errorf("invalid YDB changefeed table path")
	}
	table := path.Join(database, owner.Schema, owner.Name)
	label := joinedName(owner)
	statements, err := changefeeds.Statements(table, changes)
	if err != nil {
		return false, err
	}
	session, err := c.Open(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	current, err := c.read(ctx, session, table)
	if err != nil {
		return false, err
	}
	for _, change := range changes {
		if err := precondition(label, change, current[change.Name]); err != nil {
			return false, err
		}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, statement := range statements {
		if err := c.Execute(ctx, statement); err != nil {
			return true, err
		}
	}
	observed, err := c.read(ctx, session, table)
	if err != nil {
		return true, err
	}
	for _, change := range changes {
		if err := confirmChangefeed(label, change, observed[change.Name]); err != nil {
			return true, err
		}
	}
	return true, nil
}

// validTablePath reports whether the owner names a table by path components
// path.Join keeps as written: none empty, none a dot or two, and a name with no
// slash.
func validTablePath(owner *protocol.Reference) bool {
	parts := []string{owner.GetName()}
	if owner.GetSchema() != "" {
		parts = append(parts, strings.Split(owner.GetSchema(), "/")...)
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			return false
		}
	}
	return true
}

// joinedName is the table's path below the database, as plans name it.
func joinedName(owner *protocol.Reference) string {
	if owner.GetSchema() == "" {
		return owner.GetName()
	}
	return owner.GetSchema() + "/" + owner.GetName()
}

// read describes the table and returns each changefeed it holds with its
// topic, by name, projected as inspection projects them.
func (c *Changefeed) read(ctx context.Context, session ChangefeedTable, table string) (map[string]*native.ObservedChangefeed, error) {
	described, err := session.Describe(ctx, table)
	if err != nil {
		return nil, err
	}
	if described.GetSelf().GetType() != Ydb_Scheme.Entry_TABLE {
		return nil, fmt.Errorf("YDB changefeed table is not a row table")
	}
	if err := schema.ValidateChangefeeds(described.GetChangefeeds()); err != nil {
		return nil, err
	}
	result := make(map[string]*native.ObservedChangefeed, len(described.GetChangefeeds()))
	for _, description := range described.GetChangefeeds() {
		response, err := c.Topics.DescribeTopic(ctx, &Ydb_Topic.DescribeTopicRequest{Path: path.Join(table, description.GetName())})
		if err != nil {
			return nil, fmt.Errorf("describe YDB changefeed topic: transport failed")
		}
		if err := Status(response.GetOperation()); err != nil {
			return nil, err
		}
		payload := response.GetOperation().GetResult()
		if !payload.MessageIs(&Ydb_Topic.DescribeTopicResult{}) {
			return nil, fmt.Errorf("invalid YDB topic response type")
		}
		var topic native.DescribeTopicResult
		if err := proto.Unmarshal(payload.GetValue(), &topic); err != nil {
			return nil, fmt.Errorf("invalid YDB topic response")
		}
		if err := schema.Known(&topic); err != nil {
			return nil, err
		}
		feed, err := schema.ProjectChangefeed(&native.ObservedChangefeed{Description: description, Topic: &topic})
		if err != nil {
			return nil, err
		}
		result[description.GetName()] = feed
	}
	return result, nil
}

// precondition requires the changefeed to be what the plan read: absent for
// an addition, and for a removal or a topic change the changefeed the plan's
// operand describes, as the host's specification reads both.
func precondition(table string, step changefeeds.Change, current *native.ObservedChangefeed) error {
	subject := changefeeds.Subject(table, step.Name)
	if step.Before == nil {
		if current != nil {
			return fmt.Errorf("%s exists, and the plan read none", subject)
		}
		return nil
	}
	if current == nil {
		return fmt.Errorf("%s is gone since the plan read it", subject)
	}
	held, err := changefeeds.FromObserved(current)
	if err != nil {
		return fmt.Errorf("%s: %w", subject, err)
	}
	planned, err := changefeeds.FromObserved(step.Before)
	if err != nil {
		return fmt.Errorf("%s: %w", subject, err)
	}
	if held.Replicated || !changefeeds.Equal(planned, held) {
		return fmt.Errorf("%s changed since the plan read it", subject)
	}
	return nil
}

// confirmChangefeed requires the changefeed read back to be the one the step
// asked for: absent after a removal, and otherwise the declared changefeed
// with its topic's retention and consumers.
func confirmChangefeed(table string, step changefeeds.Change, observed *native.ObservedChangefeed) error {
	subject := changefeeds.Subject(table, step.Name)
	if step.After == nil {
		if observed != nil {
			return fmt.Errorf("the removal of %s was not confirmed", subject)
		}
		return nil
	}
	if observed == nil {
		return fmt.Errorf("%s is absent after the operation", subject)
	}
	held, err := changefeeds.FromObserved(observed)
	if err != nil {
		return fmt.Errorf("%s: %w", subject, err)
	}
	desired, err := changefeeds.FromIntent(step.After)
	if err != nil {
		return err
	}
	if !changefeeds.Equal(desired, held) {
		return fmt.Errorf("%s differs from the requested options, retention or consumers", subject)
	}
	return nil
}
