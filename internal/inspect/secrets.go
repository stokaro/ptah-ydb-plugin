package inspect

import (
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// UnavailableSecrets retains listed identities as unknown observations when
// scheme secrets are disabled. A directory listing still proves other names
// absent; a listed secret must not become a managed presence or a planned drop.
// A name the provider's operational storage occupies stays unrepresentable:
// it holds no secret whatever the capability says, and the contract refuses a
// reserved name that any other coverage claims.
func UnavailableSecrets(snapshot *protocol.Schema) {
	const reason = "The target's secrets capability is unavailable"
	snapshot.NativeObjects = slices.DeleteFunc(snapshot.NativeObjects, func(value *protocol.Envelope) bool {
		return value.GetKind() == schema.SecretProperties
	})
	for _, subject := range snapshot.GetCoverage().GetSubjects() {
		if subject.Kind == schema.SecretProperties && subject.GetKnowledge().GetState() != protocol.ModelKnowledge_UNREPRESENTABLE {
			subject.Knowledge = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: reason}
		}
	}
}
