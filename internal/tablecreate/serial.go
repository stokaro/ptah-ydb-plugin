package tablecreate

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// A Serial column, and how a table is created with one.
//
// A column is a Serial when its type is Serial, BigSerial or SmallSerial (or
// a SERIAL spelling YDB shares with PostgreSQL), or when an Int16, Int32 or
// Int64 column is declared auto_increment or with an identity, as the host's
// embedded engine reads it. Its sequence is the column's default.
//
// The native CreateTable call takes the sequence with the column, start and
// increment included, so a new table gets the declared settings in the call
// that creates it. Measured on 25.1.4.7 and 26.2.1.14, a sequence created that
// way issues its start first and records no restart, where YQL's CREATE TABLE
// takes no sequence setting and the host's embedded engine follows it with an
// ALTER SEQUENCE ... RESTART that YDB then replays on every later ALTER
// SEQUENCE. The call gives a sequence the Int64 maximum unless the request
// names one, whatever the column's width, so the request names the maximum of
// the column's type, as YQL's Serial and SmallSerial have it.

// serialSequence is the sequence a Serial column of type serialType named
// column is created with.
func serialSequence(column, serialType string, settings ydbsequence.Settings) *Ydb_Table.SequenceDescription {
	return &Ydb_Table.SequenceDescription{Name: proto.String(ydbsequence.Name(column)), MinValue: proto.Int64(1),
		MaxValue: proto.Int64(ydbsequence.Maximum(serialType)), StartValue: proto.Int64(settings.Start), Cache: proto.Uint64(1),
		Increment: proto.Int64(settings.Increment), Cycle: proto.Bool(false)}
}

// declaresSerial reports whether a declaration asks for a Serial column of the
// type it maps to.
func declaresSerial(column *protocol.DeclaredColumn, mapping ydbtype.Mapping) bool {
	return mapping.Serial || column.AutoIncrement || column.IdentityGeneration != ""
}

// serialColumn answers what a declared column, which subject names, is
// created as: the YDB type it stores and, for a Serial, the sequence it fills
// from. A column that is not
// a Serial gets its mapped type and no sequence. It refuses, in the host's
// embedded engine's words, a Serial YDB has no type for or whose sequence
// declaration it cannot honor.
func serialColumn(subject string, column *protocol.DeclaredColumn, mapping ydbtype.Mapping, caps map[string]bool) (string, *Ydb_Table.SequenceDescription, error) {
	if !declaresSerial(column, mapping) {
		if column.IdentityStart != "" || column.IdentityIncrement != "" || column.IdentityOptions != "" {
			return "", nil, unsupported("%s gives a sequence a start or an increment, and only a Serial column has a sequence", subject)
		}
		return mapping.Type, nil, nil
	}
	serialType := mapping.Type
	if !mapping.Serial {
		serial, ok := ydbsequence.SerialFor(mapping.Type)
		switch {
		case !ok:
			return "", nil, unsupported("%s: an auto-increment column is a Serial, which YDB has for Int16, Int32 and Int64 only, and the column is %s", subject, mapping.Type)
		case !caps[ydbtype.SerialColumns]:
			return "", nil, unsupported("%s", keyRefusal(ydbtype.SerialColumns, subject+" increments itself"))
		}
		serialType = serial
	}
	switch {
	case strings.EqualFold(column.IdentityGeneration, "ALWAYS"):
		return "", nil, unsupported("%s: GENERATED ALWAYS refuses an explicit value, and a YDB Serial takes one", subject)
	case column.IdentityOptions != "":
		return "", nil, unsupported("%s: a YDB Serial's sequence takes a start and an increment and nothing else, "+
			"so declare those with identity_start and identity_increment rather than the options %s", subject, column.IdentityOptions)
	case column.DefaultSet || column.DefaultValue != "" || column.DefaultExpression != "":
		return "", nil, unsupported("%s: a Serial column takes its default from its sequence", subject)
	}
	settings, err := ydbsequence.Parse(column.IdentityStart, column.IdentityIncrement)
	if err != nil {
		return "", nil, unsupported("%s: %v", subject, err)
	}
	return ydbtype.StorageType(serialType), serialSequence(column.Name, serialType, settings), nil
}

// sameSerial checks intent, the native sequence a declaration states for the
// column that subject names, against compiled, the sequence the column's
// common declaration gives it. A read states a Serial twice: as the common
// column's auto_increment, start and increment, and as the native sequence
// its default fills from. So a declaration converted from a read carries both,
// and the native one adds nothing to the creation. It has to be the owned
// sequence of a Serial column that stores stored, with the start and the
// increment the common column declares.
func sameSerial(subject, column, stored string, compiled *Ydb_Table.SequenceDescription, intent *native.SequenceIntent) error {
	if compiled == nil {
		return unsupported("%s takes its default from a sequence, and only a Serial column has one", subject)
	}
	held, err := columnsemantics.SerialSettings(column, stored, &native.SequenceDescription{Name: intent.Name, MinValue: intent.MinValue,
		MaxValue: intent.MaxValue, StartValue: intent.StartValue, Cache: intent.Cache, Increment: intent.Increment, Cycle: intent.Cycle,
		DataType: intent.DataType})
	if err != nil {
		return unsupported("%s: %v", subject, err)
	}
	if declared := requestedSettings(compiled); held != declared {
		return unsupported("%s: its native sequence starts at %d and steps by %d, and the column declares a start of %d and an increment of %d",
			subject, held.Start, held.Increment, declared.Start, declared.Increment)
	}
	return nil
}

// serialSettingsRefusal refuses a creation whose Serial sequences state
// settings the target does not take (see sequenceSettingsRefusal), naming the
// column of the table at table. It is a question about the target rather than
// about what the declaration means, so a projection of the creation does not
// ask it, and the refusal reaches the host from validation and planning, in
// its own words.
func serialSettingsRefusal(request *Ydb_Table.CreateTableRequest, table string, caps map[string]bool) error {
	for _, column := range request.GetColumns() {
		sequence := column.GetFromSequence()
		if sequence == nil {
			continue
		}
		subject := fmt.Sprintf("column %q of table %q", column.GetName(), table)
		if reason := sequenceSettingsRefusal(subject, SerialTypeOf(column), requestedSettings(sequence), caps); reason != "" {
			return unsupported("%s", reason)
		}
	}
	return nil
}

// sequenceSettingsRefusal says why the target takes no settings for the
// sequence of a Serial of type serialType that subject names, or "" when it
// takes them: settings other than the default need serial_sequence_options,
// and those of a 16-bit or 32-bit Serial need serial_sequence_keeps_range too
// (see [ydbsequence.RangeRefusal]).
func sequenceSettingsRefusal(subject, serialType string, settings ydbsequence.Settings, caps map[string]bool) string {
	if settings.IsDefault() {
		return ""
	}
	if !caps[ydbsequence.Options] {
		return keyRefusal(ydbsequence.Options, "giving the sequence of "+subject+" a start or an increment")
	}
	if reason := ydbsequence.RangeRefusal(serialType, settings, caps[ydbsequence.KeepsRange]); reason != "" {
		return keyRefusal(ydbsequence.KeepsRange, "giving the sequence of "+subject+" a start or an increment ("+reason+")")
	}
	return ""
}

// keyRefusal words the refusal of what subject asks for on a target that lacks
// the capability key, as the host's embedded engine words it.
func keyRefusal(key, subject string) string {
	return fmt.Sprintf("%s, which requires target capability %s, unavailable on this ydb target", subject, key)
}

// SerialTypeOf is the Serial type a column of a creation request is, from the
// integer type its sequence fills, or "" for a column without a sequence.
func SerialTypeOf(column *Ydb_Table.ColumnMeta) string {
	if column.GetFromSequence() == nil {
		return ""
	}
	spelling, _, err := schema.ColumnType(column.GetType())
	if err != nil {
		return ""
	}
	serial, _ := ydbsequence.SerialFor(spelling)
	return serial
}

// validateSerial holds the sequence of a creation request's column to what
// compilation writes, so a forged request cannot create a sequence the
// declaration could not state: one named otherwise, one that cycles, caches,
// starts below 1 or past the column's maximum, counts down, or ends past the
// column's type.
func validateSerial(column *Ydb_Table.ColumnMeta) error {
	sequence := column.GetFromSequence()
	if sequence == nil {
		return nil
	}
	if err := only(sequence, "name", "min_value", "max_value", "start_value", "cache", "increment", "cycle"); err != nil {
		return err
	}
	serialType := SerialTypeOf(column)
	if serialType == "" || column.GetType().GetOptionalType() != nil || !column.GetNotNull() {
		return fmt.Errorf("a YDB Serial column is a NOT NULL Int16, Int32 or Int64")
	}
	maximum := ydbsequence.Maximum(serialType)
	if sequence.GetName() != ydbsequence.Name(column.GetName()) || sequence.MinValue == nil || sequence.GetMinValue() != 1 ||
		sequence.GetMaxValue() != maximum || sequence.Cache == nil || sequence.GetCache() != 1 || sequence.Cycle == nil || sequence.GetCycle() ||
		sequence.StartValue == nil || sequence.GetStartValue() < 1 || sequence.GetStartValue() > maximum ||
		sequence.Increment == nil || sequence.GetIncrement() < 1 {
		return fmt.Errorf("the sequence of YDB Serial column %q differs from the one a declaration creates", column.GetName())
	}
	return nil
}

// requestedSettings is the start and the increment a sequence of a creation
// request states, a setting it leaves out read as 1.
func requestedSettings(sequence *Ydb_Table.SequenceDescription) ydbsequence.Settings {
	settings := ydbsequence.Default
	if sequence.StartValue != nil {
		settings.Start = sequence.GetStartValue()
	}
	if sequence.Increment != nil {
		settings.Increment = sequence.GetIncrement()
	}
	return settings
}

// createdSequenceStatements are the ALTER SEQUENCE statements that follow the
// CREATE TABLE of a creation request at path, in the database at database: one
// for each Serial whose sequence states a start or an increment other than 1,
// which restarts the sequence at its start, because START alone leaves the
// next value at 1 and the new table holds no row a restart could collide with.
//
// Such a statement names the sequence by its absolute path, so a request in
// no named database has no text form; nor has a 16-bit or 32-bit Serial with
// such settings, whose maximum the statement would raise to the Int64 maximum.
func createdSequenceStatements(database, path string, request *Ydb_Table.CreateTableRequest) ([]string, error) {
	var statements []string
	for _, column := range request.GetColumns() {
		sequence := column.GetFromSequence()
		if sequence == nil {
			continue
		}
		settings := requestedSettings(sequence)
		if settings.IsDefault() {
			continue
		}
		switch {
		case SerialTypeOf(column) != ydbtype.BigSerial:
			return nil, noTextForm("the sequence of column %q gets a start or an increment, and the ALTER SEQUENCE that would give it one raises the maximum of a 16-bit or 32-bit Serial to the Int64 maximum", column.GetName())
		case database == "":
			return nil, noTextForm("the sequence of column %q takes its start and increment from an ALTER SEQUENCE, which names the sequence by its absolute path, and the creation names no database", column.GetName())
		}
		statements = append(statements, SequenceStatement(ydbsequence.Path(database, "", path, column.GetName()), settings, settings.Start != 1))
	}
	return statements, nil
}

// SequenceStatement is the ALTER SEQUENCE that gives the sequence at the
// absolute path absolute the settings, as the host's embedded engine writes
// it: START WITH and INCREMENT BY always, and RESTART WITH the start when
// restart is set. YDB moves the next value only on a RESTART.
func SequenceStatement(absolute string, settings ydbsequence.Settings, restart bool) string {
	statement := fmt.Sprintf("ALTER SEQUENCE %s START WITH %d INCREMENT BY %d", QuoteIdentifier(absolute), settings.Start, settings.Increment)
	if restart {
		statement += fmt.Sprintf(" RESTART WITH %d", settings.Start)
	}
	return statement + ";"
}

// serialSpelling is the type a comparison reads for a column stored as the
// integer type storage: its Serial type when serial holds and one exists.
func serialSpelling(storage string, serial bool) string {
	if !serial {
		return storage
	}
	if spelling, ok := ydbsequence.SerialFor(storage); ok {
		return spelling
	}
	return storage
}

// serialRebuildRefusal says why a table declared as desired cannot be rebuilt,
// in the host's embedded engine's words, or "": the new table's sequence would
// start at 1 while the copied rows keep their values, and ALTER SEQUENCE ...
// RESTART WITH takes only a literal, so no statement can move it past them.
func serialRebuildRefusal(desired *Ydb_Table.CreateTableRequest) string {
	for _, column := range desired.GetColumns() {
		if column.GetFromSequence() != nil {
			return fmt.Sprintf("column %q takes its values from a sequence. The new table's "+
				"sequence would start at 1 while the copied rows keep theirs, so the next insert would collide "+
				"(`Conflict with existing key`), and YDB's ALTER SEQUENCE ... RESTART WITH takes only a literal, "+
				"so no statement can move it past the copied rows", column.GetName())
		}
	}
	return ""
}
