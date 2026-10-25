package nativeapi

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Coordination owns raw native calls without mutation retry wrappers.
// The injectable clients also let tests distinguish preflight from dispatch.
type Coordination struct {
	Scheme Ydb_Scheme_V1.SchemeServiceClient
	Nodes  Ydb_Coordination_V1.CoordinationServiceClient
}

// NewCoordination binds native calls to the session's authenticated driver.
func NewCoordination(driver *ydb.Driver) *Coordination {
	conn := ydb.GRPCConn(driver)
	return &Coordination{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(conn), Nodes: Ydb_Coordination_V1.NewCoordinationServiceClient(conn)}
}

// Apply verifies captured state, dispatches once, and reads back the outcome.
// Dispatched is true as soon as a write RPC is attempted, including transport
// failure or cancellation. Native YDB has no atomic schema compare-and-set.
func (c *Coordination) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	normalized, err := schema.NormalizeCoordinationOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(normalized.NormalizedOwner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("coordination operation targets another database")
	}
	owner.Database = database
	if err := schema.ValidateCoordinationOwner(owner); err != nil {
		return false, err
	}
	before, after, err := schema.CoordinationOperands(schema.CoordinationOperationChange(normalized.Value))
	if err != nil {
		return false, err
	}
	current, err := c.read(ctx, owner)
	if err != nil {
		return false, err
	}
	if (before == nil) != (current == nil) || (before != nil && !bytes.Equal(before.Payload, current.Payload)) {
		return false, fmt.Errorf("coordination state changed since capture")
	}
	var config *Ydb_Coordination.Config
	if after != nil {
		intent, err := schema.CoordinationConfiguration(after, protocol.ValueInput_DESIRED)
		if err != nil {
			return false, err
		}
		config, err = schema.CoordinationNativeConfig(intent)
		if err != nil {
			return false, err
		}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	absolute := path.Join(database, owner.Schema, owner.Name)
	switch {
	case before == nil:
		response, callErr := c.Nodes.CreateNode(ctx, &Ydb_Coordination.CreateNodeRequest{Path: absolute, Config: config})
		if callErr != nil {
			return true, fmt.Errorf("create YDB coordination node: transport failed")
		}
		err = Status(response.GetOperation())
	case after == nil:
		response, callErr := c.Nodes.DropNode(ctx, &Ydb_Coordination.DropNodeRequest{Path: absolute})
		if callErr != nil {
			return true, fmt.Errorf("drop YDB coordination node: transport failed")
		}
		err = Status(response.GetOperation())
	default:
		response, callErr := c.Nodes.AlterNode(ctx, &Ydb_Coordination.AlterNodeRequest{Path: absolute, Config: config})
		if callErr != nil {
			return true, fmt.Errorf("alter YDB coordination node: transport failed")
		}
		err = Status(response.GetOperation())
	}
	if err != nil {
		return true, err
	}
	observed, err := c.read(ctx, owner)
	if err != nil {
		return true, err
	}
	if err := confirmCoordination(before, after, observed); err != nil {
		return true, err
	}
	return true, nil
}

func (c *Coordination) read(ctx context.Context, owner *protocol.Reference) (*protocol.Envelope, error) {
	directory := path.Join(owner.Database, owner.Schema)
	response, err := c.Scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: directory})
	if err != nil {
		return nil, fmt.Errorf("read YDB coordination namespace: transport failed")
	}
	var listing Ydb_Scheme.ListDirectoryResult
	if err := Operation(response.GetOperation(), &listing); err != nil {
		return nil, err
	}
	if listing.GetSelf().GetName() != path.Base(directory) || (listing.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE && listing.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY) || len(listing.Children) > 4096 {
		return nil, fmt.Errorf("invalid YDB coordination namespace")
	}
	found := false
	for _, entry := range listing.Children {
		if entry.GetName() != owner.Name {
			continue
		}
		if found || entry.GetType() != Ydb_Scheme.Entry_COORDINATION_NODE {
			return nil, fmt.Errorf("coordination path has a different or ambiguous occupant")
		}
		found = true
	}
	if !found {
		return nil, nil
	}
	absolute := path.Join(directory, owner.Name)
	described, err := c.Nodes.DescribeNode(ctx, &Ydb_Coordination.DescribeNodeRequest{Path: absolute})
	if err != nil {
		return nil, fmt.Errorf("describe YDB coordination node: transport failed")
	}
	var result Ydb_Coordination.DescribeNodeResult
	if err := Operation(described.GetOperation(), &result); err != nil {
		return nil, err
	}
	if result.GetSelf().GetName() != owner.Name || result.GetSelf().GetType() != Ydb_Scheme.Entry_COORDINATION_NODE || (result.GetConfig().GetPath() != "" && result.GetConfig().GetPath() != absolute) {
		return nil, fmt.Errorf("YDB coordination observation has a different identity")
	}
	return schema.CaptureObject(owner, &result)
}

func confirmCoordination(before, after, current *protocol.Envelope) error {
	if after == nil {
		if current != nil {
			return fmt.Errorf("coordination removal was not confirmed")
		}
		return nil
	}
	if current == nil {
		return fmt.Errorf("coordination result is absent")
	}
	if err := schema.ValidateCoordinationAccess(current, after); err != nil {
		return err
	}
	actual, err := schema.CoordinationConfiguration(current, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	wanted, err := schema.CoordinationConfiguration(after, protocol.ValueInput_DESIRED)
	if err != nil {
		return err
	}
	a, err := schema.EffectiveCoordination(actual)
	if err != nil {
		return err
	}
	b, err := schema.EffectiveCoordination(wanted)
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("coordination result differs from the requested configuration")
	}
	if before != nil {
		var old, observed Ydb_Coordination.DescribeNodeResult
		if err := protojson.Unmarshal(before.Payload, &old); err != nil {
			return err
		}
		if err := protojson.Unmarshal(current.Payload, &observed); err != nil {
			return err
		}
		if !proto.Equal(old.Self, observed.Self) {
			return fmt.Errorf("coordination access metadata changed during alteration")
		}
	}
	return nil
}

// RunStatement runs one of Ptah's coordination node statements, which YDB's
// query service does not take, through the coordination service, as the
// host's YDB connection runs it. database is the absolute path of the
// session's database and root the directory its statements are confined to:
// database itself, or a dev realm's directory under it, whose path prefix a
// relative node path is read with.
//
// A node outside root, the host's lock node at the database root, and a path
// segment that starts with a dot are refused before any call. A CREATE first
// asks whether the node exists, because the service answers a creation of an
// existing node with success and leaves its configuration as it was. An ALTER
// merges the settings it changes with the node's own and refuses a result the
// node would not run with as written; the service keeps every setting the
// call leaves out. Dispatched is true once a write is attempted.
func (c *Coordination) RunStatement(ctx context.Context, database, root string, query coordinationsql.Query, beforeWrite func(context.Context) error) (dispatched bool, err error) {
	database = "/" + strings.Trim(database, "/")
	root = "/" + strings.Trim(root, "/")
	prefix := ""
	if root != database {
		prefix = root
	}
	absolute, err := query.Absolute(database, prefix)
	if err != nil {
		return false, &StatementRefusal{Err: err}
	}
	if _, inside := strings.CutPrefix(absolute, strings.TrimSuffix(root, "/")+"/"); !inside {
		return false, &StatementRefusal{Err: fmt.Errorf("%w: coordination node %s is not in %s", coordinationsql.ErrStatement, absolute, root)}
	}
	relative := strings.TrimPrefix(absolute, database+"/")
	directory := path.Dir(relative)
	if directory == "." {
		directory = ""
	}
	if err := ydbdecl.RefuseCoordinationName(directory, path.Base(relative)); err != nil {
		return false, &StatementRefusal{Err: fmt.Errorf("%w: %w", coordinationsql.ErrStatement, err)}
	}
	current, found, err := c.describeNode(ctx, absolute)
	if err != nil {
		return false, err
	}
	var call func() (*Ydb_Operations.Operation, error)
	switch query.Verb {
	case coordinationsql.Create:
		if found {
			return false, &StatementRefusal{Err: fmt.Errorf("create YDB coordination node %s: the node already exists", absolute)}
		}
		call = func() (*Ydb_Operations.Operation, error) {
			response, err := c.Nodes.CreateNode(ctx, &Ydb_Coordination.CreateNodeRequest{Path: absolute, Config: statementConfig(query.Settings)})
			return response.GetOperation(), err
		}
	case coordinationsql.Alter:
		if !found {
			return false, &StatementRefusal{Err: fmt.Errorf("change YDB coordination node %s: no coordination node is at this path", absolute)}
		}
		if err := ydbdecl.ValidateCoordinationNode(coordinationsql.Merge(current, query.Settings)); err != nil {
			return false, &StatementRefusal{Err: fmt.Errorf("%w: change YDB coordination node %s: %w", coordinationsql.ErrStatement, absolute, err)}
		}
		call = func() (*Ydb_Operations.Operation, error) {
			response, err := c.Nodes.AlterNode(ctx, &Ydb_Coordination.AlterNodeRequest{Path: absolute, Config: statementConfig(query.Settings)})
			return response.GetOperation(), err
		}
	default:
		if !found {
			return false, &StatementRefusal{Err: fmt.Errorf("drop YDB coordination node %s: no coordination node is at this path", absolute)}
		}
		call = func() (*Ydb_Operations.Operation, error) {
			response, err := c.Nodes.DropNode(ctx, &Ydb_Coordination.DropNodeRequest{Path: absolute})
			return response.GetOperation(), err
		}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	operation, callErr := call()
	if callErr != nil {
		return true, fmt.Errorf("YDB coordination node %s: transport failed", absolute)
	}
	if err := Status(operation); err != nil {
		return true, fmt.Errorf("YDB coordination node %s: %w", absolute, err)
	}
	return true, nil
}

// StatementRefusal is why a coordination node statement was refused before
// anything was sent: the statement and what the server holds disagree. Its
// text is Ptah's own and names the node, never a credential or server text.
type StatementRefusal struct{ Err error }

func (r *StatementRefusal) Error() string { return r.Err.Error() }
func (r *StatementRefusal) Unwrap() error { return r.Err }

// describeNode reads the configuration of the node at absolute as YDB keeps
// it, every setting nobody sent left out, and reports whether a node is
// there. A path that holds an object of another kind is an error.
func (c *Coordination) describeNode(ctx context.Context, absolute string) (ydbdecl.CoordinationNode, bool, error) {
	response, err := c.Scheme.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: path.Dir(absolute)})
	if err != nil {
		return ydbdecl.CoordinationNode{}, false, fmt.Errorf("read YDB coordination namespace: transport failed")
	}
	var listing Ydb_Scheme.ListDirectoryResult
	if err := Operation(response.GetOperation(), &listing); err != nil {
		return ydbdecl.CoordinationNode{}, false, fmt.Errorf("read the directory of YDB coordination node %s: %w", absolute, err)
	}
	found := false
	for _, entry := range listing.GetChildren() {
		if entry.GetName() != path.Base(absolute) {
			continue
		}
		if entry.GetType() != Ydb_Scheme.Entry_COORDINATION_NODE {
			return ydbdecl.CoordinationNode{}, false, &StatementRefusal{Err: fmt.Errorf("%w: %s is a YDB object of another kind than a coordination node",
				coordinationsql.ErrStatement, absolute)}
		}
		found = true
	}
	if !found {
		return ydbdecl.CoordinationNode{}, false, nil
	}
	described, err := c.Nodes.DescribeNode(ctx, &Ydb_Coordination.DescribeNodeRequest{Path: absolute})
	if err != nil {
		return ydbdecl.CoordinationNode{}, false, fmt.Errorf("describe YDB coordination node: transport failed")
	}
	var result Ydb_Coordination.DescribeNodeResult
	if err := Operation(described.GetOperation(), &result); err != nil {
		return ydbdecl.CoordinationNode{}, false, fmt.Errorf("describe YDB coordination node %s: %w", absolute, err)
	}
	current, err := nodeSettings(result.GetConfig())
	if err != nil {
		return ydbdecl.CoordinationNode{}, false, fmt.Errorf("describe YDB coordination node %s: %w", absolute, err)
	}
	return current, true, nil
}

// consistencyNames and counterNames name the enum values a statement reads
// and writes. A value outside them is refused by its number rather than read
// as the nearest one: YDB stores and reports a mode of 7.
var (
	consistencyNames = map[Ydb_Coordination.ConsistencyMode]string{
		Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_UNSET:   "",
		Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT:  "strict",
		Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED: "relaxed",
	}
	counterNames = map[Ydb_Coordination.RateLimiterCountersMode]string{
		Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_UNSET:      "",
		Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_AGGREGATED: "aggregated",
		Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED:   "detailed",
	}
)

// nodeSettings reads a node's configuration as the settings a statement
// names.
func nodeSettings(config *Ydb_Coordination.Config) (ydbdecl.CoordinationNode, error) {
	read, readKnown := consistencyNames[config.GetReadConsistencyMode()]
	attach, attachKnown := consistencyNames[config.GetAttachConsistencyMode()]
	counters, countersKnown := counterNames[config.GetRateLimiterCountersMode()]
	switch {
	case !readKnown:
		return ydbdecl.CoordinationNode{}, fmt.Errorf("its read_consistency_mode is %d, which Ptah does not know", config.GetReadConsistencyMode())
	case !attachKnown:
		return ydbdecl.CoordinationNode{}, fmt.Errorf("its attach_consistency_mode is %d, which Ptah does not know", config.GetAttachConsistencyMode())
	case !countersKnown:
		return ydbdecl.CoordinationNode{}, fmt.Errorf("its rate_limiter_counters_mode is %d, which Ptah does not know", config.GetRateLimiterCountersMode())
	}
	return ydbdecl.CoordinationNode{SelfCheckPeriodMillis: config.GetSelfCheckPeriodMillis(), SessionGracePeriodMillis: config.GetSessionGracePeriodMillis(),
		ReadConsistencyMode: read, AttachConsistencyMode: attach, RateLimiterCountersMode: counters}, nil
}

// statementConfig writes the settings a statement names as the configuration
// the coordination service takes. A setting it leaves out is sent unset,
// which the service reads as "keep" on a change and as "the default" on a
// creation.
func statementConfig(settings ydbdecl.CoordinationNode) *Ydb_Coordination.Config {
	config := &Ydb_Coordination.Config{SelfCheckPeriodMillis: settings.SelfCheckPeriodMillis, SessionGracePeriodMillis: settings.SessionGracePeriodMillis}
	for mode, name := range consistencyNames {
		if name == "" {
			continue
		}
		if settings.ReadConsistencyMode == name {
			config.ReadConsistencyMode = mode
		}
		if settings.AttachConsistencyMode == name {
			config.AttachConsistencyMode = mode
		}
	}
	for mode, name := range counterNames {
		if name != "" && settings.RateLimiterCountersMode == name {
			config.RateLimiterCountersMode = mode
		}
	}
	return config
}
