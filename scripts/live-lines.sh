#!/usr/bin/env bash
# Print the lines the live job runs: a YDB image, the feature flags its server
# starts with, and the host the job builds (see scripts/resolve-host.sh).
#
#   scripts/live-lines.sh [HOST_REF]
#
# Every run has the release lines, which block, and the master line, which
# does not. A named host ref adds both images again with that host, for a pull
# request or a manual run whose tests need a host change that master does not
# have yet: a host pull request's branch, or a commit. The ref is the argument,
# or else the first line of .github/host-ref that is neither blank nor a
# comment, so a pull request that needs a host branch says so in a file the
# review sees. A host ref leg blocks like a release leg: the run asked for it.
#
# Writes lines=<JSON array> to $GITHUB_OUTPUT.
set -euo pipefail

ref=${1:-}
if [[ -z "$ref" && -f .github/host-ref ]]; then
	ref=$(grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' .github/host-ref | head -n 1 | tr -d '[:space:]' || true)
fi

# 25.1 keeps EnableTopicTransfer at its default, off, so the transfer tests
# check the refusal there: with the flag on, dropping a table a dropped
# transfer wrote into stops 25.1.4.7 on a schemeshard VERIFY
# (`AliveChildrenCount >= 1`).
old='{"image":"ydbplatform/local-ydb:25.1.4.7","features":"enable_vector_index,enable_external_data_sources"}'
new='{"image":"ydbplatform/local-ydb:26.2.1.14","features":"enable_vector_index,enable_add_unique_index,enable_external_data_sources,enable_streaming_queries"}'

lines=$(jq -cn --argjson old "$old" --argjson new "$new" --arg ref "$ref" '
	[$old + {host: "release"}, $new + {host: "release"}, $new + {host: "master"}]
	+ if $ref == "" then [] else [$old + {host: $ref}, $new + {host: $ref}] end')
if [[ -n "$ref" ]]; then
	printf 'host ref %s adds a leg on each image\n' "$ref" >&2
fi
printf 'lines=%s\n' "$lines" >>"${GITHUB_OUTPUT:-/dev/stdout}"
