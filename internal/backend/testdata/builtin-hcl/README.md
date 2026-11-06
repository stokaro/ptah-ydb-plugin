# HCL exports of the built-in YDB engine

These files are what Ptah's built-in YDB engine wrote, at host commit
74de0b65b, for a directory `h1` that holds a row table `items`, a topic
`events` and the coordination nodes `jobs` and `locks`. The bytes are the
same on YDB 25.1.4.7 and 26.2.1.14:

- `inspect.hcl`: `ptah schema inspect --db-url <url> --schemas h1 --format hcl`;
- `compat-inspect.hcl`: `ptah-compat schema inspect --url <url> --schema h1`;
- `split/`: `ptah schema inspect --db-url <url> --schemas h1 --format hcl --out-dir split`.

HCL has no spelling for the topic, so no file declares it. Each file opens
with the `ptah:feature-coverage` header that names the built-in engine's
coordination-node model. The provider names that identity as a predecessor of
its own model (`modelPredecessors`), and the live tests apply the files
through the host with the directory renamed.

To record them again, create the objects with that binary and run the three
commands above. The YAML source the files were recorded from:

```yaml
tables:
  items:
    schema: h1
    columns:
      id: {type: Uint64, primary: true}
      body: {type: Utf8}
topics:
  events:
    schema: h1
    min_active_partitions: 1
coordination_nodes:
  locks:
    schema: h1
    self_check_period: PT2S
    read_consistency_mode: strict
  jobs:
    schema: h1
```
