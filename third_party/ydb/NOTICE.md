# YDB native protocol definitions

The table, column, sequence, and index messages in `internal/nativeprotocol/indexes.proto` are adapted
from `ydb/public/api/protos/ydb_table.proto` in
[ydb-platform/ydb at 6006bfe4442d2064e7c06ed1719d438e36ca5bf8](https://github.com/ydb-platform/ydb/blob/6006bfe4442d2064e7c06ed1719d438e36ca5bf8/ydb/public/api/protos/ydb_table.proto).
They retain the native wire field numbers and types. The package and Go
package differ to avoid conflicting with the generated YDB client bindings.
Upstream validation annotations are omitted. Provider code checks supported
variants, identity, read completeness, and unknown fields. Unsupported local-index
arms are omitted and refused as unknown fields. Ptah-owned observation
messages bind the index to its implementation tables.

The upstream definitions are licensed under Apache-2.0; see `LICENSE` here.

Implementation table names and their kind-specific membership follow
[NTableIndex::GetImplTables at the same commit](https://github.com/ydb-platform/ydb/blob/6006bfe4442d2064e7c06ed1719d438e36ca5bf8/ydb/core/base/table_index.cpp#L287)
and the constants in `ydb/core/base/table_index.h`.

Topic and consumer messages in `internal/nativeprotocol/topics.proto` are
adapted from [ydb_topic.proto at the same upstream commit](https://github.com/ydb-platform/ydb/blob/6006bfe4442d2064e7c06ed1719d438e36ca5bf8/ydb/public/api/protos/ydb_topic.proto).
The changefeed description comes from the table protocol cited above. The
shared-consumer arm is omitted and refused as unknown. Ptah-owned observation
messages pair each changefeed with its child topic and retain initial scan
presence without runtime progress counters.

The external-table description in `internal/nativeprotocol/objects.proto` is
adapted from the same table protocol, using the provider's complete column
grammar. `ViewObservation` is a Ptah-owned composition of the view and table
API descriptions. View and coordination API descriptors otherwise come from
the Go dependency pinned in `go.mod`.
