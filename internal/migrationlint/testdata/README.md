# Migration lint corpus

The files here hold the Ptah host's answers for YDB migration lint, so the
provider's lint can be compared with the host's built-in YDB owner
(`dialect/ydb/ydblint`) finding for finding. The tests in
`internal/migrationlint` replay every recorded request and require the same
answer.

## Layout

- `corpus/<case>/` is a migration directory.
- `corpus/<case>.runs.json`, where present, lists lint runs beyond the
  standard ones: a server version, a command surface, a directory format,
  disabled rules or capability overrides.
- `corpus/<case>.request.json` is a request the host sends its YDB owner
  directly: a split, an annotation or a check.
- `corpus/<case>.golden.json` is the host's answer for one case.
- `rules.golden.json` is the rule catalog the host's YDB owner publishes.
- `targets.golden.json` is the capability map the host resolves for each
  server version a run names. A recorded check names its version and any
  capability that differs from that map.

Every directory is linted through `migration/lint`, the code
`ptah migrations lint` runs, with dialect `ydb` on the newest line, on
25.1.4.7 and on the Atlas-compatible surface, and with no dialect. A recording
runtime keeps every split, annotation and check request the run sends to the
owners and the owners' answers, which is what crosses the boundary a provider
serves. The `report` of a run is what the host printed, for a reader; the
tests do not compare it, because suppression, severity and the rules every
dialect shares are the host's.

Most cases come from the inputs the host's own tests lint with the YDB dialect
or with no dialect, at the host commit the golden files name. The cases named
`edge-*` were written for this corpus, to reach readers the host's tests do
not.

## Regenerating

`oracle/main.go` imports host packages behind an internal boundary, so it runs
from a checkout of the host:

```sh
mkdir -p <host>/cmd/ydblintoracle
cp internal/migrationlint/testdata/oracle/main.go <host>/cmd/ydblintoracle/main.go
(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydblintoracle \
  <plugin>/internal/migrationlint/testdata)
```

Use a clean checkout at the commit to record against. The program rewrites
every golden file, so a diff of the regenerated files shows what the host now
answers differently.
