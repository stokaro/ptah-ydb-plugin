package inspect

import (
	"context"
	"fmt"
	"path"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

func topicIdentity(topic *native.DescribeTopicResult, name string) error {
	self := topic.GetSelf()
	if self.GetName() != name || (self.GetType() != Ydb_Scheme.Entry_TOPIC && self.GetType() != Ydb_Scheme.Entry_PERS_QUEUE_GROUP) {
		return fmt.Errorf("YDB listing and topic description disagree")
	}
	return nil
}

// readChangefeeds reads the topic of each changefeed the description of the
// table at tablePath names. It waits out a topic the table names before the
// topic service describes it, such as the one of the changefeed an async
// replication adds to its source table; see [nativeapi.DescribeListed].
func (s *source) readChangefeeds(ctx context.Context, tablePath string, table *description) (*native.ChangefeedObservations, error) {
	result := &native.ChangefeedObservations{}
	for _, feed := range table.changefeeds {
		topicPath := path.Join(tablePath, feed.Name)
		topic, err := nativeapi.DescribeListed(ctx, "YDB changefeed topic "+topicPath,
			func(ctx context.Context) (*native.DescribeTopicResult, error) { return s.describeTopic(ctx, topicPath) },
			func(ctx context.Context) (bool, error) { return s.namesChangefeed(ctx, tablePath, feed.Name) },
		)
		if err != nil {
			return nil, err
		}
		if err := topicIdentity(topic, feed.Name); err != nil {
			return nil, err
		}
		result.Changefeeds = append(result.Changefeeds, &native.ObservedChangefeed{Description: feed, Topic: topic})
	}
	return result, nil
}

// namesChangefeed reports whether the table at tablePath still names the
// changefeed name.
func (s *source) namesChangefeed(ctx context.Context, tablePath, name string) (bool, error) {
	described, err := s.owner.Describe(ctx, tablePath)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(described.GetChangefeeds(), func(feed *native.ChangefeedDescription) bool {
		return feed.GetName() == name
	}), nil
}
