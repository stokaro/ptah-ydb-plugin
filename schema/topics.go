package schema

import (
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

const (
	// TopicProperties identifies a standalone native topic observation.
	TopicProperties = modelNamespace + "topic"
	// ChangefeedProperties identifies the complete changefeed set of one table.
	ChangefeedProperties = modelNamespace + "table-changefeeds"
)

// ChangefeedEffectKind names one changefeed of a table as a planning step's
// effect subject: under the table, with the changefeed's name. The changefeed
// planner names each changefeed it adds, drops or changes so, and a transfer
// that reads one names it the same way, which is how the host orders the two.
const ChangefeedEffectKind = "ydb.changefeed"

func projectTopic(topic *native.DescribeTopicResult) {
	topic.Partitions = nil
	topic.TopicStats = nil
	if self := topic.Self; self != nil {
		self.Name = ""
		self.Type = 0
		self.SizeBytes = 0
		self.CreatedAt = nil
	}
	for _, consumer := range topic.Consumers {
		consumer.ConsumerStats = nil
	}
}

// ValidateChangefeeds validates names before they become native API paths.
func ValidateChangefeeds(feeds []*native.ChangefeedDescription) error {
	seen := make(map[string]bool)
	for _, feed := range feeds {
		if err := Known(feed); err != nil {
			return err
		}
		if !componentName(feed.Name) || seen[feed.Name] {
			return fmt.Errorf("invalid or duplicate YDB changefeed name")
		}
		seen[feed.Name] = true
		if feed.Mode == Ydb_Table.ChangefeedMode_MODE_UNSPECIFIED || feed.Format == Ydb_Table.ChangefeedFormat_FORMAT_UNSPECIFIED || feed.State == native.ChangefeedDescription_STATE_UNSPECIFIED {
			return fmt.Errorf("incomplete YDB changefeed description")
		}
	}
	return nil
}

// CaptureChangefeeds binds each native changefeed to the settings read from its
// child topic. Initial scan presence is retained without its progress counters.
func CaptureChangefeeds(owner *protocol.Reference, observed *native.ChangefeedObservations) (*protocol.Envelope, error) {
	if err := Known(observed); err != nil {
		return nil, err
	}
	projected := proto.CloneOf(observed)
	for _, feed := range projected.Changefeeds {
		if err := projectChangefeed(feed); err != nil {
			return nil, err
		}
	}
	return capture(ChangefeedProperties, owner, projected)
}

// ProjectChangefeed returns one changefeed read as inspection captures it:
// the presence of an initial scan without its progress, and its topic without
// partitions, statistics or transient identity, consumers in name order.
func ProjectChangefeed(observed *native.ObservedChangefeed) (*native.ObservedChangefeed, error) {
	set := &native.ChangefeedObservations{Changefeeds: []*native.ObservedChangefeed{proto.CloneOf(observed)}}
	if err := Known(set); err != nil {
		return nil, err
	}
	if err := projectChangefeed(set.Changefeeds[0]); err != nil {
		return nil, err
	}
	if err := normalizeChangefeeds(set); err != nil {
		return nil, err
	}
	return set.Changefeeds[0], nil
}

func projectChangefeed(feed *native.ObservedChangefeed) error {
	if feed.Description == nil || feed.Topic == nil {
		return fmt.Errorf("incomplete YDB changefeed observation")
	}
	feed.InitialScan = askedForInitialScan(feed.Description)
	feed.Description.InitialScanProgress = nil
	projectTopic(feed.Topic)
	return nil
}

// askedForInitialScan reports whether a changefeed asked for an initial scan.
// Either of two traces proves it. The progress stays on the changefeed once
// the scan ends, measured on 25.1.4.7 and 26.2.1.14, and a changefeed that
// asked for none never carries it. The scan state comes first: right after
// CREATE ASYNC REPLICATION adds its changefeed to the source table, 25.1.4.7
// reports STATE_INITIAL_SCAN with no progress yet. Reading the progress alone
// would report that changefeed as one that asked for no scan.
func askedForInitialScan(description *native.ChangefeedDescription) bool {
	return description.InitialScanProgress != nil ||
		description.State == native.ChangefeedDescription_STATE_INITIAL_SCAN
}

func normalizeChangefeeds(observed *native.ChangefeedObservations) error {
	descriptions := make([]*native.ChangefeedDescription, 0, len(observed.Changefeeds))
	for _, feed := range observed.Changefeeds {
		descriptions = append(descriptions, feed.GetDescription())
	}
	if err := ValidateChangefeeds(descriptions); err != nil {
		return err
	}
	for _, feed := range observed.Changefeeds {
		if feed.Description.InitialScanProgress != nil {
			return fmt.Errorf("changefeed properties contain initial scan progress")
		}
		if feed.Description.State == native.ChangefeedDescription_STATE_INITIAL_SCAN && !feed.InitialScan {
			return fmt.Errorf("initial scan state requires scan provenance")
		}
		if err := normalizeTopic(feed.Topic); err != nil {
			return err
		}
	}
	slices.SortFunc(observed.Changefeeds, func(a, b *native.ObservedChangefeed) int {
		return strings.Compare(a.Description.Name, b.Description.Name)
	})
	return nil
}

func normalizeTopic(topic *native.DescribeTopicResult) error {
	if err := Known(topic); err != nil {
		return err
	}
	if topic.PartitioningSettings == nil {
		return fmt.Errorf("missing YDB topic partition settings")
	}
	if len(topic.Partitions) != 0 || topic.TopicStats != nil {
		return fmt.Errorf("topic properties contain runtime topology or statistics")
	}
	self := topic.GetSelf()
	if self.GetName() != "" || self.GetType() != 0 || self.GetSizeBytes() != 0 || self.GetCreatedAt() != nil {
		return fmt.Errorf("topic properties contain transient identity")
	}
	seen := make(map[string]bool)
	for _, consumer := range topic.Consumers {
		if consumer.Name == "" || seen[consumer.Name] {
			return fmt.Errorf("invalid or duplicate YDB topic consumer")
		}
		seen[consumer.Name] = true
		if consumer.ConsumerStats != nil {
			return fmt.Errorf("topic properties contain consumer statistics")
		}
	}
	slices.SortFunc(topic.Consumers, func(a, b *native.Consumer) int { return strings.Compare(a.Name, b.Name) })
	return nil
}
