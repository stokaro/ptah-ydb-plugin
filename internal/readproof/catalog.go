package readproof

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// ErrReadLeavesDatabase is wrapped by every refusal of [ProveLocalReads] that
// found a read whose rows come from outside the database: an external table,
// read by name or through a view.
var ErrReadLeavesDatabase = errors.New("the query reads rows from outside the database")

// ReadCatalog answers what [ProveLocalReads] asks of a database: its root, the
// type of the object at an absolute path, and the query a view at an absolute
// path stores.
type ReadCatalog interface {
	Database() string
	EntryType(ctx context.Context, absolute string) (Ydb_Scheme.Entry_Type, error)
	ViewQuery(ctx context.Context, absolute string) (string, error)
}

// ProveLocalReads proves that query, a YQL SELECT sent to a database whose
// relative names mean the directory root, reads only rows the database
// stores, and returns an error naming the first read it cannot prove.
//
// The text names what the query reads (see [ReadSources]), and
// the catalog says what each name is. A row table, a column table and a
// system view are read where they are stored. A view is followed into the
// query it stores, to a depth of [MaxNesting]. Every SELECT, the
// query's own and each view's, is held to [ReadSources] too, and one that
// reaches outside the database is refused with [ErrReadLeavesDatabase], as an
// external table is: measured on 25.1.4.7 and 26.2.1.14, a SELECT over one in
// a read-only transaction lists and fetches its files from the object storage
// its data source names, and a SELECT over a view that reads one does the
// same. Every other object, and a name the catalog cannot find, is refused
// too, since nothing then says where its rows come from.
//
// The proof is as old as the catalog's answers. An object created, replaced or
// dropped between the proof and the query is not seen.
func ProveLocalReads(ctx context.Context, catalog ReadCatalog, root, query string) error {
	return proveLocalReads(ctx, catalog, root, query, 0)
}

func proveLocalReads(ctx context.Context, catalog ReadCatalog, root, query string, depth int) error {
	reads, err := ReadSources(query)
	if err != nil {
		return err
	}
	base := root
	if reads.Prefix != "" {
		base = absolutePath(root, reads.Prefix)
	}
	for _, source := range reads.Sources {
		target := absolutePath(base, source)
		if target != catalog.Database() && !strings.HasPrefix(target, strings.TrimRight(catalog.Database(), "/")+"/") {
			return fmt.Errorf("%w: %s is outside the session database", ErrReadLeavesDatabase, target)
		}
		entryType, err := catalog.EntryType(ctx, target)
		if err != nil {
			return fmt.Errorf("the query reads %s, which cannot be described: %w", target, err)
		}
		switch entryType {
		case Ydb_Scheme.Entry_TABLE, Ydb_Scheme.Entry_COLUMN_TABLE, Ydb_Scheme.Entry_SYS_VIEW:
			continue
		case Ydb_Scheme.Entry_EXTERNAL_TABLE:
			return fmt.Errorf("%w: %s is an external table, whose rows the server fetches from its data source",
				ErrReadLeavesDatabase, target)
		case Ydb_Scheme.Entry_VIEW:
			if err := proveViewReads(ctx, catalog, target, depth); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the query reads %s, a %s, which the proof does not follow",
				target, entryType.String())
		}
	}
	return nil
}

// proveViewReads proves the query the view at target stores. A name in it
// means a path under the database root, unless a TablePathPrefix pragma the
// view was created under, which the server stores with the query, says
// otherwise.
func proveViewReads(ctx context.Context, catalog ReadCatalog, target string, depth int) error {
	if depth >= MaxNesting {
		return fmt.Errorf("the query reads the view %s through more than %d views, which the proof does not follow",
			target, MaxNesting)
	}
	stored, err := catalog.ViewQuery(ctx, target)
	if err != nil {
		return fmt.Errorf("the query reads the view %s, whose query cannot be read: %w", target, err)
	}
	if err := proveLocalReads(ctx, catalog, catalog.Database(), stored, depth+1); err != nil {
		return fmt.Errorf("the query reads the view %s: %w", target, err)
	}
	return nil
}

// absolutePath is name, a path as a query writes it, under base unless it is
// absolute already.
func absolutePath(base, name string) string {
	if strings.HasPrefix(name, "/") {
		return path.Clean(name)
	}
	return path.Join(base, name)
}
