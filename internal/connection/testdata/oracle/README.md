# Host address oracle

`urls.txt` lists the connection addresses the corpus covers, one per line.
`host.json` holds the host's answer to each: whether its YDB connection layer
accepts the address, the refusal when it does not, and the database and the
address handed to ydb-go-sdk when it does.

The host (stokaro/ptah) answers through three steps, run in order over the
address: `ydburl.Parse`, `sdkParameters` and `credentialOption` of
`internal/dbschema/ydb`, with `YDB_ACCESS_TOKEN_CREDENTIALS` set in the
environment. To regenerate, check out the host commit named in
`.github/workflows/ci.yml` in a scratch worktree, add a test to
`internal/dbschema/ydb` that reads `urls.txt`, runs those three steps for each
line and writes the result in the shape of `host.json`, and run it. Remove the
scratch worktree afterwards; nothing of it is committed to the host.

Host refusals repeat the offending value; this package's never do, so the
differential test compares the verdict and the SDK address, not the text.
