# Dev cleanup corpus

Each `corpus/<case>.json` is a scheme tree: the entries of each absolute
directory, the database, the realm the cleanup treats as its root (empty for
the database itself), and for some cases a realm to remove through the
database. `corpus/<case>.golden.json` holds what the host's embedded YDB engine
did with the tree (`internal/dbschema/ydb`): the objects `Writer.ResetObjects`
listed, the statements and directory removals of `Writer.DropDatabaseRealm`
over a copy of the tree, or its error, and the same for `Writer.RemoveRealm`.
The tests in `internal/devclean` replay each tree through the provider's walk,
cleanup and realm removal and require the same objects, statements and
removals. The host words a refusal as an error; the provider reports it as a
refused cleanup that names the objects that stopped it.

## Regenerating

`oracle/main.go` imports host packages behind an internal boundary, so it runs
from a checkout of the host:

```sh
mkdir -p <host>/cmd/ydbdevcleanoracle
cp internal/devclean/testdata/oracle/main.go <host>/cmd/ydbdevcleanoracle/main.go
(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbdevcleanoracle <plugin>/internal/devclean/testdata/corpus)
```

Use a clean checkout at the commit to record against. A new case is a new
input file; run the program to record its answer.
