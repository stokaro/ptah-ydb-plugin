// Package devclean lists and empties a YDB dev database, and removes the dev
// realm a run created, as the Ptah host's embedded YDB engine does
// (internal/dbschema/ydb, Writer.ResetObjects, DropDatabaseRealm and
// RemoveRealm).
//
// A dev database is the root a session treats as its database: a dev realm, a
// directory a run created in a database the operator named, or a whole
// database on a server the run owns. A run claims one only when it is empty,
// and empties it before and after it uses it. The list and the cleanup read the
// same walk, so a database judged empty is one the cleanup leaves as it is.
//
// The walk visits each directory's entries in name order, a directory after
// its contents. It leaves alone what belongs to the server, a name that starts
// with a dot, and at the root of a database the coordination node the locks
// live on and the directory that holds the dev realms, which belong to other
// runs. The cleanup drops every transfer and streaming query first, then every
// async replication, then external tables and column tables, then external
// data sources, then the other objects, then secrets, and removes every
// directory below the root, deepest first. An object it has no statement for
// stops it before it drops anything.
package devclean

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// RealmDirectory is the directory at the root of a database that holds the
// dev realms.
const RealmDirectory = "ptah_dev"

// LockNode is the coordination node at the root of a database that the
// locks live on.
const LockNode = "ptah_locks"

// EntryStreamingQuery is the scheme entry type of a streaming query, which
// the pinned YDB bindings do not name.
const EntryStreamingQuery Ydb_Scheme.Entry_Type = 24

// Scheme is what the walk needs of YDB's scheme service. Paths are absolute.
type Scheme interface {
	ListDirectory(ctx context.Context, absolute string) ([]*Ydb_Scheme.Entry, error)
	RemoveDirectory(ctx context.Context, absolute string) error
}

// Runner runs the statements of a cleanup. A statement names its object by a
// path relative to the root the runner resolves names against.
type Runner interface {
	// Execute runs one schema statement.
	Execute(ctx context.Context, statement string) error
	// DropNode drops the coordination node at an absolute path.
	DropNode(ctx context.Context, absolute string) error
}

// Object is one object a cleanup finds: its kind in lower-case words, the
// directory that holds it relative to the root, and its name.
type Object struct {
	Kind   string
	Schema string
	Name   string
}

// Step is what removes one object: a statement, the drop of a coordination
// node, or the removal of a directory.
type Step struct {
	// Statement is the statement that drops the object, as the host writes
	// it. A coordination node's is Ptah's own statement, which runs through
	// the coordination service.
	Statement string
	// Node is the absolute path of a coordination node the step drops.
	Node string
	// Directory is the absolute path of a directory the step removes.
	Directory string
	// RowTable marks a row table the listing found, whose SQL visibility may
	// lag behind the listing.
	RowTable bool
	rank     int
}

// Entry is one object of a walk and the step that removes it. A zero step is
// an object there is no statement for.
type Entry struct {
	Object Object
	Step   Step
}

// Droppable reports an entry the cleanup has a step for.
func (e Entry) Droppable() bool { return e.Step != (Step{}) }

// Tree is a directory tree a walk reads.
type Tree struct {
	// Root is the absolute path of the tree.
	Root string
	// Base is written in front of each object's path in the statement that
	// drops it: empty when the runner resolves names against Root.
	Base string
	// Database is the absolute path of the database that holds Root. At the
	// root of a database the lock node and the realms' directory are left
	// alone.
	Database string
	// Everything walks every entry, a name that starts with a dot included,
	// as the removal of a realm does: every object in a realm is the run's.
	Everything bool
	// KeepLocks leaves the lock node at the root alone even where the root
	// is a realm. A session keeps its locks at the root it treats as its
	// database, a realm's included, and a lock it holds is storage the
	// cleanup leaves out.
	KeepLocks bool
}

// Walk lists what the directory dir, relative to the tree's root, holds, each
// directory after its contents, in name order. An empty dir walks the root.
func Walk(ctx context.Context, scheme Scheme, tree Tree, dir string) ([]Entry, error) {
	var entries []Entry
	if err := walk(ctx, scheme, tree, dir, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func walk(ctx context.Context, scheme Scheme, tree Tree, dir string, entries *[]Entry) error {
	listed, err := scheme.ListDirectory(ctx, path.Join(tree.Root, dir))
	if err != nil {
		return err
	}
	listed = slices.Clone(listed)
	slices.SortFunc(listed, func(a, b *Ydb_Scheme.Entry) int { return strings.Compare(a.GetName(), b.GetName()) })
	for _, entry := range listed {
		if !tree.Everything && leftAlone(tree, dir, entry) {
			continue
		}
		name := entry.GetName()
		child := path.Join(dir, name)
		object := Object{Kind: objectKind(entry.GetType()), Schema: dir, Name: name}
		if entry.GetType() == Ydb_Scheme.Entry_DIRECTORY {
			if err := walk(ctx, scheme, tree, child, entries); err != nil {
				return err
			}
			*entries = append(*entries, Entry{Object: object, Step: Step{Directory: path.Join(tree.Root, child),
				rank: teardownRank(Ydb_Scheme.Entry_DIRECTORY)}})
			continue
		}
		*entries = append(*entries, Entry{Object: object, Step: dropStep(entry.GetType(), path.Join(tree.Base, child), path.Join(tree.Root, child))})
	}
	return nil
}

// leftAlone reports whether a cleanup of the root leaves entry, in the
// directory dir relative to the root, where it is.
func leftAlone(tree Tree, dir string, entry *Ydb_Scheme.Entry) bool {
	name := entry.GetName()
	switch {
	case strings.HasPrefix(name, "."):
		return true
	case dir == "" && tree.KeepLocks && entry.GetType() == Ydb_Scheme.Entry_COORDINATION_NODE && name == LockNode:
		return true
	case dir != "" || tree.Root != tree.Database:
		return false
	case entry.GetType() == Ydb_Scheme.Entry_DIRECTORY && name == RealmDirectory:
		return true
	default:
		return entry.GetType() == Ydb_Scheme.Entry_COORDINATION_NODE && name == LockNode
	}
}

// treeStatements are the statements that drop an entry, by its type.
var treeStatements = map[Ydb_Scheme.Entry_Type]string{
	EntryStreamingQuery:                   "DROP STREAMING QUERY %s",
	Ydb_Scheme.Entry_EXTERNAL_TABLE:       "DROP EXTERNAL TABLE %s",
	Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE: "DROP EXTERNAL DATA SOURCE %s",
	Ydb_Scheme.Entry_TABLE:                "DROP TABLE %s",
	Ydb_Scheme.Entry_COLUMN_TABLE:         "DROP TABLE %s",
	Ydb_Scheme.Entry_VIEW:                 "DROP VIEW %s",
	Ydb_Scheme.Entry_TOPIC:                "DROP TOPIC %s",
	Ydb_Scheme.Entry_SECRET:               "DROP SECRET %s",
	Ydb_Scheme.Entry_TRANSFER:             "DROP TRANSFER %s",
	Ydb_Scheme.Entry_REPLICATION:          "DROP ASYNC REPLICATION %s",
}

// dropStep is the step that drops an entry of entryType at target, a path the
// runner resolves, and at absolute, its absolute path; a zero step for a kind
// there is no statement for.
func dropStep(entryType Ydb_Scheme.Entry_Type, target, absolute string) Step {
	if entryType == Ydb_Scheme.Entry_COORDINATION_NODE {
		return Step{Statement: "DROP COORDINATION NODE " + Quote(target), Node: absolute, rank: teardownRank(entryType)}
	}
	statement, droppable := treeStatements[entryType]
	if !droppable {
		return Step{}
	}
	return Step{Statement: fmt.Sprintf(statement, Quote(target)), RowTable: entryType == Ydb_Scheme.Entry_TABLE,
		rank: teardownRank(entryType)}
}

// Quote writes name as a YQL identifier: in backticks, with a backtick or a
// backslash inside escaped by a backslash.
func Quote(name string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(name) + "`"
}

// teardownRank orders the steps of a teardown: transfers and streaming
// queries first and then replications, before the tables and topics they read
// and write go, and external readers before their sources, then ordinary
// objects, secrets and directories. Entries at the same rank keep the order
// the walk met them.
func teardownRank(entryType Ydb_Scheme.Entry_Type) int {
	switch entryType {
	case Ydb_Scheme.Entry_TRANSFER, EntryStreamingQuery:
		return 0
	case Ydb_Scheme.Entry_REPLICATION:
		return 1
	case Ydb_Scheme.Entry_EXTERNAL_TABLE, Ydb_Scheme.Entry_COLUMN_TABLE:
		return 2
	case Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE:
		return 3
	case Ydb_Scheme.Entry_SECRET:
		return 5
	case Ydb_Scheme.Entry_DIRECTORY:
		return 6
	default:
		return 4
	}
}

// objectKind names a scheme entry type in lower-case words, as a refusal names
// the object: "table", "column table", "coordination node".
func objectKind(entryType Ydb_Scheme.Entry_Type) string {
	return strings.ToLower(strings.ReplaceAll(entryTypeName(entryType), "_", " "))
}

func entryTypeName(entryType Ydb_Scheme.Entry_Type) string {
	if entryType == EntryStreamingQuery {
		return "STREAMING_QUERY"
	}
	if name, known := Ydb_Scheme.Entry_Type_name[int32(entryType)]; known {
		return name
	}
	return fmt.Sprintf("scheme entry of type %d", int32(entryType))
}

// Describe names an object with its directory, as an error reads it.
func Describe(object Object) string {
	if object.Schema == "" {
		return fmt.Sprintf("%s %q", object.Kind, object.Name)
	}
	return fmt.Sprintf("%s %q in directory %q", object.Kind, object.Name, object.Schema)
}

// Blocking lists the entries the cleanup has no statement for.
func Blocking(entries []Entry) []Object {
	var blocking []Object
	for _, entry := range entries {
		if !entry.Droppable() {
			blocking = append(blocking, entry.Object)
		}
	}
	return blocking
}

// Refusal is why the cleanup stops at an object it has no statement for.
func Refusal(object Object) string {
	return "Ptah has no statement to drop a " + object.Kind
}

// ErrBlocked is a cleanup that dropped nothing because the tree holds an
// object it has no statement for.
var ErrBlocked = errors.New("the dev database holds an object there is no statement to drop; nothing was dropped")

// Run drops what entries hold in teardown order. It refuses, with
// ErrBlocked and before it drops anything, a tree that holds an object there
// is no statement for. started reports whether a step was dispatched, so a
// failure after it leaves the tree part way.
func Run(ctx context.Context, scheme Scheme, runner Runner, entries []Entry, extra ...Step) (started bool, err error) {
	if len(Blocking(entries)) > 0 {
		return false, ErrBlocked
	}
	steps := make([]Step, 0, len(entries)+len(extra))
	for _, entry := range entries {
		steps = append(steps, entry.Step)
	}
	for _, step := range extra {
		step.rank = teardownRank(Ydb_Scheme.Entry_DIRECTORY)
		steps = append(steps, step)
	}
	slices.SortStableFunc(steps, func(a, b Step) int { return cmp.Compare(a.rank, b.rank) })
	for _, step := range steps {
		started = true
		if err := runStep(ctx, scheme, runner, step); err != nil {
			return true, err
		}
	}
	return started, nil
}

// tableSettleBudget bounds the wait for a row table the listing named to
// become one a DROP TABLE finds: the listing can name a table a moment before
// SQL sees it.
const tableSettleBudget = 10 * time.Second

func runStep(ctx context.Context, scheme Scheme, runner Runner, step Step) error {
	switch {
	case step.Node != "":
		return runner.DropNode(ctx, step.Node)
	case step.RowTable:
		return dropListedTable(ctx, runner, step.Statement)
	case step.Statement != "":
		return runner.Execute(ctx, step.Statement)
	default:
		if err := scheme.RemoveDirectory(ctx, step.Directory); err != nil {
			return fmt.Errorf("remove the directory %s: %w", step.Directory, err)
		}
		return nil
	}
}

// ErrNotVisible is what a runner answers for a statement whose table the
// listing named and SQL does not see yet.
var ErrNotVisible = errors.New("the table is not visible to SQL yet")

// dropListedTable drops a row table the listing named, and runs the statement
// again while SQL does not see the table yet, within tableSettleBudget.
func dropListedTable(ctx context.Context, runner Runner, statement string) error {
	err := runner.Execute(ctx, statement)
	ctx, cancel := context.WithTimeout(ctx, tableSettleBudget)
	defer cancel()
	wait := 50 * time.Millisecond
	for errors.Is(err, ErrNotVisible) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w; stopped waiting to drop the listed table: %w", err, ctx.Err())
		case <-time.After(wait):
		}
		err = runner.Execute(ctx, statement)
		wait = min(2*wait, time.Second)
	}
	return err
}

// ErrScheme marks a scheme service answer of SCHEME_ERROR: a path that does
// not exist, or a directory the server refuses to remove because it is not
// empty. A [Scheme] wraps it into such an answer.
var ErrScheme = errors.New("YDB answered SCHEME_ERROR")

// RemoveRealm removes the realm name of the database at database, with
// everything in it, and then the realms' directory when no realm is left in
// it. Every object in the realm is the run's, a name that starts with a dot
// included. The whole realm is read and checked first: an object there is no
// statement for stops it before anything is dropped. A realm that does not
// exist is removed already. The runner resolves names against the database.
func RemoveRealm(ctx context.Context, scheme Scheme, runner Runner, database, name string) error {
	parent := path.Join(database, RealmDirectory)
	listed, err := scheme.ListDirectory(ctx, parent)
	if errors.Is(err, ErrScheme) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(listed, func(entry *Ydb_Scheme.Entry) bool { return entry.GetName() == name }) {
		return nil
	}
	relative := path.Join(RealmDirectory, name)
	absolute := path.Join(database, relative)
	entries, err := Walk(ctx, scheme, Tree{Root: absolute, Base: relative, Database: database, Everything: true}, "")
	if err != nil {
		return err
	}
	if blocking := Blocking(entries); len(blocking) > 0 {
		return fmt.Errorf("the dev realm %s holds %s, which Ptah has no statement to drop; nothing was dropped",
			absolute, Describe(blocking[0]))
	}
	if _, err := Run(ctx, scheme, runner, entries, Step{Directory: absolute}); err != nil {
		return err
	}
	return removeIfEmpty(ctx, scheme, parent)
}

// removeIfEmpty removes the realms' directory when no realm is left in it.
// Another run may create a realm in the moment between the listing and the
// removal; the server then refuses to remove a directory that is not empty,
// and the directory stays for that run.
func removeIfEmpty(ctx context.Context, scheme Scheme, parent string) error {
	left, err := scheme.ListDirectory(ctx, parent)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return nil
	}
	if err := scheme.RemoveDirectory(ctx, parent); err != nil && !errors.Is(err, ErrScheme) {
		return fmt.Errorf("remove the directory %s: %w", parent, err)
	}
	return nil
}
