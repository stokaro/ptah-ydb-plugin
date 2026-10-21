#!/bin/sh
# Providers consume the published neutral SDK without a workspace or host module.
set -eu
export GOWORK=off
modules=$(go list -m all)
printf '%s\n' "$modules" | awk '$1 ~ /^ptah[.]run($|\/)/ { print "Forbidden host dependency: " $1; bad=1 } END { exit bad }'
test -z "$(go list -m -f '{{if .Replace}}{{.Path}}{{end}}' all)"
go list -m github.com/stokaro/ptah/provider-sdk github.com/ydb-platform/ydb-go-sdk/v3
