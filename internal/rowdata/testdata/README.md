# Row corpus

The files here hold the Ptah host's answers for YDB rows, so the provider's
row services can be compared with the host's embedded YDB engine value for
value. The tests in `internal/rowdata` replay every recorded case and require
the same answer.

## Layout

- `corpus/type-<case>.json` is a row typing request: the capabilities, the
  live table, the columns with their declared types, and the declared and live
  rows. Its golden file holds what `internal/ydbdata.Rows.TypeRows` answered:
  the type of each column and both row sets in canonical form, or the error.
- `corpus/write-<case>.json` is a batch of row writes. Its golden file holds
  the statement `migration/datadiff` renders for each write with the YDB data
  services, or the error. The host's row diff writes no UPSERT; for one, the
  golden file holds the INSERT of the same row, whose shape YQL's UPSERT INTO
  shares.
- `query-dialect.golden.json` is the query-builder spelling of the host's YDB
  descriptor and the capabilities the builder assumes for YDB.
- `../../yqlquery/testdata/corpus/<case>.yql` is a YQL text, and its golden file
  the queries `internal/yqlquery.Split` cuts it into, or the error.

A value is a JSON object with one key that names its case, as the contract's
DataValue does: `null`, `bool`, `int64` and `uint64` (decimal text), `double`,
`string`, `bytes` (base64), `decimal` (`digits`, `precision`, `scale`),
`timestamp` (`seconds`, `nanos`), `date` (days since 1970-01-01), `interval`
(`months`, `days`, `nanoseconds`), `json` and `native`. An empty object is a
column the row does not carry. The oracle turns each value into the Go value
the host compares and writes, and each canonical Go value back into the case of
its column's kind; a column the host leaves untyped keeps the value it was
given.

The provider's refusal of a write names the write and the column and never the
value, where the host's names the value; the tests compare the rest of the
message.

## Regenerating

`oracle/main.go` imports host packages behind an internal boundary, so it runs
from a checkout of the host:

```sh
mkdir -p <host>/cmd/ydbrowsoracle
cp internal/rowdata/testdata/oracle/main.go <host>/cmd/ydbrowsoracle/main.go
(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbrowsoracle <plugin>)
```

Use a clean checkout at the commit to record against. The program rewrites
every golden file, so a diff of the regenerated files shows what the host now
answers differently. A new case is a new input file; run the program to record
its answer.
