#!/usr/bin/env bash
# Run one shard of the live integration suite.
#
#   scripts/live-shard.sh SHARD SHARDS [go test flags...]
#
# The suite runs longer than one hosted job may wait for it, so CI splits it.
# The tests are listed by name, sorted, and dealt round robin: shard SHARD of
# SHARDS runs the tests whose position modulo SHARDS is SHARD-1. Dealing them
# out spreads the slow host-driven tests, which sort next to each other,
# across every shard. Every listed test runs in exactly one shard, and a shard
# that would receive none fails rather than report a pass for nothing.
set -euo pipefail

shard=${1:?usage: live-shard.sh SHARD SHARDS [go test flags...]}
shards=${2:?usage: live-shard.sh SHARD SHARDS [go test flags...]}
shift 2
if ! [[ "$shard" =~ ^[1-9][0-9]*$ && "$shards" =~ ^[1-9][0-9]*$ ]] || ((shard > shards)); then
	printf 'shard %s of %s is not a shard\n' "$shard" "$shards" >&2
	exit 2
fi

listed=$(go test -tags integration -list '^Test' ./integration | grep '^Test' | LC_ALL=C sort)
selected=$(awk -v shard="$shard" -v shards="$shards" '(NR - 1) % shards == shard - 1' <<<"$listed")
if [[ -z "$selected" ]]; then
	printf 'shard %s of %s received no tests\n' "$shard" "$shards" >&2
	exit 1
fi
printf 'shard %s of %s runs %s of %s tests\n' "$shard" "$shards" "$(wc -l <<<"$selected" | tr -d ' ')" "$(wc -l <<<"$listed" | tr -d ' ')"
pattern="^($(paste -sd '|' - <<<"$selected"))\$"
exec go test -tags integration ./integration -run "$pattern" "$@"
