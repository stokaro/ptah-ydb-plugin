package migrationlint

import (
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
)

// readerRules are the rules that read a statement through a reader of their
// own: how the migrator runs a block or an action call, the value a secret
// statement writes, the credential an external data source names, and what a
// streaming query keeps.
func readerRules() []rule {
	return []rule{
		ydbMixedQueryRule(),
		ydbSchemaSecretInClearRule(),
		ydbDeprecatedSecretRule(),
		ydbStreamingCheckpointRule(),
	}
}

// ydbMixedQueryRule reports a block or an action call that runs a scheme
// statement and a statement that reads or writes a table in one query, which
// YDB refuses whole; [yqlquery.Reader.Mixed] has the measurements. The
// migrator runs every other scheme statement as a query of its own, so only
// those two shapes can mix.
func ydbMixedQueryRule() rule {
	return rule{
		published: published{
			Code:     "YD102",
			Title:    "scheme and data statements in one query",
			Severity: protocol.LintRule_ERROR,
		},
		File: func(file checkedFile) []Violation {
			var reader yqlquery.Reader
			var findings []Violation
			for i := range file.Statements {
				stmt := &file.Statements[i]
				if !reader.Mixed(stmt.SQL) {
					continue
				}
				findings = append(findings, Violation{
					Statement: i,
					Message: "this statement runs a scheme statement and a statement that reads or writes a table in one query, " +
						"which YDB refuses whole (Queries with mixed data and scheme operations are not supported); " +
						"move the scheme statement out of the block or action into a statement of its own",
				})
			}
			return findings
		},
	}
}

// ydbSchemaSecretInClearRule reports a statement that writes a YDB secret's value
// into the migration file: a CREATE SECRET or ALTER SECRET whose value is a
// literal, or a named expression Ptah does not define from the environment,
// and any statement on the deprecated `OBJECT ... (TYPE SECRET)`.
//
// A migration file is committed, copied into artifacts and printed by plans,
// and the value in it is the credential an external data source connects
// with. Measured on 26.2.1.14 and 25.1.4.7, the deprecated form keeps the
// value in `.metadata/secrets/values`, and every value it ever held in
// `values_history`, both readable in clear by the database administrator.
// The finding names the statement and the secret, never the value, so the
// lint output does not repeat what it reports.
func ydbSchemaSecretInClearRule() rule {
	return rule{
		published: published{
			Code:          "YD140",
			Title:         "secret value written in the migration",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			form, path, writes := clearSecretValue(stmt.SQL)
			if !writes {
				return false, ""
			}
			return true, fmt.Sprintf(
				"%s %s writes the secret's value into the migration file, where anyone who reads the file or a plan "+
					"reads it; write `CREATE SECRET %s WITH (value = $%sNAME)` and set %sNAME where the migration "+
					"runs, which Ptah defines when the statement runs and never writes down",
				form, path, path, secretValuePrefix, secretValuePrefix)
		},
	}
}

// ydbDeprecatedSecretRule reports an external data source that names a
// credential by a deprecated secret object, in an option ending in
// _SECRET_NAME.
//
// Measured on 25.1.4.7 and 26.2.1.14, the deprecated object keeps its value
// in `.metadata/secrets/values`, and every value it ever held in
// `values_history`, both readable in clear by the database administrator, and
// the user who created it cannot list it. YDB 25.4 and later take the path of
// a YDB secret instead, in the matching _SECRET_PATH option. The rule warns
// rather than refuses: 25.1, the oldest line Ptah measured, has no other way
// to give a data source a credential.
func ydbDeprecatedSecretRule() rule {
	return rule{
		published: published{
			Code:          "YD141",
			Title:         "external data source credential in a deprecated secret object",
			Severity:      protocol.LintRule_WARNING,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			path, options := deprecatedSecretNames(stmt.SQL)
			if len(options) == 0 {
				return false, ""
			}
			replacements := make([]string, 0, len(options))
			for _, option := range options {
				replacements = append(replacements, strings.TrimSuffix(option, "_NAME")+"_PATH")
			}
			return true, fmt.Sprintf(
				"external data source %s names its credential by %s, a deprecated secret object whose value the "+
					"database administrator reads in clear from .metadata/secrets; on YDB 25.4 and later, keep the "+
					"value in a YDB secret and name it by %s",
				path, strings.Join(options, " and "), strings.Join(replacements, " and "))
		},
	}
}

// YDB 26.2 resets aggregation state when a query body changes under FORCE.
// CREATE OR REPLACE also retains topic offsets while replacing the query.
func ydbStreamingCheckpointRule() rule {
	return rule{
		published: published{
			Code:          "YD160",
			Title:         "streaming query checkpoint state discarded",
			Severity:      protocol.LintRule_WARNING,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			if hasWordPrefix(stmt.Words, "DROP") || !losesCheckpoint(stmt.Words) {
				return false, ""
			}
			return true, "replacing a streaming query or changing its body retains topic offsets but resets aggregation state; rollback cannot recover that state"
		},
	}
}
