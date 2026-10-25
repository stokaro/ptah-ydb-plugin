package yqlschema

import (
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

func (p *parser) changefeed() *addChangefeed {
	table := p.path()
	p.wantWord("ADD")
	p.wantWord("CHANGEFEED")
	name := decodedName(p.identifier())
	values := p.declarationSettings(changefeedSetting)
	if value, ok := values[ydbdecl.AttributeTopicAutoPartitioning]; ok {
		switch strings.ToUpper(value) {
		case "ENABLED":
			values[ydbdecl.AttributeTopicAutoPartitioning] = "true"
		case "DISABLED":
			values[ydbdecl.AttributeTopicAutoPartitioning] = "false"
		default:
			p.failf("topic_auto_partitioning requires ENABLED or DISABLED")
		}
	}
	values[ydbdecl.ChangefeedName] = name
	spec, err := ydbdecl.ParseChangefeed(values)
	if err != nil {
		p.failf("%v", err)
	}
	if spec.Name != name {
		p.failf("changefeed name %q cannot be represented exactly", name)
	}
	return &addChangefeed{table: table, feed: spec}
}

func changefeedSetting(key string) string {
	switch key {
	case ydbdecl.AttributeMode, ydbdecl.AttributeFormat,
		ydbdecl.AttributeVirtualTimestamps, ydbdecl.AttributeInitialScan,
		ydbdecl.AttributeUserSIDs, ydbdecl.AttributeSchemaChanges,
		ydbdecl.AttributeTopicMinActivePartitions, ydbdecl.AttributeTopicAutoPartitioning:
		return ""
	case ydbdecl.AttributeResolvedTimestamps, ydbdecl.AttributeFeedRetentionPeriod:
		return "Interval"
	default:
		return "unsupported"
	}
}
