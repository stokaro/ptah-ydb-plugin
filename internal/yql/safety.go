package yql

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"slices"
)

func prefix(words []string, wanted ...string) bool {
	return len(words) >= len(wanted) && slices.Equal(words[:len(wanted)], wanted)
}
func sequence(words []string, wanted ...string) bool {
	for i := range words {
		if prefix(words[i:], wanted...) {
			return true
		}
	}
	return false
}

// safetyOf carries native state-loss semantics across the process boundary.
// Unmodeled syntax is UNKNOWN, so the host cannot label it safe by default.
func safetyOf(words []string) (protocol.SQLStatement_Safety, string) {
	switch {
	case prefix(words, "DROP", "ASYNC", "REPLICATION") && !slices.Contains(words, "CASCADE"):
		return protocol.SQLStatement_BEHAVIORAL, "Dropping replication without CASCADE leaves replica tables read-only unless failover already completed"
	case prefix(words, "DROP", "ASYNC", "REPLICATION"):
		return protocol.SQLStatement_DESTRUCTIVE, "Dropping replication with CASCADE removes replica tables"
	case prefix(words, "DROP", "STREAMING", "QUERY"), prefix(words, "CREATE", "OR", "REPLACE", "STREAMING", "QUERY") && !prefix(words[5:], "IF", "NOT", "EXISTS"), prefix(words, "ALTER", "STREAMING", "QUERY") && sequence(words, "AS", "DO", "BEGIN"):
		return protocol.SQLStatement_DESTRUCTIVE, "The operation removes or resets streaming-query checkpoint state"
	case prefix(words, "ALTER", "STREAMING", "QUERY"):
		return protocol.SQLStatement_BEHAVIORAL, "The operation changes continuous query execution"
	case prefix(words, "DROP", "TOPIC"):
		return protocol.SQLStatement_DESTRUCTIVE, "Dropping a topic removes its messages and every consumer position"
	case prefix(words, "ALTER", "TOPIC") && sequence(words, "DROP", "CONSUMER"):
		return protocol.SQLStatement_DESTRUCTIVE, "Dropping a consumer removes its position in the topic"
	case prefix(words, "DROP", "TRANSFER"):
		return protocol.SQLStatement_DESTRUCTIVE, "Dropping a transfer removes its topic consumer and position"
	case prefix(words, "DROP", "COORDINATION", "NODE"):
		return protocol.SQLStatement_DESTRUCTIVE, "Dropping a coordination node removes its sessions, semaphores, and rate limiter resources"
	case prefix(words, "DROP", "SECRET"):
		return protocol.SQLStatement_DESTRUCTIVE, "Dropping a secret removes a value that cannot be read back"
	case prefix(words, "DROP", "EXTERNAL", "DATA", "SOURCE"), prefix(words, "DROP", "EXTERNAL", "TABLE"), prefix(words, "CREATE", "OR", "REPLACE", "EXTERNAL"):
		return protocol.SQLStatement_BEHAVIORAL, "The operation changes how YDB reads external data; external data remains outside this operation"
	case prefix(words, "DROP", "TABLE"), prefix(words, "TRUNCATE", "TABLE"), prefix(words, "ALTER", "TABLE") && sequence(words, "DROP", "COLUMN"):
		return protocol.SQLStatement_DESTRUCTIVE, "The operation removes stored table data"
	case prefix(words, "ALTER", "TABLE") && slices.Contains(words, "TTL"):
		return protocol.SQLStatement_UNKNOWN, "The operation changes automatic data expiration; its effect requires review"
	case slices.Contains(words, ";"):
		return protocol.SQLStatement_UNKNOWN, "The statement contains a compound body whose complete effect requires review"
	case prefix(words, "CREATE", "TABLE"):
		return protocol.SQLStatement_COMMON_DDL, ""
	case prefix(words, "ALTER", "TABLE") && len(words) > 5 && words[3] == "ADD" && words[4] == "COLUMN" && !slices.Contains(words, ","):
		return protocol.SQLStatement_COMMON_DDL, ""
	case prefix(words, "DROP", "VIEW"), prefix(words, "DROP", "INDEX"):
		return protocol.SQLStatement_COMMON_DDL, ""
	case len(words) > 0 && slices.Contains([]string{"INSERT", "UPSERT", "UPDATE", "DELETE", "REPLACE"}, words[0]):
		return protocol.SQLStatement_DESTRUCTIVE, "The operation writes table data; declaration rollback cannot recover previous row values"
	default:
		return protocol.SQLStatement_UNKNOWN, "YDB cannot establish the complete safety effect of this statement; manual review is required"
	}
}
