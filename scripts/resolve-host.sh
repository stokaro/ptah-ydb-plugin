#!/usr/bin/env bash
# Resolve which Ptah the live tests build as the host.
#
#   scripts/resolve-host.sh master    the head of Ptah's master branch
#   scripts/resolve-host.sh release   the newest published Ptah release whose
#                                     provider contract this provider can run on
#
# A release qualifies when its contract major equals the provider's minimum
# major and its minor is at least the provider's minimum minor. The host's
# contract is the one its pinned provider-sdk speaks: Revision is the major,
# ContractMinor the minor (absent in SDKs that predate minor versions, so 0).
#
# Writes ref=<git ref> and skip=false to $GITHUB_OUTPUT, or skip=true when no
# release qualifies yet.
set -euo pipefail

mode=${1:?usage: resolve-host.sh master|release}
out=${GITHUB_OUTPUT:-/dev/stdout}

if [[ "$mode" == master ]]; then
	printf 'ref=master\nskip=false\n' >>"$out"
	exit 0
fi
if [[ "$mode" != release ]]; then
	printf 'unknown host mode %q\n' "$mode" >&2
	exit 2
fi

manifest=$(go run ./cmd/provider-manifest -version 0.0.0)
want_major=$(jq -r '.min_contract.major' <<<"$manifest")
want_minor=$(jq -r '.min_contract.minor' <<<"$manifest")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Newest first; drafts and prereleases are not releases users install.
tags=$(gh api 'repos/stokaro/ptah/releases?per_page=30' \
	--jq '[.[] | select((.draft | not) and (.prerelease | not)) | .tag_name] | .[]')

while read -r tag; do
	[[ -n "$tag" ]] || continue
	gomod=$(gh api "repos/stokaro/ptah/contents/go.mod?ref=$tag" --jq '.content' | base64 -d)
	sdk=$(awk '$1 == "github.com/stokaro/ptah/provider-sdk" { print $2; exit }' <<<"$gomod")
	if [[ -z "$sdk" ]]; then
		# Older releases predate executable providers.
		printf 'release %s pins no provider-sdk\n' "$tag" >&2
		break
	fi
	dir=$(cd "$work" && GOFLAGS=-mod=mod go mod download -json "github.com/stokaro/ptah/provider-sdk@$sdk" | jq -r '.Dir')
	major=$(grep -hoE 'const Revision uint32 = [0-9]+' "$dir"/provider/*.go | grep -oE '[0-9]+$' | head -1)
	minor=$(grep -hoE 'const ContractMinor uint32 = [0-9]+' "$dir"/provider/*.go | grep -oE '[0-9]+$' | head -1 || true)
	minor=${minor:-0}
	printf 'release %s speaks contract %s.%s; this provider needs %s.%s\n' "$tag" "$major" "$minor" "$want_major" "$want_minor" >&2
	if [[ "$major" == "$want_major" && "$minor" -ge "$want_minor" ]]; then
		printf 'ref=%s\nskip=false\n' "$tag" >>"$out"
		exit 0
	fi
	# Contracts only grow, so an older release cannot speak a newer major.
	if [[ "$major" -lt "$want_major" ]]; then
		break
	fi
done <<<"$tags"

printf '::notice::No published Ptah release speaks contract %s.%s or a later minor of it; the release leg is skipped.\n' "$want_major" "$want_minor"
printf 'skip=true\n' >>"$out"
