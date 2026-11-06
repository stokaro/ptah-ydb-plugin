# An HCL export of this provider

`inspect.hcl` is what this provider wrote through the host, built as
release 0.4.0, for a directory `h1` that holds a row table `items`, a topic
`events` and the coordination nodes `jobs` and `locks`: the objects the
built-in engine's exports in `../builtin-hcl` describe. The bytes are the same
on YDB 25.1.4.7 and 26.2.1.14:

```sh
ptah schema apply --provider-config <manifest> --provider-engine ydb \
  --db-url <url> --schemas h1 --schema-file h1.yaml --auto-approve
ptah schema inspect --provider-config <manifest> --provider-engine ydb \
  --db-url <url> --schemas h1 --format hcl
```

`h1.yaml` is the YAML source `../builtin-hcl/README.md` gives. HCL has no
spelling for the topic, so the file does not declare it.

The file opens with the `ptah:feature-coverage` header the host writes for
this provider's models. It names each model by the identity the provider
describes, and `TestTheRecordedProviderHeaderNamesPinnedIdentities` checks
that each is one `exportedModels` pins. The live tests apply the file through
the host with the build under test, so a definition text that changes without
naming the identity it replaced as a predecessor refuses this file.

Record it again only to add a model a header names, never to follow a
changed digest: the file stands for an export a user already holds.
