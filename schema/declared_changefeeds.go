package schema

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// DeclaredChangefeedNames are the names of the changefeeds a declared
// changefeed set of one table holds, in the order the value lists them. The
// value is the desired ChangefeedProperties model, as a host hands it to a
// comparison that reads the declared changefeeds of the tables
// (models.declared-facets).
func DeclaredChangefeedNames(value *protocol.Envelope) ([]string, error) {
	if value.GetKind() != ChangefeedProperties {
		return nil, fmt.Errorf("a declared changefeed set has kind %s, not %s", value.GetKind(), ChangefeedProperties)
	}
	intent := &native.ChangefeedSetIntent{}
	if err := protojson.Unmarshal(value.GetPayload(), intent); err != nil {
		return nil, fmt.Errorf("decode the declared changefeeds of table %s: %w", value.GetOwner().GetName(), err)
	}
	names := make([]string, 0, len(intent.GetChangefeeds()))
	for _, feed := range intent.GetChangefeeds() {
		names = append(names, feed.GetName())
	}
	return names, nil
}
