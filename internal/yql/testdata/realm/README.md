# Host replay guard oracle

`statements.txt` lists one statement per line. `host.json` holds the answer of
the host's YDB replay guard (`internal/devclean/ydbreplay`) for each, under the
scope that confines a statement to a dev realm: whether it refuses and what it
says the statement does.

To regenerate, check out the host commit named in `.github/workflows/ci.yml` in
a scratch worktree, add a white-box test to `internal/devclean/ydbreplay` that
reads `statements.txt`, calls `validateYDBReplayStatement` with the tokens of
each line and an empty `replay.Scope`, and writes the result in the shape of
`host.json`. Remove the scratch worktree afterwards.

One difference from the host is deliberate: the host refuses every `--!` line,
and `ConfineToRealm` lets `--!syntax_v1` through, since that is the syntax the
server reads without it.
