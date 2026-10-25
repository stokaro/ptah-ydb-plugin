// Package releaseline reads the YDB release line a server reports, so a
// capability that arrived in a release can be decided from the version a
// session measured.
package releaseline

import (
	"strconv"
	"strings"
)

// Parse reads the major and minor release out of a YDB version: dotted, such
// as "26.2.1.14", or a build branch, such as "stable-25-4-1".
func Parse(version string) (major, minor int, ok bool) {
	trimmed := strings.ToLower(strings.TrimSpace(version))
	separator := "."
	if rest, branch := strings.CutPrefix(trimmed, "stable-"); branch {
		trimmed, separator = rest, "-"
	}
	parts := strings.Split(trimmed, separator)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// AtLeast reports whether version is release major.minor or later. A version
// that does not read as a release is taken as the newest line's.
func AtLeast(version string, major, minor int) bool {
	have, haveMinor, ok := Parse(version)
	return !ok || have > major || have == major && haveMinor >= minor
}
