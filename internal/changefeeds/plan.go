package changefeeds

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// Action is what a change does to one changefeed.
type Action int

// The actions of a change.
const (
	// Add adds a changefeed and the consumers of its topic.
	Add Action = iota + 1
	// Drop removes a changefeed with its topic.
	Drop
	// Recreate drops a changefeed and adds it again, because an option of it
	// changed, which YDB changes in no other way.
	Recreate
	// Topic changes the retention or the consumers of a changefeed's topic.
	Topic
)

// Change is how one changefeed of a table moves: from Before, the changefeed
// as the table holds it, to After, the declared one. A nil Before is an
// addition, a nil After a removal.
type Change struct {
	// Name is the changefeed's name.
	Name string
	// Before is the changefeed as the table holds it, nil where it holds none.
	Before *native.ObservedChangefeed
	// After is the declared changefeed, nil where the declaration states none.
	After *native.ChangefeedIntent
}

// Action reads what the change does. A pair whose sides cannot be read is a
// topic change, which the operation's normalization refuses.
func (c Change) Action() Action {
	switch {
	case c.Before == nil:
		return Add
	case c.After == nil:
		return Drop
	}
	current, err := FromObserved(c.Before)
	if err != nil {
		return Topic
	}
	desired, err := FromIntent(c.After)
	if err != nil {
		return Topic
	}
	if Recreated(desired, current) {
		return Recreate
	}
	return Topic
}

// Diff returns the changes that give a table the declared changefeeds, in name
// order. An empty result means the table holds what the declaration states.
//
// A non-empty reason means the move cannot be decided, as the host's
// comparison leaves it undecided: a changefeed the read cannot represent, one
// the declaration cannot state, or a stream an asynchronous replication
// controller owns that the declaration changes. A replicated stream the
// declaration leaves out is kept, because its controller, not the schema,
// owns it.
func Diff(desired *native.ChangefeedSetIntent, held *native.ChangefeedObservations) ([]Change, string) {
	wanted := map[string]*native.ChangefeedIntent{}
	wantedFeeds := map[string]Feed{}
	for _, intent := range desired.GetChangefeeds() {
		feed, err := FromIntent(intent)
		if err != nil {
			return nil, fmt.Sprintf("the desired source cannot describe the complete changefeed: %v", err)
		}
		wanted[intent.GetName()], wantedFeeds[intent.GetName()] = intent, feed
	}
	having := map[string]*native.ObservedChangefeed{}
	havingFeeds := map[string]Feed{}
	for _, observed := range held.GetChangefeeds() {
		name := observed.GetDescription().GetName()
		feed, err := FromObserved(observed)
		if err != nil {
			return nil, fmt.Sprintf("changefeed %q has settings this provider cannot represent, so the current changefeed was not established: %v", name, err)
		}
		having[name], havingFeeds[name] = observed, feed
	}
	names := slices.Collect(maps.Keys(wanted))
	for name := range having {
		if _, both := wanted[name]; !both {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var changes []Change
	for _, name := range names {
		want, declared := wantedFeeds[name]
		have, present := havingFeeds[name]
		switch {
		case present && have.Replicated:
			if declared && !Equal(want, have) {
				return nil, fmt.Sprintf("changefeed %q is a replication-managed stream, and the retained replication-managed "+
					"changefeed cannot be created, removed, or changed independently of its controller", name)
			}
		case declared && want.Replicated:
			return nil, fmt.Sprintf("changefeed %q is declared as a replication-managed stream, and the retained "+
				"replication-managed changefeed cannot be created, removed, or changed independently of its controller", name)
		case !present:
			changes = append(changes, Change{Name: name, After: wanted[name]})
		case !declared:
			changes = append(changes, Change{Name: name, Before: having[name]})
		case !Equal(want, have):
			changes = append(changes, Change{Name: name, Before: having[name], After: wanted[name]})
		}
	}
	return changes, ""
}

// Statements writes what moves the changefeeds of the table at path through
// changes: every removal first, a recreation's among them, since YDB keeps a
// limit on the changefeeds of one table and a swap at the limit must free a
// place; then every addition, a recreation's among them, each with the
// consumers of its topic; then every topic change. Each group keeps the order
// of changes.
func Statements(path string, changes []Change) ([]string, error) {
	var drops, adds, topics []string
	for _, change := range changes {
		action := change.Action()
		if action == Drop || action == Recreate {
			drops = append(drops, DropStatement(path, change.Name))
		}
		if action == Add || action == Recreate {
			feed, err := FromIntent(change.After)
			if err != nil {
				return nil, err
			}
			adds = append(adds, AddStatements(path, feed.Changefeed)...)
		}
		if action == Topic {
			desired, err := FromIntent(change.After)
			if err != nil {
				return nil, err
			}
			previous, err := FromObserved(change.Before)
			if err != nil {
				return nil, err
			}
			written, _ := TopicStatements(path, desired.Changefeed, previous.Changefeed)
			topics = append(topics, written...)
		}
	}
	return slices.Concat(drops, adds, topics), nil
}

// Declared reads the declared changefeeds of a table, in name order.
func Declared(desired *native.ChangefeedSetIntent) ([]Feed, error) {
	var feeds []Feed
	for _, intent := range desired.GetChangefeeds() {
		feed, err := FromIntent(intent)
		if err != nil {
			return nil, err
		}
		feeds = append(feeds, feed)
	}
	slices.SortFunc(feeds, func(a, b Feed) int { return strings.Compare(a.Name, b.Name) })
	return feeds, nil
}
