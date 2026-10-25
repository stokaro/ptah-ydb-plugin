package devclean_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/devclean"
)

// The corpus holds scheme trees and what the host's embedded YDB engine did
// with each, recorded by testdata/oracle at one host commit: the objects
// Writer.ResetObjects listed, the statements and directory removals of
// Writer.DropDatabaseRealm, and those of Writer.RemoveRealm where the case
// names a realm to remove. The host words a refusal as an error; the provider
// reports the same refusal as REFUSED with the objects that stopped it.

type corpusEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type corpusCase struct {
	Database    string                   `json:"database"`
	Realm       string                   `json:"realm"`
	RemoveRealm string                   `json:"remove_realm"`
	Tree        map[string][]corpusEntry `json:"tree"`
}

type corpusRun struct {
	Executed []string `json:"executed"`
	Removed  []string `json:"removed"`
	Error    string   `json:"error"`
}

type corpusGolden struct {
	HostCommit  string            `json:"host_commit"`
	Objects     []devclean.Object `json:"objects"`
	Drop        corpusRun         `json:"drop"`
	RemoveRealm *corpusRun        `json:"remove_realm"`
}

type corpusRow struct {
	name   string
	input  corpusCase
	golden corpusGolden
}

func corpus(c *qt.C) []corpusRow {
	c.Helper()
	paths, err := filepath.Glob("testdata/corpus/*.golden.json")
	c.Assert(err, qt.IsNil)
	slices.Sort(paths)
	var rows []corpusRow
	for _, golden := range paths {
		name := strings.TrimSuffix(filepath.Base(golden), ".golden.json")
		var row corpusRow
		row.name = name
		readJSON(c, filepath.Join("testdata/corpus", name+".json"), &row.input)
		readJSON(c, golden, &row.golden)
		rows = append(rows, row)
	}
	c.Assert(len(rows) >= 11, qt.IsTrue, qt.Commentf("%d corpus cases", len(rows)))
	return rows
}

func readJSON(c *qt.C, file string, target any) {
	c.Helper()
	data, err := os.ReadFile(file)
	c.Assert(err, qt.IsNil)
	c.Assert(json.Unmarshal(data, target), qt.IsNil)
}

// fakeTree is a scheme tree the cleanup lists and changes. root is what the
// runner resolves a statement's names against.
type fakeTree struct {
	root     string
	tree     map[string][]*Ydb_Scheme.Entry
	executed []string
	removed  []string
}

func newFakeTree(c *qt.C, input corpusCase, root string) *fakeTree {
	c.Helper()
	f := &fakeTree{root: root, tree: map[string][]*Ydb_Scheme.Entry{}}
	for dir, entries := range input.Tree {
		f.tree[dir] = []*Ydb_Scheme.Entry{}
		for _, entry := range entries {
			kind, known := Ydb_Scheme.Entry_Type_value[entry.Type]
			c.Assert(known, qt.IsTrue)
			f.tree[dir] = append(f.tree[dir], &Ydb_Scheme.Entry{Name: entry.Name, Type: Ydb_Scheme.Entry_Type(kind)})
		}
	}
	return f
}

func (f *fakeTree) ListDirectory(_ context.Context, dir string) ([]*Ydb_Scheme.Entry, error) {
	entries, ok := f.tree[dir]
	if !ok {
		return nil, fmt.Errorf("%w: listed %s, which the corpus does not hold", devclean.ErrScheme, dir)
	}
	return slices.Clone(entries), nil
}

func (f *fakeTree) RemoveDirectory(_ context.Context, dir string) error {
	f.removed = append(f.removed, dir)
	f.drop(dir)
	delete(f.tree, dir)
	return nil
}

func (f *fakeTree) drop(absolute string) {
	parent, name := path.Split(absolute)
	parent = strings.TrimSuffix(parent, "/")
	if entries, ok := f.tree[parent]; ok {
		f.tree[parent] = slices.DeleteFunc(entries, func(entry *Ydb_Scheme.Entry) bool { return entry.GetName() == name })
	}
}

func (f *fakeTree) Execute(_ context.Context, statement string) error {
	f.executed = append(f.executed, statement)
	_, target, _ := strings.Cut(statement, "`")
	f.drop(path.Join(f.root, strings.ReplaceAll(strings.TrimSuffix(target, "`"), "\\`", "`")))
	return nil
}

func (f *fakeTree) DropNode(_ context.Context, absolute string) error {
	f.executed = append(f.executed, "DROP COORDINATION NODE "+devclean.Quote(strings.TrimPrefix(absolute, f.root+"/")))
	f.drop(absolute)
	return nil
}

func treeRoot(input corpusCase) string {
	if input.Realm == "" {
		return input.Database
	}
	return path.Join(input.Database, devclean.RealmDirectory, input.Realm)
}

func TestWalkListsWhatTheHostLists(t *testing.T) {
	for _, row := range corpus(qt.New(t)) {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			root := treeRoot(row.input)
			fake := newFakeTree(c, row.input, root)

			entries, err := devclean.Walk(context.Background(), fake, devclean.Tree{Root: root, Database: row.input.Database}, "")

			c.Assert(err, qt.IsNil)
			objects := make([]devclean.Object, len(entries))
			for i, entry := range entries {
				objects[i] = entry.Object
			}
			c.Assert(objects, qt.DeepEquals, row.golden.Objects)
		})
	}
}

// cleaned is what one cleanup did, in the golden form.
func cleaned(fake *fakeTree, err error) corpusRun {
	run := corpusRun{Executed: slices.Clone(fake.executed), Removed: slices.Clone(fake.removed)}
	if run.Executed == nil {
		run.Executed = []string{}
	}
	if run.Removed == nil {
		run.Removed = []string{}
	}
	if err != nil {
		run.Error = "ydb: " + err.Error()
	}
	return run
}

func TestRunDropsWhatTheHostDrops(t *testing.T) {
	for _, row := range corpus(qt.New(t)) {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			root := treeRoot(row.input)
			fake := newFakeTree(c, row.input, root)
			entries, err := devclean.Walk(context.Background(), fake, devclean.Tree{Root: root, Database: row.input.Database}, "")
			c.Assert(err, qt.IsNil)

			_, err = devclean.Run(context.Background(), fake, fake, entries)

			got := cleaned(fake, err)
			c.Assert(got.Executed, qt.DeepEquals, row.golden.Drop.Executed)
			c.Assert(got.Removed, qt.DeepEquals, row.golden.Drop.Removed)
			c.Assert(got.Error != "", qt.Equals, row.golden.Drop.Error != "")
		})
	}
}

// TestRunNamesWhatStopsIt pins the refusal of a tree that holds an object
// there is no statement for to the object the host names.
func TestRunNamesWhatStopsIt(t *testing.T) {
	c := qt.New(t)
	refused := 0
	for _, row := range corpus(c) {
		root := treeRoot(row.input)
		fake := newFakeTree(c, row.input, root)
		entries, err := devclean.Walk(context.Background(), fake, devclean.Tree{Root: root, Database: row.input.Database}, "")
		c.Assert(err, qt.IsNil)
		for _, object := range devclean.Blocking(entries) {
			refused++
			c.Assert(row.golden.Drop.Error, qt.Equals, fmt.Sprintf("ydb: %s holds %s, which Ptah has no statement to drop; nothing was dropped",
				root, devclean.Describe(object)), qt.Commentf("%s", row.name))
		}
	}
	c.Assert(refused, qt.Equals, 2)
}

func TestRemoveRealmRemovesWhatTheHostRemoves(t *testing.T) {
	c := qt.New(t)
	rows := slices.DeleteFunc(corpus(c), func(row corpusRow) bool { return row.input.RemoveRealm == "" })
	c.Assert(len(rows) >= 4, qt.IsTrue)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)
			fake := newFakeTree(c, row.input, row.input.Database)

			err := devclean.RemoveRealm(context.Background(), fake, fake, row.input.Database, row.input.RemoveRealm)

			c.Assert(cleaned(fake, err), qt.DeepEquals, *row.golden.RemoveRealm)
		})
	}
}

// A session keeps its locks on the lock node at the root it treats as its
// database, a realm's included, so a cleanup that keeps locks leaves that one
// node alone and walks a node of the same name anywhere else.
func TestWalkKeepsTheSessionsLockNode(t *testing.T) {
	realm := path.Join("/local", devclean.RealmDirectory, "r1")
	fake := newFakeTree(qt.New(t), corpusCase{Tree: map[string][]corpusEntry{
		realm:             {{Name: devclean.LockNode, Type: "COORDINATION_NODE"}, {Name: "nested", Type: "DIRECTORY"}},
		realm + "/nested": {{Name: devclean.LockNode, Type: "COORDINATION_NODE"}},
	}}, realm)
	for _, test := range []struct {
		name string
		keep bool
		want []string
	}{
		{name: "keeping locks", keep: true, want: []string{"nested/" + devclean.LockNode, "/nested"}},
		{name: "not keeping them", keep: false, want: []string{"nested/" + devclean.LockNode, "/nested", "/" + devclean.LockNode}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			entries, err := devclean.Walk(context.Background(), fake, devclean.Tree{Root: realm, Database: "/local", KeepLocks: test.keep}, "")

			c.Assert(err, qt.IsNil)
			walked := make([]string, 0, len(entries))
			for _, entry := range entries {
				walked = append(walked, entry.Object.Schema+"/"+entry.Object.Name)
			}
			c.Assert(walked, qt.DeepEquals, test.want)
		})
	}
}
