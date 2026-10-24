# Statement facts corpus

Each `<case>.yql` is a YQL file. `<case>.golden.json` is what the Ptah host's
built-in YQL reading of `ptah sql lint` (`internal/sqllint`, `lintYQL`) reports
over it on YDB 25.1.4.7, 25.4.1.15 and 26.2.1.14, with the capabilities the host
resolves for each line, recorded at the host commit the file names.

`statementfacts_test.go` lints each file from the statement facts of this
provider's split, the way the host's `lintOwned` reads them, and requires the
same rules at the same positions about the same tables and capabilities.
`integration/host_lint_test.go` runs the host's `ptah sql lint` through the
provider over the same files and requires the same.

`stricter/` holds the files the two readings answer differently on purpose.
The built-in reading skips a statement that starts with no word and the first
statement of a file that opens with a UTF-8 byte order mark, where the
statement facts lint both; and it names a table written as a named expression
with an empty string, where the facts keep the name as written.

## Recording the goldens again

The oracle imports host packages behind an internal boundary, so it runs from a
checkout of the host:

```sh
mkdir -p <host>/cmd/ydbsqllintoracle
cp internal/migrationlint/testdata/statementfacts/oracle/main.go <host>/cmd/ydbsqllintoracle/main.go
cd <host>
HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbsqllintoracle <plugin>/internal/migrationlint/testdata/statementfacts
HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbsqllintoracle <plugin>/internal/migrationlint/testdata/statementfacts/stricter
rm -r cmd/ydbsqllintoracle
```

Record every golden at one commit; the tests refuse a corpus recorded at two.
