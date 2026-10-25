// Package ydbdecl reads the YDB settings and objects a YQL schema document
// declares into the specifications the Ptah host's YDB owner keeps for them:
// a row table's partitioning, starting layout, column families and TTL, a
// column table's storage, a secondary index's settings, a changefeed, a topic
// and its consumers, a coordination node, and an external data source or
// table.
//
// It is the reading the host's internal/ydbpartition, internal/ydbfamily,
// internal/ydbttl, internal/ydbcolumn, internal/ydbindex,
// internal/ydbchangefeed and the dialect/ydb packages give the host's YQL
// reader, ported so the provider refuses what the host refuses, with the
// host's words. Each specification encodes to JSON as the host's codec writes
// its payload, which is how internal/yqlschema's corpus compares the two. The
// provider's own models are built from these specifications where a SQL
// schema reading has a home for them.
package ydbdecl

// TableFacets are the settings a CREATE TABLE declares beside its columns,
// key and indexes. A nil field is one it leaves out.
type TableFacets struct {
	Partitioning *TablePartitioning
	Families     []ColumnFamily
	TTL          *TTL
	ColumnStore  *ColumnStore
}

// Empty reports a table that declares none of them.
func (f TableFacets) Empty() bool {
	return f.Partitioning == nil && len(f.Families) == 0 && f.TTL == nil && f.ColumnStore == nil
}
