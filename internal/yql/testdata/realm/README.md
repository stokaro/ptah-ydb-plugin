# Host replay guard oracle

`statements.txt` lists one statement per line, with `\n` standing for a line
break. `host.json` holds the answer of the host's YDB replay guard
(`internal/devclean/ydbreplay`) for each of the first 83, under the scope that
confines a statement to a dev realm: whether it refuses and what it says the
statement does. `scopes.json` holds its answer for every statement under every
replay scope: the dev database, a pinned replay session, the user databases of
a server named as a whole, a server the run owns, and a server it provisioned.

`oracle/main.go` records `scopes.json`. It imports host packages behind an
internal boundary, so it runs from a checkout of the host:

```sh
mkdir -p <host>/cmd/ydbreplayoracle
cp internal/yql/testdata/realm/oracle/main.go <host>/cmd/ydbreplayoracle/main.go
(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbreplayoracle <plugin>/internal/yql/testdata/realm)
```

`host.json` was recorded by a white-box test in `internal/devclean/ydbreplay`
that called `validateYDBReplayStatement` with an empty `replay.Scope`; the
`database` answers of `scopes.json` are the same.

One difference from the host is deliberate: the host refuses every `--!` line,
and `ConfineToRealm`, which guards the units a realm session runs, lets
`--!syntax_v1` through, since that is the syntax the server reads without it.
`GuardReplay`, which answers the host's replay guard requests, refuses it as
the host does.
