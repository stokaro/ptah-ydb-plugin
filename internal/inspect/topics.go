package inspect

import (
	"context"
	"fmt"
	"path"

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

func (s *source) readChangefeeds(ctx context.Context, tablePath string, table *description) (*native.ChangefeedObservations, error) {
	result := &native.ChangefeedObservations{}
	for _, feed := range table.changefeeds {
		topic, err := s.describeTopic(ctx, path.Join(tablePath, feed.Name))
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
