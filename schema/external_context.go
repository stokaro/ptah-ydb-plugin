package schema

import (
	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// A conversion changes the interpretation root while retaining the receipt.
// Rebase absolute external references so conversion does not change their target.
func convertExternalContext(source *protocol.Envelope, converted proto.Message, to protocol.ValueInput_Representation) error {
	from := InterpretationRoot(source)
	root := source.GetContext().GetDatabase()
	if to == protocol.ValueInput_DESIRED {
		root = source.GetContext().GetSourceDatabase()
	}
	var properties map[string]string
	var sourcePath **string
	switch value := converted.(type) {
	case *native.ExternalSourceIntent:
		properties = value.Properties
	case *Ydb_Table.DescribeExternalDataSourceResult:
		properties = value.Properties
	case *native.ExternalTableIntent:
		sourcePath = &value.DataSourcePath
	case *native.DescribeExternalTableResult:
		sourcePath = &value.DataSourcePath
	default:
		return nil
	}
	for name, value := range properties {
		if externalpath.IsSecretOption(name) {
			rebased, err := externalpath.Rebase(value, from, root)
			if err != nil {
				return err
			}
			properties[name] = rebased
		}
	}
	if sourcePath != nil && *sourcePath != nil {
		rebased, err := externalpath.Rebase(**sourcePath, from, root)
		if err != nil {
			return err
		}
		*sourcePath = new(rebased)
	}
	return nil
}
