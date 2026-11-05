package sourceencode

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Secret writes the approved input reference without resolving its value.
func Secret(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	intent, err := schema.DesiredSecret(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	fields = append(fields, field("value_env", intent.GetValueEnv()))
	return nativePart(format, value, fields, secretsource.YAMLKey(), secretsource.GoDirective(), secretsource.Decode)
}
