package inspect

import (
	"slices"

	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// primaryKey gives table the record of its primary key, the columns key
// names in key order, when the read reports keys. The columns' membership
// alone states no order: a table of columns (a, b) can be keyed (b, a), and
// an export of the membership writes the key (a, b). YDB names no key, so
// the record carries the name the host's embedded engine gave it,
// <table>_pkey, which the refusal of a key change names.
func (c Common) primaryKey(table *protocol.Table, key []string) {
	if !c.PrimaryKeys {
		return
	}
	identity := table.GetIdentity()
	table.PrimaryKey = &protocol.ObservedConstraint{Name: identity.GetName() + "_pkey", TableName: identity.GetName(), Schema: identity.GetSchema(),
		Type: "PRIMARY KEY", ColumnNames: slices.Clone(key)}
}
