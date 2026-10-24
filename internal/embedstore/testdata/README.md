# Embedding store corpus

`golden.json` is what the Ptah host's embedded YDB engine
(`internal/embedydb`) answers over this corpus, recorded at the host commit it
names:

- `schema`: the statements that create the run-state tables, for the 25.1 and
  the 26.2 capabilities;
- `targets`: for each specification of `targets.json`, the column and index a
  generation needs, or the error YDB refuses it with;
- `vectors`: the bytes each vector of `vectors.json` is stored as, in
  hexadecimal.

`differential_test.go` requires the provider's statements to be the same text,
its target objects to be the same and each refusal's message to be part of the
host's error, and its bytes to be the same.

## Recording the goldens again

The oracle imports host packages behind an internal boundary, so it runs from a
checkout of the host:

```sh
mkdir -p <host>/cmd/ydbembedoracle
cp internal/embedstore/testdata/oracle/main.go <host>/cmd/ydbembedoracle/main.go
cd <host>
HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbembedoracle <plugin>/internal/embedstore/testdata
rm -r cmd/ydbembedoracle
```
