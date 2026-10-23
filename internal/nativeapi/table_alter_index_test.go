package nativeapi_test

import (
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	labelCol = `{"name":"label","type":` + utf8Optional + `}`
	// byLabelIndex is a ready sync index on label.
	byLabelIndex    = `{"name":"by_label","indexColumns":["label"],"status":"STATUS_READY","globalIndex":{}}`
	byLabelBuilding = `{"name":"by_label","indexColumns":["label"],"status":"STATUS_BUILDING","globalIndex":{}}`
	byExtraIndex    = `{"name":"by_extra","indexColumns":["extra"],"dataColumns":["label"],"status":"STATUS_READY","globalAsyncIndex":{}}`
	otherByLabel    = `{"name":"by_label","indexColumns":["extra"],"status":"STATUS_READY","globalIndex":{}}`
	changedExtra    = `{"name":"by_extra","indexColumns":["label"],"status":"STATUS_READY","globalAsyncIndex":{}}`
	asyncByLabel    = `{"name":"by_label","indexColumns":["label"],"status":"STATUS_READY","globalAsyncIndex":{}}`

	addByLabel = `{"request":{"addIndexes":[{"name":"by_label","indexColumns":["label"],"globalIndex":{}}]}}`
	addByExtra = `{"request":{"addIndexes":[{"name":"by_extra","indexColumns":["extra"],"dataColumns":["label"],"globalAsyncIndex":{}}]}}`
	addUnique  = `{"request":{"addIndexes":[{"name":"by_label","indexColumns":["label"],"globalUniqueIndex":{}}]}}`
	uniqueRead = `{"name":"by_label","indexColumns":["label"],"status":"STATUS_READY","globalUniqueIndex":{}}`
)

func indexedTable(indexes ...string) string {
	text := ""
	for i, index := range indexes {
		if i > 0 {
			text += ","
		}
		text += index
	}
	if text == "" {
		return withColumns("", labelCol, extraCol)
	}
	return withColumns(`,"indexes":[`+text+`]`, labelCol, extraCol)
}

func dropIndex(name, held string) string {
	return `{"request":{"dropIndexes":["` + name + `"]},"held_indexes":[` + held + `]}`
}

func TestNativeTableAlteration_IndexHappyPath(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after string
		later         []string
		payload       string
		wantAdded     string
		wantDropped   []string
		events        []string
	}{
		{"adds a sync index", indexedTable(), indexedTable(byLabelIndex), nil, addByLabel, "by_label", nil,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}},
		{"adds an async index that covers a column", indexedTable(), indexedTable(byExtraIndex), nil, addByExtra, "by_extra", nil,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}},
		{"adds a unique index", indexedTable(), indexedTable(uniqueRead), nil, addUnique, "by_label", nil,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}},
		{"adds an index beside another", indexedTable(byExtraIndex), indexedTable(byExtraIndex, byLabelIndex), nil, addByLabel, "by_label", nil,
			[]string{"open", "describe", "guard", "alter", "describe", "close"}},
		{"waits while the index is being built", indexedTable(), indexedTable(byLabelBuilding), []string{indexedTable(byLabelBuilding), indexedTable(byLabelIndex)}, addByLabel, "by_label", nil,
			[]string{"open", "describe", "guard", "alter", "describe", "describe", "describe", "close"}},
		{"drops an index", indexedTable(byLabelIndex), indexedTable(), nil, dropIndex("by_label", byLabelIndex), "", []string{"by_label"},
			[]string{"open", "describe", "guard", "alter", "describe", "close"}},
		{"drops an index and keeps another", indexedTable(byLabelIndex, byExtraIndex), indexedTable(byExtraIndex), nil, dropIndex("by_label", byLabelIndex), "", []string{"by_label"},
			[]string{"open", "describe", "guard", "alter", "describe", "close"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := &alterFixture{before: test.before, after: test.after, later: test.later}
			dispatched, err := applyAlteration(c, fixture, "", test.payload, false)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(fixture.events, qt.DeepEquals, test.events)
			c.Assert(fixture.sent.GetDropIndexes(), qt.DeepEquals, test.wantDropped)
			sent, err := protojson.Marshal(fixture.sent)
			c.Assert(err, qt.IsNil)
			c.Assert(string(sent), qt.Contains, test.wantAdded)
		})
	}
}

func TestNativeTableAlteration_IndexFailurePath(t *testing.T) {
	refused := []string{"open", "describe", "close"}
	unconfirmed := []string{"open", "describe", "guard", "alter", "describe", "close"}
	for _, test := range []struct {
		name       string
		fixture    alterFixture
		payload    string
		dispatched bool
		events     []string
		message    string
	}{
		{"the index exists since planning", alterFixture{before: indexedTable(byLabelIndex)}, addByLabel, false, refused, "YDB index to add already exists"},
		{"the index reads a column the table does not hold", alterFixture{before: withColumns("", extraCol)}, addByLabel, false, refused,
			`YDB index to add reads the absent column "label"`},
		{"the index to drop is gone", alterFixture{before: indexedTable()}, dropIndex("by_label", byLabelIndex), false, refused, "YDB index to drop is absent"},
		{"the index to drop changed since planning", alterFixture{before: indexedTable(otherByLabel)}, dropIndex("by_label", byLabelIndex), false, refused, "YDB index differs from the plan"},
		{"the index to drop changed its kind since planning", alterFixture{before: indexedTable(asyncByLabel)}, dropIndex("by_label", byLabelIndex), false, refused, "YDB index differs from the plan"},
		{"the index is not created", alterFixture{before: indexedTable(), after: indexedTable()}, addByLabel, true, unconfirmed, "YDB added index differs from the request"},
		{"another kind is read back", alterFixture{before: indexedTable(), after: indexedTable(asyncByLabel)}, addByLabel, true, unconfirmed, "YDB added index differs from the request"},
		{"other columns are read back", alterFixture{before: indexedTable(), after: indexedTable(otherByLabel)}, addByLabel, true, unconfirmed, "YDB added index differs from the request"},
		{"the index is still being built when the wait ends", alterFixture{before: indexedTable(), after: indexedTable(byLabelBuilding), indexTimeout: 20 * time.Millisecond},
			addByLabel, true, nil, "YDB added index differs from the request"},
		{"the index is still present", alterFixture{before: indexedTable(byLabelIndex), after: indexedTable(byLabelIndex)}, dropIndex("by_label", byLabelIndex), true, unconfirmed,
			"YDB dropped index is still present"},
		{"another index disappeared", alterFixture{before: indexedTable(byExtraIndex), after: indexedTable(byLabelIndex)}, addByLabel, true, unconfirmed,
			`YDB altered table changed the index "by_extra"`},
		{"another index changed", alterFixture{before: indexedTable(byExtraIndex), after: indexedTable(byLabelIndex, changedExtra)}, addByLabel, true, unconfirmed,
			`YDB altered table changed the index "by_extra"`},
		{"an unrelated index appeared", alterFixture{before: indexedTable(), after: indexedTable(byLabelIndex, byExtraIndex)}, addByLabel, true, unconfirmed,
			"YDB altered table holds unexpected indexes"},
		{"lost reply", alterFixture{before: indexedTable(), alterFailure: true}, addByLabel, true, []string{"open", "describe", "guard", "alter", "close"}, "lost native reply"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := test.fixture
			dispatched, err := applyAlteration(c, &fixture, "", test.payload, false)
			c.Assert(err, qt.ErrorMatches, test.message)
			c.Assert(dispatched, qt.Equals, test.dispatched)
			if test.events != nil {
				c.Assert(fixture.events, qt.DeepEquals, test.events)
			}
		})
	}
}
