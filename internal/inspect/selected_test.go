package inspect_test

import (
	"context"
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// tree lists the directories it holds and answers every other path as YDB
// answers one it does not show.
func tree(directories map[string]*Ydb_Scheme.ListDirectoryResult) func(context.Context, string) (*Ydb_Scheme.ListDirectoryResult, error) {
	return func(_ context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error) {
		listed, found := directories[absolute]
		if !found {
			return nil, fmt.Errorf("list YDB directory: %w", inspect.ErrNotVisible)
		}
		return listed, nil
	}
}

func TestListSelected_HappyPath(t *testing.T) {
	existing := map[string]*Ydb_Scheme.ListDirectoryResult{
		"/local":     directory("/local", &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}),
		"/local/app": directory("/local/app", &Ydb_Scheme.Entry{Name: "events", Type: Ydb_Scheme.Entry_TOPIC}),
	}
	for _, test := range []struct {
		name, selected, wantName string
		wantChildren             int
	}{
		{name: "an existing directory", selected: "app", wantName: "app", wantChildren: 1},
		{name: "a directory its parent does not name", selected: "fresh", wantName: "fresh"},
		{name: "a directory below one that does not exist", selected: "fresh/nested", wantName: "nested"},
		{name: "a directory below an existing one", selected: "app/fresh", wantName: "fresh"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			listed, err := inspect.ListSelected(c.Context(), tree(existing), "/local", test.selected)
			c.Assert(err, qt.IsNil)
			c.Assert(listed.GetSelf().GetName(), qt.Equals, test.wantName)
			c.Assert(listed.GetSelf().GetType(), qt.Equals, Ydb_Scheme.Entry_DIRECTORY)
			c.Assert(listed.GetChildren(), qt.HasLen, test.wantChildren)
		})
	}
}

func TestListSelected_FailurePath(t *testing.T) {
	// The parent names hidden, so it exists and the session may not list it.
	existing := map[string]*Ydb_Scheme.ListDirectoryResult{
		"/local": directory("/local", &Ydb_Scheme.Entry{Name: "hidden", Type: Ydb_Scheme.Entry_DIRECTORY}),
	}
	for _, test := range []struct{ name, selected string }{
		{name: "a directory its parent names", selected: "hidden"},
		{name: "a directory below one its parent names", selected: "hidden/nested"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			listed, err := inspect.ListSelected(c.Context(), tree(existing), "/local", test.selected)
			c.Assert(err, qt.ErrorIs, inspect.ErrNotVisible)
			c.Assert(listed, qt.IsNil)
		})
	}
	t.Run("the database root", func(t *testing.T) {
		c := qt.New(t)
		listed, err := inspect.ListSelected(c.Context(), tree(nil), "/local", "")
		c.Assert(err, qt.ErrorIs, inspect.ErrNotVisible)
		c.Assert(listed, qt.IsNil)
	})
}
