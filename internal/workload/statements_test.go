package workload_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
)

func TestCreatePoolStatement(t *testing.T) {
	tests := []struct {
		name string
		spec workload.PoolSpec
		want string
	}{
		{
			// A WITH clause needs an option, and YQL writes no negative
			// number in one, so "-1" is YDB's own spelling of no limit.
			name: "no setting",
			spec: workload.PoolSpec{},
			want: "CREATE RESOURCE POOL `batch` WITH (CONCURRENT_QUERY_LIMIT = \"-1\");",
		},
		{
			// A fraction is a string literal, which YDB reads as the number;
			// a whole number of a percentage is written bare.
			name: "every setting, in the order YDB lists them",
			spec: workload.PoolSpec{
				ConcurrentQueryLimit: new(int32(10)), QueueSize: new(int32(0)),
				DatabaseLoadCPUThreshold: new(80.5), QueryMemoryLimitPercentPerNode: new(25.0),
				QueryCPULimitPercentPerNode: new(0.001), TotalCPULimitPercentPerNode: new(100.0),
				ResourceWeight: new(2.25),
			},
			want: "CREATE RESOURCE POOL `batch` WITH (CONCURRENT_QUERY_LIMIT = 10, QUEUE_SIZE = 0, " +
				"DATABASE_LOAD_CPU_THRESHOLD = '80.5', QUERY_MEMORY_LIMIT_PERCENT_PER_NODE = 25, " +
				"QUERY_CPU_LIMIT_PERCENT_PER_NODE = '0.001', TOTAL_CPU_LIMIT_PERCENT_PER_NODE = 100, " +
				"RESOURCE_WEIGHT = '2.25');",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(workload.CreatePoolStatement("batch", test.spec), qt.Equals, test.want)
		})
	}
}

func TestAlterPoolStatement(t *testing.T) {
	tests := []struct {
		name             string
		desired, current workload.PoolSpec
		want             string
	}{
		{
			name:    "the same settings need no statement",
			desired: workload.PoolSpec{ConcurrentQueryLimit: new(int32(5))},
			current: workload.PoolSpec{ConcurrentQueryLimit: new(int32(5))},
			want:    "",
		},
		{
			// A setting another statement left alone is not named: YDB
			// changes only what a SET names.
			name:    "one setting changes",
			desired: workload.PoolSpec{ConcurrentQueryLimit: new(int32(20)), QueueSize: new(int32(7))},
			current: workload.PoolSpec{ConcurrentQueryLimit: new(int32(10)), QueueSize: new(int32(7))},
			want:    "ALTER RESOURCE POOL `batch` SET (CONCURRENT_QUERY_LIMIT = 20);",
		},
		{
			// YDB checks the pool a statement leaves behind as a whole, so the
			// limit and the queue that waits for it go in one statement.
			name:    "a limit and its queue go together",
			desired: workload.PoolSpec{},
			current: workload.PoolSpec{ConcurrentQueryLimit: new(int32(10)), QueueSize: new(int32(7))},
			want:    "ALTER RESOURCE POOL `batch` RESET (CONCURRENT_QUERY_LIMIT, QUEUE_SIZE);",
		},
		{
			name:    "a set and a reset in one statement",
			desired: workload.PoolSpec{ResourceWeight: new(3.0)},
			current: workload.PoolSpec{QueryMemoryLimitPercentPerNode: new(12.5)},
			want:    "ALTER RESOURCE POOL `batch` SET (RESOURCE_WEIGHT = 3), RESET (QUERY_MEMORY_LIMIT_PERCENT_PER_NODE);",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(workload.AlterPoolStatement("batch", test.desired, test.current), qt.Equals, test.want)
		})
	}
}

func TestClassifierStatements(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		want      string
	}{
		{
			name: "create with a member",
			statement: workload.CreateClassifierStatement("etl_users",
				workload.ClassifierSpec{ResourcePool: "batch", MemberName: "etl", Rank: 100}),
			want: "CREATE RESOURCE POOL CLASSIFIER `etl_users` WITH (RESOURCE_POOL = 'batch', RANK = 100, " +
				"MEMBER_NAME = 'etl');",
		},
		{
			name: "create for every query, escaping a quote",
			statement: workload.CreateClassifierStatement("all",
				workload.ClassifierSpec{ResourcePool: `a'b`, Rank: 0}),
			want: "CREATE RESOURCE POOL CLASSIFIER `all` WITH (RESOURCE_POOL = 'a\\'b', RANK = 0);",
		},
		{
			// The pool and the rank are named whether or not they change: YDB
			// takes a rank the classifier already holds.
			name: "alter names the whole classifier",
			statement: workload.AlterClassifierStatement("c",
				workload.ClassifierSpec{ResourcePool: "b", MemberName: "u2", Rank: 5},
				workload.ClassifierSpec{ResourcePool: "a", MemberName: "u", Rank: 5}),
			want: "ALTER RESOURCE POOL CLASSIFIER `c` SET (RESOURCE_POOL = 'b', RANK = 5, MEMBER_NAME = 'u2');",
		},
		{
			name: "alter resets a member the classifier stops naming",
			statement: workload.AlterClassifierStatement("c",
				workload.ClassifierSpec{ResourcePool: "a", Rank: 6},
				workload.ClassifierSpec{ResourcePool: "a", MemberName: "u", Rank: 5}),
			want: "ALTER RESOURCE POOL CLASSIFIER `c` SET (RESOURCE_POOL = 'a', RANK = 6), RESET (MEMBER_NAME);",
		},
		{
			name: "alter resets no member the classifier never named",
			statement: workload.AlterClassifierStatement("c",
				workload.ClassifierSpec{ResourcePool: "b", Rank: 5},
				workload.ClassifierSpec{ResourcePool: "a", Rank: 5}),
			want: "ALTER RESOURCE POOL CLASSIFIER `c` SET (RESOURCE_POOL = 'b', RANK = 5);",
		},
		{
			name: "alter of the same classifier is nothing",
			statement: workload.AlterClassifierStatement("c",
				workload.ClassifierSpec{ResourcePool: "a", Rank: 5},
				workload.ClassifierSpec{ResourcePool: "a", Rank: 5}),
			want: "",
		},
		{
			name:      "drop a pool",
			statement: workload.DropPoolStatement("batch"),
			want:      "DROP RESOURCE POOL `batch`;",
		},
		{
			name:      "drop a classifier",
			statement: workload.DropClassifierStatement("c"),
			want:      "DROP RESOURCE POOL CLASSIFIER `c`;",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(test.statement, qt.Equals, test.want)
		})
	}
}

func TestPoolsEqual(t *testing.T) {
	tests := []struct {
		name           string
		left, right    workload.PoolSpec
		wantEquivalent bool
	}{
		{name: "both unset", wantEquivalent: true},
		{
			name: "values, not pointers, are compared",
			left: workload.PoolSpec{ConcurrentQueryLimit: new(int32(3)), ResourceWeight: new(2.5)},
			right: workload.PoolSpec{
				ConcurrentQueryLimit: new(int32(3)), ResourceWeight: new(2.5),
			},
			wantEquivalent: true,
		},
		{
			name:  "zero is not unset",
			left:  workload.PoolSpec{ConcurrentQueryLimit: new(int32(0))},
			right: workload.PoolSpec{},
		},
		{
			name:  "a fraction that differs",
			left:  workload.PoolSpec{DatabaseLoadCPUThreshold: new(80.5)},
			right: workload.PoolSpec{DatabaseLoadCPUThreshold: new(80.0)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(workload.PoolsEqual(test.left, test.right), qt.Equals, test.wantEquivalent)
			c.Assert(workload.PoolsEqual(test.right, test.left), qt.Equals, test.wantEquivalent)
		})
	}
}
