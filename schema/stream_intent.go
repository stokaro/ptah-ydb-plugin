package schema

import (
	"fmt"
	"maps"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
)

func topicIntent(observed *native.DescribeTopicResult) *native.TopicIntent {
	result := &native.TopicIntent{
		Access: accessIntent(observed.Self), PartitioningSettings: proto.CloneOf(observed.PartitioningSettings),
		RetentionPeriod: proto.CloneOf(observed.RetentionPeriod), RetentionStorageMb: observed.RetentionStorageMb,
		SupportedCodecs:                          proto.CloneOf(observed.SupportedCodecs),
		PartitionWriteSpeedBytesPerSecond:        observed.PartitionWriteSpeedBytesPerSecond,
		PartitionTotalReadSpeedBytesPerSecond:    observed.PartitionTotalReadSpeedBytesPerSecond,
		PartitionConsumerReadSpeedBytesPerSecond: observed.PartitionConsumerReadSpeedBytesPerSecond,
		PartitionWriteBurstBytes:                 observed.PartitionWriteBurstBytes, Attributes: maps.Clone(observed.Attributes),
		MeteringMode: observed.MeteringMode, MetricsLevel: observed.MetricsLevel,
	}
	for _, consumer := range observed.Consumers {
		result.Consumers = append(result.Consumers, &native.ConsumerIntent{
			Name: consumer.Name, Important: consumer.Important, ReadFrom: proto.CloneOf(consumer.ReadFrom),
			SupportedCodecs: proto.CloneOf(consumer.SupportedCodecs), Attributes: maps.Clone(consumer.Attributes),
			AvailabilityPeriod: proto.CloneOf(consumer.AvailabilityPeriod), StreamingConsumerType: proto.CloneOf(consumer.GetStreamingConsumerType()),
		})
	}
	return result
}

func topicObservation(intent *native.TopicIntent) *native.DescribeTopicResult {
	result := &native.DescribeTopicResult{
		Self: accessObservation(intent.Access), PartitioningSettings: proto.CloneOf(intent.PartitioningSettings),
		RetentionPeriod: proto.CloneOf(intent.RetentionPeriod), RetentionStorageMb: intent.RetentionStorageMb,
		SupportedCodecs:                          proto.CloneOf(intent.SupportedCodecs),
		PartitionWriteSpeedBytesPerSecond:        intent.PartitionWriteSpeedBytesPerSecond,
		PartitionTotalReadSpeedBytesPerSecond:    intent.PartitionTotalReadSpeedBytesPerSecond,
		PartitionConsumerReadSpeedBytesPerSecond: intent.PartitionConsumerReadSpeedBytesPerSecond,
		PartitionWriteBurstBytes:                 intent.PartitionWriteBurstBytes, Attributes: maps.Clone(intent.Attributes),
		MeteringMode: intent.MeteringMode, MetricsLevel: intent.MetricsLevel,
	}
	for _, consumer := range intent.Consumers {
		converted := &native.Consumer{
			Name: consumer.Name, Important: consumer.Important, ReadFrom: proto.CloneOf(consumer.ReadFrom),
			SupportedCodecs: proto.CloneOf(consumer.SupportedCodecs), Attributes: maps.Clone(consumer.Attributes),
			AvailabilityPeriod: proto.CloneOf(consumer.AvailabilityPeriod),
		}
		if consumer.StreamingConsumerType != nil {
			converted.ConsumerType = &native.Consumer_StreamingConsumerType{StreamingConsumerType: proto.CloneOf(consumer.StreamingConsumerType)}
		}
		result.Consumers = append(result.Consumers, converted)
	}
	return result
}

func validateTopicIntent(intent *native.TopicIntent) error {
	if intent == nil {
		return fmt.Errorf("missing YDB topic declaration")
	}
	if intent.RetentionStorageMb < 0 || intent.PartitionWriteSpeedBytesPerSecond < 0 || intent.PartitionTotalReadSpeedBytesPerSecond < 0 || intent.PartitionConsumerReadSpeedBytesPerSecond < 0 || intent.PartitionWriteBurstBytes < 0 {
		return fmt.Errorf("negative YDB topic retention or quota")
	}
	if access := intent.Access; access != nil {
		for _, grants := range [][]*Ydb_Scheme.Permissions{access.DirectGrants, access.RequiredEffectivePermissions} {
			if err := validatePermissionSet(grants); err != nil {
				return err
			}
		}
	}
	return nil
}

func changefeedIntents(observed *native.ChangefeedObservations) *native.ChangefeedSetIntent {
	result := &native.ChangefeedSetIntent{}
	for _, feed := range observed.Changefeeds {
		d := feed.Description
		result.Changefeeds = append(result.Changefeeds, &native.ChangefeedIntent{
			Name: d.Name, Mode: d.Mode, Format: d.Format, Disabled: d.State == native.ChangefeedDescription_STATE_DISABLED,
			InitialScan: feed.InitialScan, VirtualTimestamps: d.VirtualTimestamps, Attributes: maps.Clone(d.Attributes),
			AwsRegion: d.AwsRegion, ResolvedTimestampsInterval: proto.CloneOf(d.ResolvedTimestampsInterval),
			SchemaChanges: d.SchemaChanges, UserSids: d.UserSids, Topic: topicIntent(feed.Topic),
		})
	}
	return result
}

func changefeedObservations(intent *native.ChangefeedSetIntent) (*native.ChangefeedObservations, error) {
	result := &native.ChangefeedObservations{}
	for _, feed := range intent.Changefeeds {
		if err := validateTopicIntent(feed.Topic); err != nil {
			return nil, err
		}
		state := native.ChangefeedDescription_STATE_ENABLED
		if feed.Disabled {
			state = native.ChangefeedDescription_STATE_DISABLED
		}
		result.Changefeeds = append(result.Changefeeds, &native.ObservedChangefeed{
			Description: &native.ChangefeedDescription{
				Name: feed.Name, Mode: feed.Mode, Format: feed.Format, State: state, VirtualTimestamps: feed.VirtualTimestamps,
				Attributes: maps.Clone(feed.Attributes), AwsRegion: feed.AwsRegion,
				ResolvedTimestampsInterval: proto.CloneOf(feed.ResolvedTimestampsInterval), SchemaChanges: feed.SchemaChanges, UserSids: feed.UserSids,
			},
			InitialScan: feed.InitialScan, Topic: topicObservation(feed.Topic),
		})
	}
	if err := normalizeChangefeeds(result); err != nil {
		return nil, err
	}
	return result, nil
}
