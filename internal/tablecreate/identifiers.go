package tablecreate

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Preparation, CREATE prediction, and planning interpret the same captured
// identifiers. Keeping this check shared prevents one path from accepting a
// default namespace or folded name that the execution capture cannot retain.
func validateTableIdentifiers(facts *protocol.ComparisonIdentifiers) error {
	if facts.GetDefaultSchema() != "" || facts.GetCatalogCollation() != "" {
		return fmt.Errorf("invalid YDB table identifier context")
	}
	for _, resolved := range facts.GetResolvedNames() {
		if resolved.Name != resolved.Key {
			return fmt.Errorf("YDB table operations require exact identifiers")
		}
	}
	return nil
}
