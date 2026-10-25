// Command ydbdevcleanoracle records what the Ptah host's embedded YDB engine
// lists and drops when it cleans a dev database and removes a dev realm, over
// the scheme trees of the provider's corpus, so the provider's cleanup can be
// compared with it without importing host code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages that sit behind an internal boundary, so it has to be copied
// into a checkout of the host and run from there:
//
//	mkdir -p <host>/cmd/ydbdevcleanoracle
//	cp internal/devclean/testdata/oracle/main.go <host>/cmd/ydbdevcleanoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/ydbdevcleanoracle <plugin>/internal/devclean/testdata/corpus)
//
// Each <case>.json holds a scheme tree: the entries of each absolute
// directory, the database, the realm the writer treats as its root (empty for
// the database itself), and optionally a realm to remove through the
// database's writer. The program writes <case>.golden.json: the objects
// Writer.ResetObjects lists, the statements and directory removals
// Writer.DropDatabaseRealm runs over a copy of the tree, or its error, and the
// same for Writer.RemoveRealm.
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"ptah.run/internal/dbreset"
	ydbschema "ptah.run/internal/dbschema/ydb"
)

type entry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type input struct {
	Database    string             `json:"database"`
	Realm       string             `json:"realm"`
	RemoveRealm string             `json:"remove_realm"`
	Tree        map[string][]entry `json:"tree"`
}

type object struct {
	Kind   string `json:"kind"`
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

type run struct {
	Executed []string `json:"executed"`
	Removed  []string `json:"removed"`
	Error    string   `json:"error,omitempty"`
}

type golden struct {
	HostCommit  string   `json:"host_commit"`
	Objects     []object `json:"objects"`
	ListError   string   `json:"list_error,omitempty"`
	Drop        run      `json:"drop"`
	RemoveRealm *run     `json:"remove_realm,omitempty"`
}

// fake is a scheme tree a writer lists and changes. root is what the
// writer's runner resolves a statement's names against.
type fake struct {
	root     string
	tree     map[string][]*Ydb_Scheme.Entry
	executed []string
	removed  []string
}

func newFake(in input, realm string) (*fake, error) {
	root := in.Database
	if realm != "" {
		root = path.Join(in.Database, "ptah_dev", realm)
	}
	f := &fake{root: root, tree: map[string][]*Ydb_Scheme.Entry{}}
	for dir, entries := range in.Tree {
		f.tree[dir] = []*Ydb_Scheme.Entry{}
		for _, e := range entries {
			kind, known := Ydb_Scheme.Entry_Type_value[e.Type]
			if !known {
				return nil, fmt.Errorf("unknown entry type %q", e.Type)
			}
			f.tree[dir] = append(f.tree[dir], &Ydb_Scheme.Entry{Name: e.Name, Type: Ydb_Scheme.Entry_Type(kind)})
		}
	}
	return f, nil
}

func (f *fake) ListDirectory(_ context.Context, dir string) ([]*Ydb_Scheme.Entry, error) {
	entries, ok := f.tree[dir]
	if !ok {
		return nil, fmt.Errorf("listed %s, which the corpus does not hold", dir)
	}
	return append([]*Ydb_Scheme.Entry(nil), entries...), nil
}

func (f *fake) MakeDirectory(context.Context, string) error { return errors.New("not used") }

func (f *fake) DescribePath(context.Context, string) (*Ydb_Scheme.Entry, error) {
	return nil, errors.New("not used")
}

func (f *fake) RemoveDirectory(_ context.Context, dir string) error {
	f.removed = append(f.removed, dir)
	f.drop(dir)
	delete(f.tree, dir)
	return nil
}

func (f *fake) drop(absolute string) {
	parent, name := path.Split(absolute)
	parent = strings.TrimSuffix(parent, "/")
	var kept []*Ydb_Scheme.Entry
	for _, e := range f.tree[parent] {
		if e.GetName() != name {
			kept = append(kept, e)
		}
	}
	if _, ok := f.tree[parent]; ok {
		f.tree[parent] = kept
	}
}

func (f *fake) BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error) {
	return nil, errors.New("not used")
}

func (f *fake) Exec(query string, args ...any) (sql.Result, error) {
	return f.ExecContext(context.Background(), query, args...)
}

// ExecContext records a statement and removes the object a DROP names, a path
// the writer's runner resolves against its root.
func (f *fake) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	f.executed = append(f.executed, query)
	for _, verb := range []string{"DROP TABLE `", "DROP VIEW `", "DROP TOPIC `", "DROP SECRET `",
		"DROP TRANSFER `", "DROP ASYNC REPLICATION `", "DROP COORDINATION NODE `", "DROP STREAMING QUERY `",
		"DROP EXTERNAL TABLE `", "DROP EXTERNAL DATA SOURCE `"} {
		target, dropped := strings.CutPrefix(query, verb)
		if dropped {
			f.drop(path.Join(f.root, strings.ReplaceAll(strings.TrimSuffix(target, "`"), "\\`", "`")))
		}
	}
	return driver.RowsAffected(0), nil
}

func (f *fake) Query(string, ...any) (*sql.Rows, error) { return nil, errors.New("not used") }

func (f *fake) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("not used")
}

func (f *fake) QueryRow(string, ...any) *sql.Row { return nil }

func (f *fake) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ydbdevcleanoracle <corpus>")
		os.Exit(2)
	}
	commit := os.Getenv("HOST_COMMIT")
	if commit == "" {
		fmt.Fprintln(os.Stderr, "HOST_COMMIT is required")
		os.Exit(2)
	}
	paths, err := filepath.Glob(filepath.Join(os.Args[1], "*.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	count := 0
	for _, file := range paths {
		if strings.HasSuffix(file, ".golden.json") {
			continue
		}
		if err := record(file, commit); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, err)
			os.Exit(1)
		}
		count++
	}
	if count == 0 {
		fmt.Fprintln(os.Stderr, "the corpus holds no case")
		os.Exit(1)
	}
}

func record(file, commit string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var in input
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	ctx := context.Background()
	out := golden{HostCommit: commit, Objects: []object{}}

	lister, err := newFake(in, in.Realm)
	if err != nil {
		return err
	}
	writer := ydbschema.NewWriterFromScheme(lister, lister, in.Database, in.Realm)
	objects, err := writer.ResetObjects(ctx, dbreset.Scope{})
	if err != nil {
		out.ListError = err.Error()
	}
	for _, o := range objects {
		out.Objects = append(out.Objects, object{Kind: o.Kind, Schema: o.Schema, Name: o.Name})
	}

	dropper, err := newFake(in, in.Realm)
	if err != nil {
		return err
	}
	dropErr := ydbschema.NewWriterFromScheme(dropper, dropper, in.Database, in.Realm).DropDatabaseRealm(ctx)
	out.Drop = result(dropper, dropErr)

	if in.RemoveRealm != "" {
		remover, err := newFake(in, "")
		if err != nil {
			return err
		}
		removeErr := ydbschema.NewWriterFromScheme(remover, remover, in.Database, "").RemoveRealm(ctx, in.RemoveRealm)
		removed := result(remover, removeErr)
		out.RemoveRealm = &removed
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(strings.TrimSuffix(file, ".json")+".golden.json", append(encoded, '\n'), 0o644)
}

func result(f *fake, err error) run {
	out := run{Executed: append([]string{}, f.executed...), Removed: append([]string{}, f.removed...)}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}
