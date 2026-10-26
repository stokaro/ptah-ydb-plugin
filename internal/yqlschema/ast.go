package yqlschema

import (
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// span is the bytes of a document a statement was read from.
type span struct{ start, end int }

// statement is one declaration the parser read. Its span is the text of the
// declaration it came from: one GRANT on two paths is two statements with
// one span.
type statement interface {
	place(span)
	at() span
}

type spanned struct{ text span }

func (s *spanned) place(text span) { s.text = text }
func (s *spanned) at() span        { return s.text }

// createTable is a CREATE TABLE. Names are as written; conversion reads them
// the way YDB resolves them.
type createTable struct {
	spanned
	name        string
	ifNotExists bool
	columns     []*column
	indexes     []*index
	primaryKey  []string
	facets      ydbdecl.TableFacets
}

type column struct {
	name, typ    string
	nullable     bool
	defaultValue *string
}

// index is a secondary index a CREATE TABLE declares.
type index struct {
	name, table  string
	kind         string
	unique       bool
	columns      []string
	include      []string
	storage      map[string]string
	partitioning *ydbdecl.IndexPartitioning
	vector       *ydbdecl.VectorIndex
}

// addChangefeed is an ALTER TABLE ... ADD CHANGEFEED.
type addChangefeed struct {
	spanned
	table string
	feed  ydbdecl.Changefeed
}

// createView is a CREATE VIEW.
type createView struct {
	spanned
	name, body string
}

// commentOn is a COMMENT ON, kept as its text, which conversion reads as the
// statement executor does.
type commentOn struct {
	spanned
	text string
}

// alterSequence is an ALTER SEQUENCE of a Serial column's sequence.
type alterSequence struct {
	spanned
	name             string
	start, increment *int64
}

// createRole is a CREATE USER or CREATE GROUP.
type createRole struct {
	spanned
	name     string
	login    bool
	group    bool
	password *string
}

// roleOption is one option of an ALTER USER: a password, or whether the
// user logs in.
type roleOption struct {
	password *string
	login    *bool
}

// alterUser is an ALTER USER.
type alterUser struct {
	spanned
	name    string
	options []roleOption
}

// membership is one member an ALTER GROUP or CREATE GROUP ... WITH USER adds
// or drops.
type membership struct {
	spanned
	group, member string
	add           bool
}

// privilege is a GRANT or REVOKE of permissions on one path to one subject.
type privilege struct {
	spanned
	grant       bool
	role, path  string
	permissions []string
}

// createTopic is a CREATE TOPIC.
type createTopic struct {
	spanned
	schema, name string
	spec         ydbdecl.Topic
}

// addConsumer is an ALTER TOPIC ... ADD CONSUMER.
type addConsumer struct {
	spanned
	schema, name string
	consumer     ydbdecl.Consumer
}

// createCoordinationNode is a CREATE COORDINATION NODE.
type createCoordinationNode struct {
	spanned
	schema, name string
	spec         ydbdecl.CoordinationNode
}

// createExternalSource is a CREATE [OR REPLACE] EXTERNAL DATA SOURCE.
type createExternalSource struct {
	spanned
	schema, name string
	spec         ydbdecl.ExternalDataSource
}

// createExternalTable is a CREATE [OR REPLACE] EXTERNAL TABLE.
type createExternalTable struct {
	spanned
	schema, name string
	spec         ydbdecl.ExternalTable
}

// createPool is a CREATE RESOURCE POOL, or the ALTER RESOURCE POOL default
// SET that declares the default pool's settings.
type createPool struct {
	spanned
	name string
	spec workload.PoolSpec
}

// createClassifier is a CREATE RESOURCE POOL CLASSIFIER.
type createClassifier struct {
	spanned
	name string
	spec workload.ClassifierSpec
}

// unsupported is a statement the host's reader reads and this provider has
// no model for. construct names it in the refusal.
type unsupported struct {
	spanned
	construct string
}

// createSecret is a value-free CREATE SECRET declaration.
type createSecret struct {
	spanned
	schema, name string
	spec         secrets.Spec
}
