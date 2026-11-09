package sourceencode

import (
	"fmt"
	"strconv"

	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ResourcePool writes the limits a pool states, an explicit zero included,
// and leaves out the ones it leaves unset.
func ResourcePool(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	spec, err := schema.WorkloadPool(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	return workloadPart(format, value, workload.PoolValues(spec), 0)
}

// ResourcePoolClassifier writes a classifier's pool, member and rank.
func ResourcePoolClassifier(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	spec, err := schema.WorkloadClassifier(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	attributes := map[string]string{workload.AttributeResourcePool: spec.ResourcePool, workload.AttributeRank: strconv.FormatInt(spec.Rank, 10)}
	if spec.MemberName != "" {
		attributes[workload.AttributeMemberName] = spec.MemberName
	}
	return workloadPart(format, value, attributes, 1)
}

func workloadPart(format protocol.SourceDocument_Format, value *protocol.NormalizedValue, attributes map[string]string, claim int) (Result, error) {
	identity, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	fragment := &protocol.SourceFragment{Id: "native." + value.GetId()}
	fields := sortedFields(attributes)
	switch format {
	case protocol.SourceDocument_YAML:
		fragment.YamlKey = workloadsource.YAMLKeys()[claim]
		fragment.Selector = fragment.YamlKey.Selector
		fragment.Content = &protocol.SourceFragment_Value{Value: mapping(&protocol.SourceEntry{Key: value.GetValue().GetOwner().GetName(), Value: mapping(fields...)})}
	case protocol.SourceDocument_GO_ANNOTATIONS:
		fragment.GoDirective = workloadsource.GoDirectives()[claim]
		fragment.Selector = fragment.GoDirective.Selector
		fragment.Content = &protocol.SourceFragment_Value{Value: mapping(append(identity, fields...)...)}
	case protocol.SourceDocument_HCL:
		return HCLLoss(value.GetValue()), nil
	default:
		return Result{}, fmt.Errorf("unsupported workload source format")
	}
	return nativeParts(value, format, []*protocol.SourceFragment{fragment}, workloadsource.Decode, exact)
}
