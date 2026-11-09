package sourceencode

import (
	"strconv"

	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// StreamingQuery writes a streaming query's text, whether it runs and the
// resource pool it runs in where the value states them, and its permission
// to reset the query's state where it is given: one entry of the
// streaming_queries key in YAML, one streamingquery directive in Go.
func StreamingQuery(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	spec, allowStateReset, err := schema.DesiredStreamingQuery(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	// A Go attribute is text, and a YAML flag is a boolean: the YAML decoder
	// refuses the text true.
	flag := func(key string, on bool) *protocol.SourceEntry {
		if format == protocol.SourceDocument_YAML {
			return &protocol.SourceEntry{Key: key, Value: &protocol.SourceValue{Value: &protocol.SourceValue_Boolean{Boolean: on}}}
		}
		return field(key, strconv.FormatBool(on))
	}
	fields = append(fields, field("text", spec.Text))
	if spec.Run != nil {
		fields = append(fields, flag("run", *spec.Run))
	}
	if spec.ResourcePool != "" {
		fields = append(fields, field("resource_pool", spec.ResourcePool))
	}
	if allowStateReset {
		fields = append(fields, flag("allow_state_reset", true))
	}
	return nativePart(format, value, fields, streamingsource.YAMLKey(), streamingsource.GoDirectives()[0], streamingsource.Decode, exact)
}
