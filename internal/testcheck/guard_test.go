package testcheck_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// minTestFiles sits below the number of test files the repository holds. A
// listing that stopped matching would read as a tree where no test calls
// quicktest's constructor, which is what a clean tree reads as too.
const minTestFiles = 250

// direct matches a checker built by quicktest's own constructor, under the
// import name the tests use or the package's own.
var direct = regexp.MustCompile(`\b(qt|quicktest)\.New\(`)

// Every test asserts through a checker testcheck.New built. One built by
// quicktest prints a failed slice of protobuf messages through kr/pretty, and
// nothing would notice until such a check failed on CI and ran for hours. The
// files come from git rather than the filesystem, so a checkout parked inside
// the repository is not read as this one's.
func TestEveryTestAssertsThroughTestcheck(t *testing.T) {
	c := testcheck.New(t)
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	c.Assert(err, qt.IsNil)
	root := strings.TrimSpace(string(output))
	listed, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "--", "*_test.go").Output()
	c.Assert(err, qt.IsNil)
	files := strings.Fields(string(listed))

	c.Assert(len(files) >= minTestFiles, qt.IsTrue, qt.Commentf("read %d test files", len(files)))
	c.Assert(directCallers(c, root, files), qt.DeepEquals, []string(nil),
		qt.Commentf("build the checker with testcheck.New(t), which prints protobuf messages as text"))
}

// directCallers returns the files among files that build a checker with
// quicktest's constructor.
func directCallers(c *qt.C, root string, files []string) []string {
	var callers []string
	for _, name := range files {
		source, err := os.ReadFile(filepath.Join(root, name))
		c.Assert(err, qt.IsNil)
		if direct.Match(source) {
			callers = append(callers, name)
		}
	}
	return callers
}
