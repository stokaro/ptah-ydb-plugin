package changefeeds_test

import (
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"google.golang.org/protobuf/types/known/durationpb"
)

// declared writes declarations as the desired changefeed set.
func declared(c *qt.C, specs ...ydbdecl.Changefeed) *native.ChangefeedSetIntent {
	c.Helper()
	set := &native.ChangefeedSetIntent{}
	for _, spec := range specs {
		intent, err := changefeeds.Intent(spec)
		c.Assert(err, qt.IsNil)
		set.Changefeeds = append(set.Changefeeds, intent)
	}
	return set
}

// held is a read changefeed set holding the named changefeeds, each in
// UPDATES mode with its topic's defaults.
func held(names ...string) *native.ChangefeedObservations {
	set := &native.ChangefeedObservations{}
	for _, name := range names {
		feed := observed()
		feed.Description.Name = name
		set.Changefeeds = append(set.Changefeeds, feed)
	}
	return set
}

// step names one planned change as its action and changefeed.
type step struct {
	Action changefeeds.Action
	Name   string
}

func steps(planned []changefeeds.Change) []step {
	var result []step
	for _, item := range planned {
		result = append(result, step{Action: item.Action(), Name: item.Name})
	}
	return result
}

func TestDiff_HappyPath(t *testing.T) {
	updates := func(name string, consumers ...ydbdecl.Consumer) ydbdecl.Changefeed {
		return ydbdecl.Changefeed{Name: name, Mode: "UPDATES", Format: "JSON", Consumers: consumers}
	}
	keysOnly := updates("b")
	keysOnly.Mode = "KEYS_ONLY"
	replicated := held("mirror")
	replicated.Changefeeds[0].Description.Attributes = map[string]string{"__async_replication": `{"path":"/local/repl/t","id":"1","supports_topic_autopartitioning":false}`}
	longer := updates("a")
	longer.RetentionPeriod = "P2D"
	tests := []struct {
		name    string
		desired []ydbdecl.Changefeed
		held    *native.ChangefeedObservations
		want    []step
	}{
		{name: "nothing to change", desired: []ydbdecl.Changefeed{updates("a")}, held: held("a")},
		{name: "a changefeed added", desired: []ydbdecl.Changefeed{updates("a")}, held: held(), want: []step{{Action: changefeeds.Add, Name: "a"}}},
		{name: "a changefeed dropped", held: held("a"), want: []step{{Action: changefeeds.Drop, Name: "a"}}},
		{name: "an option changed drops the changefeed and adds it again", desired: []ydbdecl.Changefeed{keysOnly}, held: held("b"),
			want: []step{{Action: changefeeds.Recreate, Name: "b"}}},
		{name: "a topic changed in place", desired: []ydbdecl.Changefeed{longer}, held: held("a"), want: []step{{Action: changefeeds.Topic, Name: "a"}}},
		{name: "a consumer added changes the topic", desired: []ydbdecl.Changefeed{updates("a", ydbdecl.Consumer{Name: "c"})}, held: held("a"),
			want: []step{{Action: changefeeds.Topic, Name: "a"}}},
		{name: "every kind of change, by name",
			desired: []ydbdecl.Changefeed{updates("z_new"), longer, keysOnly, updates("c_new")}, held: held("a", "b", "y_old", "d_old"),
			want: []step{{Action: changefeeds.Topic, Name: "a"}, {Action: changefeeds.Recreate, Name: "b"}, {Action: changefeeds.Add, Name: "c_new"},
				{Action: changefeeds.Drop, Name: "d_old"}, {Action: changefeeds.Drop, Name: "y_old"}, {Action: changefeeds.Add, Name: "z_new"}}},
		{name: "a replication-managed stream the declaration leaves out is kept", held: replicated},
		{name: "a replication-managed stream the declaration states as it is", desired: []ydbdecl.Changefeed{updates("mirror")}, held: replicated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			planned, undecided := changefeeds.Diff(declared(c, test.desired...), test.held)
			c.Assert(undecided, qt.Equals, "")
			c.Assert(steps(planned), qt.DeepEquals, test.want)
		})
	}
}

// TestDiff_CarriesTheOperands holds that each change carries the operands its
// statements and its precondition read.
func TestDiff_CarriesTheOperands(t *testing.T) {
	c := qt.New(t)
	keysOnly := ydbdecl.Changefeed{Name: "a", Mode: "KEYS_ONLY", Format: "JSON"}
	planned, undecided := changefeeds.Diff(declared(c, keysOnly), held("a"))
	c.Assert(undecided, qt.Equals, "")
	c.Assert(planned, qt.HasLen, 1)
	c.Assert(planned[0].Before.GetDescription().GetName(), qt.Equals, "a")
	c.Assert(planned[0].After.GetMode().String(), qt.Equals, "MODE_KEYS_ONLY")
}

// TestStatements_HappyPath writes every removal first, a recreation's among
// them, so a table at YDB's limit on changefeeds can swap one; then every
// addition with its consumers; then every topic change.
func TestStatements_HappyPath(t *testing.T) {
	c := qt.New(t)
	updates := func(name string, consumers ...ydbdecl.Consumer) ydbdecl.Changefeed {
		return ydbdecl.Changefeed{Name: name, Mode: "UPDATES", Format: "JSON", Consumers: consumers}
	}
	keysOnly := updates("b")
	keysOnly.Mode = "KEYS_ONLY"
	longer := updates("a")
	longer.RetentionPeriod = "P2D"
	planned, undecided := changefeeds.Diff(declared(c, updates("z_new", ydbdecl.Consumer{Name: "c"}), longer, keysOnly), held("a", "b", "d_old"))
	c.Assert(undecided, qt.Equals, "")
	statements, err := changefeeds.Statements("/local/t", planned)
	c.Assert(err, qt.IsNil)
	c.Assert(statements, qt.DeepEquals, []string{
		"ALTER TABLE `/local/t` DROP CHANGEFEED `b`;",
		"ALTER TABLE `/local/t` DROP CHANGEFEED `d_old`;",
		"ALTER TABLE `/local/t` ADD CHANGEFEED `b` WITH (MODE = 'KEYS_ONLY', FORMAT = 'JSON');",
		"ALTER TABLE `/local/t` ADD CHANGEFEED `z_new` WITH (MODE = 'UPDATES', FORMAT = 'JSON');",
		"ALTER TOPIC `/local/t/z_new` ADD CONSUMER `c`;",
		"ALTER TOPIC `/local/t/a` SET (retention_period = Interval('P2D'));",
	})
}

// TestDiff_LeavesUndecidedWhatItCannotRead holds the moves the host's
// comparison leaves undecided rather than plans.
func TestDiff_LeavesUndecidedWhatItCannotRead(t *testing.T) {
	updates := ydbdecl.Changefeed{Name: "mirror", Mode: "UPDATES", Format: "JSON"}
	longer := updates
	longer.RetentionPeriod = "P2D"
	replicated := held("mirror")
	replicated.Changefeeds[0].Description.Attributes = map[string]string{"__async_replication": `{"path":"/local/repl/t","id":"1","supports_topic_autopartitioning":false}`}
	unrepresentable := held("mirror")
	unrepresentable.Changefeeds[0].Description.AwsRegion = "eu-west-1"
	fraction := held("mirror")
	fraction.Changefeeds[0].Topic.RetentionPeriod = durationpb.New(1500 * time.Millisecond)
	tests := []struct {
		name          string
		desired       []ydbdecl.Changefeed
		held          *native.ChangefeedObservations
		wantUndecided string
	}{
		{name: "a replication-managed stream the declaration changes", desired: []ydbdecl.Changefeed{longer}, held: replicated,
			wantUndecided: `changefeed "mirror" is a replication-managed stream, and the retained replication-managed changefeed cannot be created, removed, or changed independently of its controller`},
		{name: "a changefeed the read cannot represent, declared", desired: []ydbdecl.Changefeed{updates}, held: unrepresentable,
			wantUndecided: `changefeed "mirror" has settings this provider cannot represent, so the current changefeed was not established: it names an AWS region, which Ptah does not model`},
		{name: "a changefeed the read cannot represent, left out", held: fraction,
			wantUndecided: `changefeed "mirror" has settings this provider cannot represent, .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			planned, undecided := changefeeds.Diff(declared(c, test.desired...), test.held)
			c.Assert(undecided, qt.Matches, test.wantUndecided)
			c.Assert(planned, qt.HasLen, 0)
		})
	}
}

// TestDiff_DesiredTheDeclarationCannotState holds a desired set holding a
// changefeed no declaration states, as a conversion of an unusual read makes.
func TestDiff_DesiredTheDeclarationCannotState(t *testing.T) {
	c := qt.New(t)
	set := declared(c, ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"})
	set.Changefeeds[0].AwsRegion = "eu-west-1"
	planned, undecided := changefeeds.Diff(set, held())
	c.Assert(undecided, qt.Matches, `the desired source cannot describe the complete changefeed: .*AWS region.*`)
	c.Assert(planned, qt.HasLen, 0)
}
