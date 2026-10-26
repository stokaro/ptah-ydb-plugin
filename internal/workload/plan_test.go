package workload_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
)

func classifier(pool string, rank int64) *workload.ClassifierSpec {
	return &workload.ClassifierSpec{ResourcePool: pool, Rank: rank}
}

func TestStatements_HappyPath(t *testing.T) {
	tests := []struct {
		name        string
		pools       []workload.PoolChange
		classifiers []workload.ClassifierChange
		want        []string
	}{
		{
			// A removed classifier stops routing to a pool before the pool
			// goes, and a classifier is created after the pool it names.
			name: "each phase in the host's order",
			pools: []workload.PoolChange{
				{Name: "new", After: &workload.PoolSpec{ConcurrentQueryLimit: new(int32(2))}},
				{Name: "old", Before: &workload.PoolSpec{}},
				{Name: "default", Before: &workload.PoolSpec{}, After: &workload.PoolSpec{ResourceWeight: new(30.0)}},
			},
			classifiers: []workload.ClassifierChange{
				{Name: "to_new", After: classifier("new", 30)},
				{Name: "to_old", Before: classifier("old", 10)},
				{Name: "moved", Before: classifier("default", 20), After: classifier("new", 20)},
			},
			want: []string{
				"DROP RESOURCE POOL CLASSIFIER `to_old`;",
				"DROP RESOURCE POOL `old`;",
				"ALTER RESOURCE POOL `default` SET (RESOURCE_WEIGHT = 30);",
				"CREATE RESOURCE POOL `new` WITH (CONCURRENT_QUERY_LIMIT = 2);",
				"ALTER RESOURCE POOL CLASSIFIER `moved` SET (RESOURCE_POOL = 'new', RANK = 20);",
				"CREATE RESOURCE POOL CLASSIFIER `to_new` WITH (RESOURCE_POOL = 'new', RANK = 30);",
			},
		},
		{
			// YDB refuses the second ALTER of a swap at its first statement
			// (`Classifier with rank 20 already exists`), so the classifier
			// moving into a held rank is dropped and created around the move.
			name: "two classifiers trade ranks",
			classifiers: []workload.ClassifierChange{
				{Name: "alpha", Before: classifier("batch", 10), After: classifier("batch", 20)},
				{Name: "beta", Before: classifier("batch", 20), After: classifier("batch", 10)},
			},
			want: []string{
				"DROP RESOURCE POOL CLASSIFIER `alpha`;",
				"DROP RESOURCE POOL CLASSIFIER `beta`;",
				"CREATE RESOURCE POOL CLASSIFIER `alpha` WITH (RESOURCE_POOL = 'batch', RANK = 20);",
				"CREATE RESOURCE POOL CLASSIFIER `beta` WITH (RESOURCE_POOL = 'batch', RANK = 10);",
			},
		},
		{
			// A removal frees its rank before any classifier moves, so the
			// move into it is an ALTER.
			name: "a move into the rank of a removed classifier",
			classifiers: []workload.ClassifierChange{
				{Name: "gone", Before: classifier("batch", 10)},
				{Name: "moved", Before: classifier("batch", 20), After: classifier("batch", 10)},
			},
			want: []string{
				"DROP RESOURCE POOL CLASSIFIER `gone`;",
				"ALTER RESOURCE POOL CLASSIFIER `moved` SET (RESOURCE_POOL = 'batch', RANK = 10);",
			},
		},
		{
			name:  "a pool reset to no limit",
			pools: []workload.PoolChange{{Name: "batch", Before: &workload.PoolSpec{ConcurrentQueryLimit: new(int32(1))}, After: &workload.PoolSpec{}}},
			want:  []string{"ALTER RESOURCE POOL `batch` RESET (CONCURRENT_QUERY_LIMIT);"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			statements, err := workload.Statements(test.pools, test.classifiers)
			c.Assert(err, qt.IsNil)
			c.Assert(statements, qt.DeepEquals, test.want)
		})
	}
}

func TestStatements_FailurePath(t *testing.T) {
	tests := []struct {
		name        string
		pools       []workload.PoolChange
		classifiers []workload.ClassifierChange
		want        workload.ChangeError
	}{
		{
			name:  "creating the pool default",
			pools: []workload.PoolChange{{Name: "default", After: &workload.PoolSpec{}}},
			want:  workload.ChangeError{Name: "default", Reason: "the default pool cannot be created or dropped"},
		},
		{
			name:  "dropping the pool default",
			pools: []workload.PoolChange{{Name: "default", Before: &workload.PoolSpec{}}},
			want:  workload.ChangeError{Name: "default", Reason: "the default pool cannot be created or dropped"},
		},
		{
			name:  "a pool change with no difference",
			pools: []workload.PoolChange{{Name: "p", Before: &workload.PoolSpec{}, After: &workload.PoolSpec{}}},
			want:  workload.ChangeError{Name: "p", Reason: "the change holds no difference"},
		},
		{
			name:  "a pool change with no operand",
			pools: []workload.PoolChange{{Name: "p"}},
			want:  workload.ChangeError{Name: "p", Reason: "the change has no operand"},
		},
		{
			name:        "a classifier change with no difference",
			classifiers: []workload.ClassifierChange{{Name: "c", Before: classifier("p", 1), After: classifier("p", 1)}},
			want:        workload.ChangeError{Classifier: true, Name: "c", Reason: "the change holds no difference"},
		},
		{
			name: "two declared classifiers on one rank",
			classifiers: []workload.ClassifierChange{
				{Name: "a", After: classifier("p", 5)},
				{Name: "b", After: classifier("p", 5)},
			},
			want: workload.ChangeError{Classifier: true, Name: "b", Reason: `classifiers "a" and "b" cannot share rank 5`},
		},
		{
			name: "two read classifiers on one rank",
			classifiers: []workload.ClassifierChange{
				{Name: "a", Before: classifier("p", 5)},
				{Name: "b", Before: classifier("p", 5)},
			},
			want: workload.ChangeError{Classifier: true, Name: "b", Reason: `the database holds classifiers "a" and "b" on rank 5`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			statements, err := workload.Statements(test.pools, test.classifiers)
			var refused *workload.ChangeError
			c.Assert(err, qt.ErrorAs, &refused)
			c.Assert(*refused, qt.Equals, test.want)
			c.Assert(statements, qt.IsNil)
		})
	}
}

func TestChangeError_NamesTheObject(t *testing.T) {
	c := qt.New(t)
	c.Assert((&workload.ChangeError{Name: "p", Reason: "r"}).Error(), qt.Equals, `resource pool "p": r`)
	c.Assert((&workload.ChangeError{Classifier: true, Name: "c", Reason: "r"}).Error(), qt.Equals, `resource pool classifier "c": r`)
}
