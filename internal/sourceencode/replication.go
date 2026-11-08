package sourceencode

import (
	"fmt"
	"path"
	"strconv"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Replication writes one async replication and its ordered items. YAML keeps
// the items under one entry; Go writes the parent followed by item directives.
func Replication(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	spec, err := schema.DesiredReplication(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	fields = append(fields, connectionFields(spec.Connection)...)
	if spec.ConsistencyLevel != "" {
		fields = append(fields, field(replication.AttributeConsistencyLevel, spec.ConsistencyLevel))
	}
	if spec.CommitInterval != "" {
		fields = append(fields, field(replication.AttributeCommitInterval, spec.CommitInterval))
	}
	owner := value.GetValue().GetOwner()
	first := &protocol.SourceFragment{Id: "native." + value.GetId()}
	fragments := []*protocol.SourceFragment{first}
	switch format {
	case protocol.SourceDocument_YAML:
		items := make([]*protocol.SourceValue, 0, len(spec.Items))
		for _, item := range spec.Items {
			items = append(items, mapping(field(replication.AttributeSource, item.Source), field(replication.AttributeTarget, item.Target)))
		}
		fields = append(fields, &protocol.SourceEntry{Key: "items", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Sequence{
			Sequence: &protocol.SourceSequence{Values: items},
		}}})
		key := replicationsource.YAMLKeys()[0]
		first.YamlKey, first.Selector = key, key.Selector
		first.Content = &protocol.SourceFragment_Value{Value: mapping(&protocol.SourceEntry{
			Key: path.Join(owner.Schema, owner.Name), Value: mapping(fields...),
		})}
	case protocol.SourceDocument_GO_ANNOTATIONS:
		directives := replicationsource.GoDirectives()
		first.GoDirective, first.Selector = directives[0], directives[0].Selector
		first.Content = &protocol.SourceFragment_Value{Value: mapping(fields...)}
		for i, item := range spec.Items {
			attributes := []*protocol.SourceEntry{
				field(replication.AttributeReplication, owner.Name),
				field(replication.AttributeSource, item.Source),
				field(replication.AttributeTarget, item.Target),
			}
			if owner.Schema != "" {
				attributes = append(attributes, field(replication.AttributeSchema, owner.Schema))
			}
			fragments = append(fragments, &protocol.SourceFragment{Id: first.Id + ".item." + strconv.Itoa(i),
				GoDirective: directives[1], Selector: directives[1].Selector,
				Content: &protocol.SourceFragment_Value{Value: mapping(attributes...)}})
		}
	case protocol.SourceDocument_HCL:
		return HCLLoss(value.GetValue()), nil
	default:
		return Result{}, fmt.Errorf("unsupported replication source format")
	}
	return nativeParts(value, format, fragments, replicationsource.Decode, exact)
}

// Transfer writes one transfer, including the lambda exactly as declared.
func Transfer(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	spec, err := schema.DesiredTransfer(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	fields = append(fields, connectionFields(spec.Connection)...)
	fields = append(fields, field(replication.AttributeSource, spec.Source), field(replication.AttributeTarget, spec.Target),
		field(replication.AttributeUsing, spec.Lambda))
	if spec.Consumer != "" {
		fields = append(fields, field(replication.AttributeConsumer, spec.Consumer))
	}
	if spec.BatchSizeBytes != 0 {
		fields = append(fields, field(replication.AttributeBatchSizeBytes, strconv.FormatUint(spec.BatchSizeBytes, 10)))
	}
	if spec.FlushInterval != "" {
		fields = append(fields, field(replication.AttributeFlushInterval, spec.FlushInterval))
	}
	return nativePart(format, value, fields, replicationsource.YAMLKeys()[1], replicationsource.GoDirectives()[2],
		replicationsource.Decode, exact)
}

func connectionFields(connection replication.Connection) []*protocol.SourceEntry {
	values := map[string]string{
		replication.AttributeConnectionString:   connection.ConnectionString,
		replication.AttributeTokenSecretName:    connection.TokenSecretName,
		replication.AttributeTokenSecretPath:    connection.TokenSecretPath,
		replication.AttributeUser:               connection.User,
		replication.AttributePasswordSecretName: connection.PasswordSecretName,
		replication.AttributePasswordSecretPath: connection.PasswordSecretPath,
	}
	for key, value := range values {
		if value == "" {
			delete(values, key)
		}
	}
	return sortedFields(values)
}
