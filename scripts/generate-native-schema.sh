#!/bin/sh
# Protoc 35.1 is used for the checked-in native descriptors.
set -eu
export GOWORK=off
test "$(protoc --version)" = 'libprotoc 35.1'
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
go run ./internal/cmd/native-descriptors "$tmp/dependencies.pb"
protoc -I . --descriptor_set_in="$tmp/dependencies.pb" \
  --plugin=protoc-gen-go="$(go tool -n protoc-gen-go)" \
  --go_out=. --go_opt=paths=source_relative internal/nativeprotocol/indexes.proto internal/nativeprotocol/topics.proto internal/nativeprotocol/objects.proto internal/nativeprotocol/columnstore.proto internal/nativeprotocol/intents.proto
