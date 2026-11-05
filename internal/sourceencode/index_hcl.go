package sourceencode

import (
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// EncodeIndexHCL writes a global index's own facet as the attributes of the
// frontend's index block, as the host's embedded engine writes them there: a
// switch and the read replicas as strings, a count as a number. An index that
// holds the settings YDB gives a new one is written with no attribute, which
// the block states by writing nothing.
func EncodeIndexHCL(value *protocol.EncodeSourceValue) ([]*protocol.EncodedSourcePart, error) {
	normalized, err := schema.NormalizeDesired(value.GetValue().GetValue())
	if err != nil {
		return nil, err
	}
	var intent native.IndexSettingsIntent
	if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
		return nil, err
	}
	index := indexSettings{owner: proto.CloneOf(normalized.GetOwner()), intent: &intent}
	counts := []string{tablesource.AttributePartitionSizeMB, tablesource.AttributeMinPartitions, tablesource.AttributeMaxPartitions}
	entries := index.attributes()
	for _, entry := range entries {
		if slices.Contains(counts, entry.GetKey()) {
			entry.Value = &protocol.SourceValue{Value: &protocol.SourceValue_Number{Number: entry.GetValue().GetText()}}
		}
	}
	claim := tablesource.HCLIndexBlock()
	return []*protocol.EncodedSourcePart{{Fragment: &protocol.SourceFragment{Id: "index." + value.GetBindingId() + "." + index.owner.GetName(), Selector: claim.GetSelector(),
		HclBlock: claim, BindingId: value.GetBindingId(), Subject: index.subject(), Content: &protocol.SourceFragment_Value{Value: mapping(entries...)}}}}, nil
}
