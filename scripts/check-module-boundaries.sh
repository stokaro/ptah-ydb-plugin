#!/bin/sh
# Providers consume the published neutral SDK without a workspace or host module.
# The SDK and the SQL syntax module live under the host's path, ptah.run, and
# are the only modules under it a provider may require.
set -eu
export GOWORK=off
modules=$(go list -m all)
printf '%s\n' "$modules" | awk '$1 ~ /^ptah[.]run($|\/)/ && $1 != "ptah.run/provider-sdk" && $1 != "ptah.run/sqlsyntax" { print "Forbidden host dependency: " $1; bad=1 } END { exit bad }'
test -z "$(go list -m -f '{{if .Replace}}{{.Path}}{{end}}' all)"
go list -m ptah.run/provider-sdk github.com/ydb-platform/ydb-go-sdk/v3
