#!/usr/bin/env sh
# Hold the test files to the host's declarative-test rule: no if, switch or goto
# in a test function. The violations that existed when the rule was adopted sit
# in .teststyle-baseline.json. The baseline may shrink and must not grow.
#
#   scripts/check-test-style.sh                    check against the baseline
#   scripts/check-test-style.sh --write-baseline   record the current violations
#
# teststyle is never handed the repository to walk. It prunes by directory name
# only, and the root of a linked worktree is an ordinary directory whose .git is
# a file, so a walk would read every checkout parked under .claude/worktrees as
# this one's. The files git lists are copied to a scratch root instead: tracked
# files, and new ones not yet added, so the gate fires before `git add`.
set -eu

mode=check
case "${1-}" in
"") ;;
--write-baseline) mode=write ;;
*)
	echo "usage: $0 [--write-baseline]" >&2
	exit 2
	;;
esac

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/teststyle.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT INT TERM
scan_root="$work_dir/root"
path_list="$work_dir/paths"
mkdir -p "$scan_root"

# The list is written to a file first: a failing git in a pipe would scan
# nothing and still report success.
git -c core.quotePath=false ls-files --cached --others --exclude-standard -- '*_test.go' >"$path_list"
if [ ! -s "$path_list" ]; then
	echo "teststyle: git lists no test files; refusing to report success" >&2
	exit 1
fi

while IFS= read -r path; do
	if [ ! -f "$path" ]; then
		echo "teststyle: skipping $path (not a regular file in the working tree)" >&2
		continue
	fi
	mkdir -p "$scan_root/$(dirname "$path")"
	cp "$path" "$scan_root/$path"
done <"$path_list"

# teststyle reports paths relative to -root, so the baseline keys are the
# repository's paths. -baseline is resolved against the working directory, so
# it is absolute. Example functions are documentation and are not judged.
if [ "$mode" = write ]; then
	GOWORK=off go tool teststyle -skip-examples -baseline "$repo_root/.teststyle-baseline.json" -write-baseline -root "$scan_root"
	exit 0
fi
GOWORK=off go tool teststyle -skip-examples -baseline "$repo_root/.teststyle-baseline.json" -root "$scan_root"
