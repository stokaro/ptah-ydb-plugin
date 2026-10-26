package nativeapi_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func streamingOperation(c *qt.C, database string, change streaming.Change) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Database: database, Schema: "app", Name: "copy"}
	var before, after *protocol.Envelope
	var err error
	if spec := change.Before; spec != nil {
		before, err = schema.CaptureObject(owner, &native.StreamingQuerySettings{Text: spec.Text, Run: spec.Run, ResourcePool: spec.ResourcePool})
		c.Assert(err, qt.IsNil)
	}
	if spec := change.After; spec != nil {
		after, err = schema.CaptureStreamingQueryIntent(owner, *spec, change.AllowStateReset)
		c.Assert(err, qt.IsNil)
	}
	captured, err := schema.CaptureStreamingQueryChange("change", owner, before, after)
	c.Assert(err, qt.IsNil)
	operation, err := schema.StreamingQueryOperationFromChange("operation", captured.Value)
	c.Assert(err, qt.IsNil)
	return operation.Value
}

func TestStreamingApplyHappyPath(t *testing.T) {
	running := &streaming.Spec{Text: "SELECT 1;"}
	stopped := &streaming.Spec{Text: "SELECT 1;", Run: new(false)}
	for _, tc := range []struct {
		name   string
		change streaming.Change
		want   []string
	}{
		{"create", streaming.Change{After: stopped}, []string{"CREATE STREAMING QUERY `app/copy` WITH (RUN = FALSE, RESOURCE_POOL = `default`) AS DO BEGIN\nSELECT 1;\nEND DO;"}},
		{"drop", streaming.Change{Before: running}, []string{"DROP STREAMING QUERY `app/copy`;"}},
		{"stop", streaming.Change{Before: running, After: stopped}, []string{"ALTER STREAMING QUERY `app/copy` SET (RUN = FALSE, RESOURCE_POOL = `default`);"}},
		{"start", streaming.Change{Before: stopped, After: running}, []string{"ALTER STREAMING QUERY `app/copy` SET (RUN = TRUE, RESOURCE_POOL = `default`);"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			reads, checks := 0, 0
			var executed []string
			api := &nativeapi.StreamingQuery{
				Read: func(_ context.Context, absolute string) (*streaming.Spec, error) {
					c.Assert(absolute, qt.Equals, "/local/app/copy")
					reads++
					if reads == 1 {
						return tc.change.Before, nil
					}
					return tc.change.After, nil
				},
				Execute: func(_ context.Context, statement string) error {
					c.Assert(checks, qt.Equals, 1)
					executed = append(executed, statement)
					return nil
				},
			}
			dispatched, err := api.Apply(c.Context(), "/local", streamingOperation(c, "/local", tc.change), func(context.Context) error { checks++; return nil })
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(reads, qt.Equals, 2)
			c.Assert(executed, qt.DeepEquals, tc.want)
		})
	}
}

func TestStreamingApplyRefusesBeforeDispatch(t *testing.T) {
	for _, name := range []string{"changed precondition", "read failure", "lock failure", "canceled", "foreign database", "invalid operation"} {
		t.Run(name, func(t *testing.T) {
			c := qt.New(t)
			change := streaming.Change{After: &streaming.Spec{Text: "SELECT 1;", Run: new(false)}}
			database := "/local"
			if name == "foreign database" {
				database = "/other"
			}
			operation := streamingOperation(c, database, change)
			if name == "invalid operation" {
				operation = nil
			}
			ctx, cancel := context.WithCancel(c.Context())
			c.Cleanup(cancel)
			var executed int
			failure := errors.New(name)
			api := &nativeapi.StreamingQuery{
				Read: func(context.Context, string) (*streaming.Spec, error) {
					if name == "read failure" {
						return nil, failure
					}
					if name == "changed precondition" {
						return change.After, nil
					}
					return nil, nil
				},
				Execute: func(context.Context, string) error { executed++; return nil },
			}
			dispatched, err := api.Apply(ctx, "/local", operation, func(context.Context) error {
				if name == "lock failure" {
					return failure
				}
				if name == "canceled" {
					cancel()
				}
				return nil
			})
			c.Assert(err, qt.IsNotNil)
			if name == "read failure" || name == "lock failure" {
				c.Assert(err, qt.ErrorIs, failure)
			}
			if name == "canceled" {
				c.Assert(err, qt.ErrorIs, context.Canceled)
			}
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(executed, qt.Equals, 0)
		})
	}
}

func TestStreamingApplyReportsUncertainDispatch(t *testing.T) {
	for _, name := range []string{"execute failure", "confirmation failure", "confirmation differs"} {
		t.Run(name, func(t *testing.T) {
			c := qt.New(t)
			change := streaming.Change{After: &streaming.Spec{Text: "SELECT 1;", Run: new(false)}}
			reads, executions := 0, 0
			failure := errors.New(name)
			api := &nativeapi.StreamingQuery{
				Read: func(context.Context, string) (*streaming.Spec, error) {
					reads++
					if reads > 1 && name == "confirmation failure" {
						return nil, failure
					}
					return nil, nil
				},
				Execute: func(context.Context, string) error {
					executions++
					if name == "execute failure" {
						return failure
					}
					return nil
				},
			}
			dispatched, err := api.Apply(c.Context(), "/local", streamingOperation(c, "/local", change), func(context.Context) error { return nil })
			c.Assert(err, qt.IsNotNil)
			if name != "confirmation differs" {
				c.Assert(err, qt.ErrorIs, failure)
			}
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(executions, qt.Equals, 1)
			if name == "execute failure" {
				c.Assert(reads, qt.Equals, 1)
			} else {
				c.Assert(reads, qt.Equals, 2)
			}
		})
	}
}

func TestStreamingApplySplitStepChecksStoppedPrecondition(t *testing.T) {
	c := qt.New(t)
	running := &streaming.Spec{Text: "SELECT 1;"}
	change := streaming.Change{Before: &streaming.Spec{Text: "SELECT 1;", Run: new(false)}, After: running}
	executions := 0
	api := &nativeapi.StreamingQuery{
		Read:    func(context.Context, string) (*streaming.Spec, error) { return running, nil },
		Execute: func(context.Context, string) error { executions++; return nil },
	}
	dispatched, err := api.Apply(c.Context(), "/local", streamingOperation(c, "/local", change), func(context.Context) error { return nil })
	c.Assert(err, qt.ErrorMatches, `.*changed since the plan read it`)
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(executions, qt.Equals, 0)
}

func TestStreamingApplyDoesNotDispatchAfterCancellation(t *testing.T) {
	c := qt.New(t)
	change := streaming.Change{Before: &streaming.Spec{Text: "SELECT 1;"}, After: &streaming.Spec{Text: "SELECT 2;"}, AllowStateReset: true}
	ctx, cancel := context.WithCancel(c.Context())
	c.Cleanup(cancel)
	executions := 0
	api := &nativeapi.StreamingQuery{
		Read: func(context.Context, string) (*streaming.Spec, error) { return change.Before, nil },
		Execute: func(context.Context, string) error {
			executions++
			cancel()
			return nil
		},
	}
	dispatched, err := api.Apply(ctx, "/local", streamingOperation(c, "/local", change), func(context.Context) error { return nil })
	c.Assert(err, qt.ErrorIs, context.Canceled)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(executions, qt.Equals, 1)
}

func TestReadStreamingQueryChecksCompleteObservations(t *testing.T) {
	valid := []driver.Value{"/local/app/copy", "SELECT 1;", false, "default"}
	for _, tc := range []struct {
		name      string
		rows      [][]driver.Value
		closeErr  error
		wantError bool
	}{
		{"stored settings", [][]driver.Value{valid}, nil, false},
		{"absence", nil, nil, false},
		{"duplicate", [][]driver.Value{valid, valid}, nil, true},
		{"wrong path", [][]driver.Value{{"/local/app/other", "SELECT 1;", false, "default"}}, nil, true},
		{"missing run", [][]driver.Value{{"/local/app/copy", "SELECT 1;", nil, "default"}}, nil, true},
		{"missing pool", [][]driver.Value{{"/local/app/copy", "SELECT 1;", false, nil}}, nil, true},
		{"schema statement", [][]driver.Value{{"/local/app/copy", "DROP TABLE t;", false, "default"}}, nil, true},
		{"late read failure", [][]driver.Value{valid}, errors.New("stream ended"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			connector := &streamingReadConnector{rows: tc.rows, closeErr: tc.closeErr}
			db := sql.OpenDB(connector)
			c.Cleanup(func() { c.Check(db.Close(), qt.IsNil) })
			spec, err := nativeapi.ReadStreamingQuery(c.Context(), db, "/local", "/local/app/copy")
			if tc.wantError {
				c.Assert(err, qt.IsNotNil)
				c.Assert(spec, qt.IsNil)
			} else {
				c.Assert(err, qt.IsNil)
				if len(tc.rows) == 0 {
					c.Assert(spec, qt.IsNil)
				} else {
					c.Assert(spec, qt.DeepEquals, &streaming.Spec{Text: "SELECT 1;", Run: new(false), ResourcePool: "default"})
				}
			}
			c.Assert(connector.closed, qt.IsTrue)
			c.Assert(connector.query, qt.Equals, "SELECT Path, Text, Run, ResourcePool FROM `/local/.sys/streaming_queries` WHERE Path = $path")
			c.Assert(connector.arguments, qt.DeepEquals, []driver.NamedValue{{Name: "path", Ordinal: 1, Value: "/local/app/copy"}})
		})
	}
}

type streamingReadConnector struct {
	rows      [][]driver.Value
	closeErr  error
	closed    bool
	query     string
	arguments []driver.NamedValue
}

func (c *streamingReadConnector) Connect(context.Context) (driver.Conn, error) {
	return streamingReadConn{connector: c}, nil
}

func (*streamingReadConnector) Driver() driver.Driver { return streamingReadDriver{} }

type streamingReadDriver struct{}

func (streamingReadDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the connector")
}

type streamingReadConn struct{ connector *streamingReadConnector }

func (streamingReadConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not expected")
}

func (streamingReadConn) Close() error { return nil }

func (streamingReadConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not expected")
}

func (c streamingReadConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.connector.query, c.connector.arguments = query, args
	return &streamingReadRows{connector: c.connector}, nil
}

type streamingReadRows struct {
	connector *streamingReadConnector
	position  int
}

func (*streamingReadRows) Columns() []string { return []string{"Path", "Text", "Run", "ResourcePool"} }

func (r *streamingReadRows) Close() error {
	r.connector.closed = true
	return r.connector.closeErr
}

func (r *streamingReadRows) Next(values []driver.Value) error {
	if r.position == len(r.connector.rows) {
		return io.EOF
	}
	copy(values, r.connector.rows[r.position])
	r.position++
	return nil
}
