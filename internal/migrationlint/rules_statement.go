package migrationlint

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// defaultResourcePool is the pool YDB runs every query in that no classifier
// sends elsewhere. A pool's name is case-sensitive.
const defaultResourcePool = "default"

// topicCodecs are the codecs a topic's supported_codecs keeps.
var topicCodecs = []string{"raw", "gzip", "lzop", "zstd", "custom"}

// statementRules are the rules that judge one statement by what it says: the
// forms YDB refuses whatever the table holds, and the statements YDB runs with
// an effect they do not state.
func statementRules() []rule {
	return []rule{
		ydbUniqueIndexOnExistingTableRule(),
		ydbAddColumnRefusedRule(),
		ydbGrantOptionRevokeRule(),
		ydbPrincipalDropRule(),
		ydbTopicResetRule(),
		ydbTopicSettingIgnoredRule(),
		ydbSecretInClearRule(),
		ydbDefaultPoolDropRule(),
		ydbBackupCollectionDropRule(),
		ydbAnalyzeRule(),
		ydbVectorIndexUnbuiltRule(),
	}
}

// ydbUniqueIndexOnExistingTableRule reports a unique index added to a table
// that exists, which a target without
// [yqlddl.UniqueIndexOnExistingTable] refuses. A table the same migration
// created exists too by the time ALTER TABLE runs, since every scheme
// statement is a query of its own. Measured:
//
//	26.2.1.14, flag off  Failed item check: Adding a unique index to an existing table is disabled
//	25.1.4.7             Unknown index type: syncGlobalUnique
//
// on an empty table and on one holding rows, while the same index declared in
// CREATE TABLE is accepted on both. With EnableAddUniqueIndex on, 26.2.1.14
// builds it over an empty table and fails on a duplicate (`Duplicate key
// found`), which is MF101's and MF102's question rather than this rule's. On a
// target that refuses the index, this rule replaces theirs.
func ydbUniqueIndexOnExistingTableRule() rule {
	return rule{
		published: published{
			Code:          "YD101",
			Title:         "unique index added to an existing table",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
			Subsumes:      []string{"MF101", "MF102"},
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			read := yqlddl.Read(stmt.SQL)
			action, missing := missingRequirement(read, file.Target, yqlddl.UniqueIndexOnExistingTable)
			if !missing {
				return false, ""
			}
			return true, fmt.Sprintf(
				"ADD INDEX %s adds a unique index to %s, a table that exists already, which needs target capability %s, "+
					"unavailable on this %s target; declare the unique index in the CREATE TABLE that creates the table",
				action.Index.Name, read.Name, yqlddl.UniqueIndexOnExistingTable, ydbEngine)
		},
	}
}

// missingRequirement returns the first action of a statement that needs key,
// and reports whether the target lacks it; see
// [yqlddl.Statement.Requirements].
func missingRequirement(read yqlddl.Statement, target Target, key yqlddl.Capability) (yqlddl.Action, bool) {
	if target.Has(key) {
		return yqlddl.Action{}, false
	}
	for _, requirement := range read.Requirements() {
		if requirement.Capability == key && !requirement.Inline {
			return read.Actions[requirement.Action], true
		}
	}
	return yqlddl.Action{}, false
}

// ydbAddColumnRefusedRule reports an ADD COLUMN YDB refuses. Measured on
// 26.2.1.14 and 25.1.4.7, on an empty table and on one holding rows:
//
//	ADD COLUMN c Int64 NOT NULL           Cannot add not null column without default value
//	ADD COLUMN c Int64 [NOT NULL] DEFAULT 7
//	                                      26.2.1.14: accepted, existing rows take the default
//	                                      25.1.4.7: Column addition with default value is not supported now.
//
// The first is refused on every line, the second where the target lacks
// [yqlddl.AddColumnWithDefault]. Neither depends on the rows, so a table
// this migration created is no exception, which is where it differs from
// DD101; it replaces DD101 on the statement.
func ydbAddColumnRefusedRule() rule {
	return rule{
		published: published{
			Code:          "YD103",
			Title:         "column added in a form YDB refuses",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
			Subsumes:      []string{"DD101"},
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			read := yqlddl.Read(stmt.SQL)
			if action, missing := missingRequirement(read, file.Target, yqlddl.AddColumnWithDefault); missing {
				return true, fmt.Sprintf(
					"ADD COLUMN %s gives the column a default, which needs target capability %s, unavailable on this %s target; "+
						"add the column without a default and write its values in a data statement",
					action.Column.Name, yqlddl.AddColumnWithDefault, ydbEngine)
			}
			withDefault := file.Target.Has(yqlddl.AddColumnWithDefault)
			for _, action := range read.Actions {
				if action.Kind != yqlddl.AddColumn {
					continue
				}
				column := action.Column
				switch {
				case column.NotNull && !column.Default && withDefault:
					return true, fmt.Sprintf(
						"ADD COLUMN %s is NOT NULL without a default, which YDB refuses even on an empty table "+
							"(Cannot add not null column without default value); give it a DEFAULT, which existing rows take",
						column.Name)
				case column.NotNull && !column.Default:
					return true, fmt.Sprintf(
						"ADD COLUMN %s is NOT NULL without a default, which YDB refuses even on an empty table "+
							"(Cannot add not null column without default value), and a default needs target capability %s, "+
							"unavailable on this %s target; add the column as nullable",
						column.Name, yqlddl.AddColumnWithDefault, ydbEngine)
				}
			}
			return false, ""
		},
	}
}

// ydbGrantOptionRevokeRule reports REVOKE GRANT OPTION FOR, which on YDB takes
// away the permission it names together with the right to grant it. YDB keeps
// WITH GRANT OPTION as a permission of its own, ydb.access.grant, beside the
// one it was written with, and the clause revokes both. Measured on 25.1.4.7
// and 26.2.1.14: after `GRANT SELECT ... WITH GRANT OPTION`, the object holds
// ydb.generic.read and ydb.access.grant for the subject, and `REVOKE GRANT
// OPTION FOR SELECT ...` leaves it neither, where the statement reads as
// keeping SELECT. Revoking the GRANT permission alone takes the option and
// keeps the rest.
func ydbGrantOptionRevokeRule() rule {
	return rule{
		published: published{
			Code:          "YD110",
			Title:         "REVOKE GRANT OPTION FOR revokes the permission too",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			if !hasWordPrefix(stmt.Words, "REVOKE", "GRANT", "OPTION", "FOR") {
				return false, ""
			}
			return true, "REVOKE GRANT OPTION FOR takes away the permission it names as well as " +
				"the right to grant it, because YDB keeps the grant option as the permission ydb.access.grant; " +
				"revoke 'ydb.access.grant' alone to keep the permission"
		},
	}
}

// ydbPrincipalDropRule reports DROP USER and DROP GROUP, which leave every
// permission entry of the principal behind. Measured on 25.1.4.7 and
// 26.2.1.14: after `DROP USER u`, .sys/auth_permissions still lists u's
// entries, and a user created later under the same name holds them. DS107
// reports the drop itself; this rule says what it leaves.
func ydbPrincipalDropRule() rule {
	return rule{
		published: published{
			Code:          "YD111",
			Title:         "dropped user or group keeps its permissions",
			Severity:      protocol.LintRule_WARNING,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			if !hasWordPrefix(stmt.Words, "DROP") || len(stmt.Words) < 2 {
				return false, ""
			}
			kind := stmt.Words[1]
			if kind != "USER" && kind != "GROUP" {
				return false, ""
			}
			return true, fmt.Sprintf("DROP %s leaves the %s's permissions on every object it was granted, and a "+
				"user or group created later under the same name holds them; REVOKE ALL ON each object FROM it first",
				kind, strings.ToLower(kind))
		},
	}
}

// ydbTopicResetRule reports an ALTER TOPIC that resets a topic setting, or a
// consumer setting other than availability_period. Measured with every
// setting a topic and a consumer take:
//
//	26.2.1.14  accepted, and every value reads back as it was
//	25.1.4.7   Empty list, did you forget quote?   (topic RESET)
//	           INTERNAL_ERROR ... TCallable(): requirement Match(node.Get()) failed   (consumer RESET)
//
// so a migration that relies on RESET changes nothing on one line and fails
// on the other. A consumer's availability_period is the exception: 26.2.1.14
// resets it, and 25.1.4.7 has no availability period to reset. Ptah writes
// SET with the value instead.
func ydbTopicResetRule() rule {
	return rule{
		published: published{
			Code:          "YD113",
			Title:         "topic setting reset",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			read := yqlddl.Read(stmt.SQL)
			if read.Kind != yqlddl.AlterTopic {
				return false, ""
			}
			for _, action := range read.Actions {
				names := resetNames(action)
				if len(names) == 0 {
					continue
				}
				owner := "topic " + read.Name
				if action.Kind == yqlddl.ResetConsumerSettings {
					owner = "consumer " + action.Consumer + " of topic " + read.Name
				}
				return true, fmt.Sprintf("RESET (%s) of %s changes nothing on YDB 26.2, which keeps every value as it "+
					"was, and is refused on 25.1; SET each setting to the value it is to hold instead",
					strings.ToLower(strings.Join(names, ", ")), owner)
			}
			return false, ""
		},
	}
}

// resetNames returns the settings an ALTER TOPIC action resets that YDB keeps
// as they were.
func resetNames(action yqlddl.Action) []string {
	var names []string
	switch action.Kind {
	case yqlddl.ResetSettings:
		for _, setting := range action.Settings {
			names = append(names, setting.Name)
		}
	case yqlddl.ResetConsumerSettings:
		for _, setting := range action.Settings {
			if setting.Name != "AVAILABILITY_PERIOD" {
				names = append(names, setting.Name)
			}
		}
	}
	return names
}

// ydbTopicSettingIgnoredRule reports a topic setting YDB accepts and keeps as
// nothing, so the statement runs and the topic does not hold what it says.
// Measured on 26.2.1.14 and 25.1.4.7 by reading the topic back through
// DescribeTopic:
//
//   - retention_storage_mb and partition_count_limit, in CREATE TOPIC and in
//     ALTER TOPIC SET, keep no value;
//   - metering_mode in CREATE TOPIC keeps none outside a serverless database
//     (ALTER TOPIC refuses it: `Metering mode can only be specified in a
//     serverless database`);
//   - supported_codecs naming a codec other than raw, gzip, lzop, zstd and
//     custom keeps no codec list at all, `raw,foo` included;
//   - max_active_partitions in a CREATE TOPIC without a strategy other than
//     disabled keeps the default: max_active_partitions = 5 reads back as 1;
//   - a consumer of type shared in CREATE TOPIC is created as an ordinary
//     consumer on 26.2.1.14 (ADD CONSUMER refuses it: `shared consumers is
//     disabled`), and 25.1.4.7 refuses the option.
func ydbTopicSettingIgnoredRule() rule {
	return rule{
		published: published{
			Code:          "YD114",
			Title:         "topic setting YDB keeps as nothing",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			read := yqlddl.Read(stmt.SQL)
			if read.Kind != yqlddl.CreateTopic && read.Kind != yqlddl.AlterTopic {
				return false, ""
			}
			reason := ignoredTopicSetting(read)
			if reason == "" {
				return false, ""
			}
			return true, fmt.Sprintf("%s of topic %s: YDB accepts the statement and keeps nothing of it, so the topic "+
				"does not hold what the statement says", reason, read.Name)
		},
	}
}

// ignoredTopicSetting names the first setting of read YDB keeps as nothing,
// or is empty.
func ignoredTopicSetting(read yqlddl.Statement) string {
	if reason := ignoredTopicOwnSetting(read); reason != "" {
		return reason
	}
	return ignoredConsumerSetting(read)
}

// ignoredTopicOwnSetting names the first setting of the topic itself that
// YDB keeps as nothing, or is empty.
func ignoredTopicOwnSetting(read yqlddl.Statement) string {
	settings := read.Settings
	for _, action := range read.Actions {
		if action.Kind == yqlddl.SetSettings {
			settings = append(settings, action.Settings...)
		}
	}
	strategy := ""
	for _, setting := range settings {
		switch setting.Name {
		case "RETENTION_STORAGE_MB", "PARTITION_COUNT_LIMIT":
			return strings.ToLower(setting.Name)
		case "METERING_MODE":
			if read.Kind == yqlddl.CreateTopic {
				return "metering_mode outside a serverless database"
			}
		case "SUPPORTED_CODECS":
			if codec := unknownCodec(setting.Text); codec != "" {
				return fmt.Sprintf("supported_codecs naming codec %q", codec)
			}
		case "AUTO_PARTITIONING_STRATEGY":
			strategy = strings.ToLower(setting.Text)
		}
	}
	if read.Kind == yqlddl.CreateTopic && (strategy == "" || strategy == "disabled") &&
		slices.ContainsFunc(settings, func(setting yqlddl.Setting) bool { return setting.Name == "MAX_ACTIVE_PARTITIONS" }) {
		return "max_active_partitions without an auto_partitioning_strategy that enables auto-partitioning"
	}
	return ""
}

// ignoredConsumerSetting names the first setting of a consumer the statement
// creates that YDB keeps as nothing, or is empty.
func ignoredConsumerSetting(read yqlddl.Statement) string {
	consumers := slices.Clone(read.Consumers)
	for _, action := range read.Actions {
		if action.Kind == yqlddl.AddConsumer {
			consumers = append(consumers, yqlddl.Consumer{Name: action.Consumer, Settings: action.Settings})
		}
	}
	for _, consumer := range consumers {
		for _, setting := range consumer.Settings {
			switch {
			case setting.Name == "TYPE" && strings.EqualFold(setting.Text, "shared") && read.Kind == yqlddl.CreateTopic:
				return fmt.Sprintf("consumer %s of type shared", consumer.Name)
			case setting.Name == "SUPPORTED_CODECS" && unknownCodec(setting.Text) != "":
				return fmt.Sprintf("supported_codecs of consumer %s naming codec %q", consumer.Name, unknownCodec(setting.Text))
			}
		}
	}
	return ""
}

// unknownCodec returns the first codec of a codec list YDB keeps no list for,
// or is empty.
func unknownCodec(list string) string {
	if strings.TrimSpace(list) == "" {
		return ""
	}
	for part := range strings.SplitSeq(list, ",") {
		codec := strings.ToLower(strings.TrimSpace(part))
		if !slices.Contains(topicCodecs, codec) {
			return codec
		}
	}
	return ""
}

// ydbSecretInClearRule reports an async replication or a transfer given a
// password or a token as a value rather than as the secret that holds it.
// Measured on 26.2.1.14 and 25.1.4.7: `PASSWORD = '...'` and `TOKEN = '...'`
// are accepted in CREATE ASYNC REPLICATION and CREATE TRANSFER, and the
// description reads back neither, the password as no password at all and the
// token as no credential. The value stays in the migration file, and in every
// copy of it, while the database holds a secret nobody can read back.
func ydbSecretInClearRule() rule {
	return rule{
		published: published{
			Code:          "YD116",
			Title:         "secret written in clear",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			read := yqlddl.Read(stmt.SQL)
			switch read.Kind {
			case yqlddl.CreateAsyncReplication, yqlddl.AlterAsyncReplication,
				yqlddl.CreateTransfer, yqlddl.AlterTransfer:
			default:
				return false, ""
			}
			settings := slices.Clone(read.Settings)
			for _, action := range read.Actions {
				settings = append(settings, action.Settings...)
			}
			for _, setting := range settings {
				if setting.Name == "PASSWORD" || setting.Name == "TOKEN" {
					return true, fmt.Sprintf("%s of %s is written in clear: the migration file now holds the "+
						"secret, and YDB keeps it without reading it back; put it in a secret and name it with %s_SECRET_NAME "+
						"or %s_SECRET_PATH", setting.Name, read.Name, setting.Name, setting.Name)
				}
			}
			return false, ""
		},
	}
}

// ydbDefaultPoolDropRule reports DROP RESOURCE POOL default. Measured on
// 25.1.4.7 and 26.2.1.14 with EnableResourcePools on: YDB takes the statement,
// and every later query of the database -- a SELECT, and CREATE RESOURCE POOL
// default itself -- answers `Resource pool default not found or you don't have
// access permissions`. Only a restart of a local-ydb server brought the
// database back, by creating a new one. A pool's name is case-sensitive, so
// only `default` itself is reported.
func ydbDefaultPoolDropRule() rule {
	return rule{
		published: published{
			Code:          "YD120",
			Title:         "the resource pool default dropped",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			read := yqlddl.Read(stmt.SQL)
			if read.Kind != yqlddl.DropResourcePool || read.Name != defaultResourcePool {
				return false, ""
			}
			return true, "DROP RESOURCE POOL default drops the pool YDB runs every query in that no classifier " +
				"sends elsewhere; YDB takes it, and every later query of the database, CREATE RESOURCE POOL " +
				"default included, fails with `Resource pool default not found`. Change its settings with " +
				"ALTER RESOURCE POOL default instead"
		},
	}
}

// ydbBackupCollectionDropRule reports DROP BACKUP COLLECTION, which deletes
// every backup the collection holds: a backup is a set of tables under the
// collection's path, and they go with it. Measured on 25.1.4.7 with
// EnableBackupService on, the statement stops the server: the ydbd process
// exits, and its last log line is the schemeshard's TDropBackupCollection
// propose, on a collection that held no backup as well as one that did.
// 26.2.1.14 drops the collection and serves on.
func ydbBackupCollectionDropRule() rule {
	return rule{
		published: published{
			Code:          "YD121",
			Title:         "backup collection dropped",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			if yqlddl.Read(stmt.SQL).Kind != yqlddl.DropBackupCollection {
				return false, ""
			}
			return true, "DROP BACKUP COLLECTION deletes every backup the collection holds, and a YDB 25.1 " +
				"server stops on it: measured on 25.1.4.7, the server process exits; copy or restore the " +
				"backups first, and run it only on a later line"
		},
	}
}

// ydbAnalyzeRule reports ANALYZE in a migration. Statistics are data the
// server collects rather than schema a migration declares, and YDB runs the
// statement only where a flag that is off by default is on. Measured with
// default flags, 26.2.1.14 answers `ANALYZE command is not supported because
// EnableColumnStatistics feature flag is off`. 25.1.4.7 answers `analyze is
// not supported for oltp tables` for a row table with the flag on or off.
// With the flag on, both lines still need a statistics service the database
// may not run (local-ydb answers `Can't get statistics aggregator ID` for a
// column table), and refuse more than one table in a statement (`ANALYZE
// with multitables hasn't been implemented yet`). The migrator runs each scheme statement as a query of its own, so a
// refused ANALYZE stops the migration after the statements before it applied.
func ydbAnalyzeRule() rule {
	return rule{
		published: published{
			Code:          "YD122",
			Title:         "ANALYZE in a migration",
			Severity:      protocol.LintRule_WARNING,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			if yqlddl.Read(stmt.SQL).Kind != yqlddl.Analyze {
				return false, ""
			}
			return true, "ANALYZE collects column statistics and changes no schema; YDB runs it only with the " +
				"EnableColumnStatistics flag on, which is off by default, and refuses it on a row table on 25.1, " +
				"so on such a cluster the migration stops here after the statements before it applied. Collect " +
				"statistics outside the migration"
		},
	}
}

// ydbVectorIndexUnbuiltRule reports a vector index, inline in CREATE TABLE or
// added by ALTER TABLE, on a target that does not build it: one without
// [yqlddl.VectorIndexes], or, for an index over bit vectors, one without
// [yqlddl.VectorBitType]. Measured on local-ydb:
//
//	25.1.4.7, flag off   Vector index support is disabled
//	25.1.4.7, 25.2.1.24  bit vector type is not supported
//	25.3.1.25, 25.4.1.15 Unsupported vector_type: VECTOR_TYPE_BIT
//
// 25.1.4.7 accepts a bit index on an empty table and refuses to build it over
// rows, so a migration that creates the table applies and the first rebuild
// fails. [yqlddl.Statement.Requirements] decides which statement needs which
// key.
func ydbVectorIndexUnbuiltRule() rule {
	return rule{
		published: published{
			Code:          "YD130",
			Title:         "vector index the target does not build",
			Severity:      protocol.LintRule_ERROR,
			AppliesToDown: true,
		},
		Statement: func(file checkedFile, stmt *Statement) (bool, string) {
			read := yqlddl.Read(stmt.SQL)
			for _, requirement := range read.Requirements() {
				if file.Target.Has(requirement.Capability) {
					continue
				}
				name := requiredIndex(read, requirement).Name
				switch requirement.Capability {
				case yqlddl.VectorIndexes:
					return true, fmt.Sprintf("vector index %s on %s needs target capability %s, unavailable on this %s "+
						"target; YDB 25.1 keeps vector indexes behind the EnableVectorIndex feature flag (Vector index "+
						"support is disabled), so turn the flag on for the cluster", name, read.Name, yqlddl.VectorIndexes,
						ydbEngine)
				case yqlddl.VectorBitType:
					return true, fmt.Sprintf("vector index %s on %s stores bit vectors, which needs target capability %s, "+
						"unavailable on this %s target, where the index is not built over rows; declare vector_type float, "+
						"uint8 or int8", name, read.Name, yqlddl.VectorBitType, ydbEngine)
				}
			}
			return false, ""
		},
	}
}

// requiredIndex is the index a requirement of read belongs to: the one a
// CREATE TABLE declares inline, or the one an ALTER TABLE action adds.
func requiredIndex(read yqlddl.Statement, requirement yqlddl.Requirement) yqlddl.Index {
	if requirement.Inline {
		return read.Indexes[requirement.Action]
	}
	return read.Actions[requirement.Action].Index
}
