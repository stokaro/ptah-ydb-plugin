package yqlschema

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// HeaderKinds are the kinds a `ptah:not-described` header of a YQL document
// may name, as the host's YDB owner reads them. The host hands this provider
// the header's entries of these kinds as limits on its claim.
func HeaderKinds() []string {
	return []string{"coordination_node", "streaming_query", "resource_pool", "resource_pool_classifier", "secret", "topic",
		"external_data_source", "external_table", "replication", "transfer"}
}

// ClaimedModels are the desired models whose coverage a YQL document read for
// request claims: a row table's settings, a column's, a column table's
// storage, a coordination node, a topic, an external data source, an external
// table, an async replication and a transfer, a table's changefeeds when the
// host takes the facets of a reading (see [takesFacets]), and an index's
// partitioning and read replicas when it also reads an index's own facets
// (see [takesIndexFacets]), and a table's vector and full-text indexes when
// its sources state them too (see [takesNativeIndexes]). Their source formats
// include SQL. Without that feature the reading cannot say a changefeed, so
// it claims nothing about the changefeeds of its tables and leaves those a
// table holds unmanaged; an index's settings and a table's vector and
// full-text indexes are left so in the same way. A table's settings, a
// column's and a column table's storage are claimed either way, as a YAML or
// Go source claims them; without the feature a load that declares one is
// refused.
func ClaimedModels(request *protocol.ReadSQLSchemaRequest) []*protocol.ValueModel {
	models := claimedModels()
	if takesFacets(request) {
		models = append(models, schema.NativeDesiredModel(schema.ChangefeedProperties))
	}
	if takesIndexFacets(request) {
		models = append(models, schema.NativeDesiredModel(schema.IndexSettingsProperties))
	}
	if takesNativeIndexes(request) {
		models = append(models, schema.NativeDesiredModel(schema.IndexProperties))
	}
	return models
}

// takesFacets reports whether the host that sent request takes the facets of
// a reading, provider.SQLFacetsFeature, which a table's settings, the family of
// a column and a table's changefeeds need.
func takesFacets(request *protocol.ReadSQLSchemaRequest) bool {
	return slices.Contains(request.GetTargetFeatures(), provider.SQLFacetsFeature)
}

// takesIndexFacets reports whether the host that sent request takes the facet
// of an index's partitioning and read replicas: it takes the facets of a
// reading and reads an index's own facets, provider.ObservedIndexFacetsFeature.
// Without the second the comparison could not see what an index holds, so a
// declared setting would be dropped on every index the table already has, as
// a YAML or Go source's would.
func takesIndexFacets(request *protocol.ReadSQLSchemaRequest) bool {
	return takesFacets(request) && slices.Contains(request.GetTargetFeatures(), provider.ObservedIndexFacetsFeature)
}

// takesNativeIndexes reports whether the host that sent request takes a
// table's vector, full-text and local indexes in a SQL schema reading: it
// takes the facets of a reading, and its sources state those indexes
// (provider.IndexKindsFeature), so the comparison reads a table a source
// declares with none of them as one that has none. The reading then writes a
// table's vector and full-text indexes as one facet of the table and a column
// table's local indexes in its storage facet, the values a YAML or Go source
// of the table declares, and builds no common index of them.
func takesNativeIndexes(request *protocol.ReadSQLSchemaRequest) bool {
	return takesFacets(request) && slices.Contains(request.GetTargetFeatures(), provider.IndexKindsFeature)
}

func claimedModels() []*protocol.ValueModel {
	return []*protocol.ValueModel{
		schema.NativeDesiredModel(schema.SecretProperties),
		schema.TableDesiredModel(),
		schema.ColumnDesiredModel(),
		schema.NativeDesiredModel(schema.ColumnStorageProperties),
		schema.CoordinationDesiredModel(),
		schema.NativeDesiredModel(schema.StreamingQueryProperties),
		schema.NativeDesiredModel(schema.TopicProperties),
		schema.NativeDesiredModel(schema.ExternalSourceProperties),
		schema.NativeDesiredModel(schema.ExternalTableProperties),
		schema.NativeDesiredModel(schema.ResourcePoolProperties),
		schema.NativeDesiredModel(schema.ResourcePoolClassifierProperties),
		schema.NativeDesiredModel(schema.ReplicationProperties),
		schema.NativeDesiredModel(schema.TransferProperties),
	}
}

// Reply reads the documents of a ReadSQLSchema request and answers with the
// reading: the tables, views, principals, grants and coordination nodes the
// load declares, the facets of the tables' settings and changefeeds for a
// host that takes them, what each document declares, the coverage the load
// claims, and a trace for every object. A refused load, and a reading that declares
// something a SQL schema reading has no home for, is answered with complete
// false and diagnostics that name what is refused, and with no schema.
func Reply(request *protocol.ReadSQLSchemaRequest) *protocol.SQLSchema {
	documents := make([]Document, 0, len(request.GetDocuments()))
	for _, document := range request.GetDocuments() {
		documents = append(documents, Document{Path: document.GetPath(), Text: document.GetText()})
	}
	reading, refusal := Read(documents, request.GetDatabasePath())
	if refusal != nil {
		return refused(request, []*Refusal{refusal})
	}
	coverage, refusal := claim(request.GetLimits(), ClaimedModels(request))
	if refusal != nil {
		refusal.Document, refusal.Start, refusal.End = limitDocument(request, refusal.limit)
		return refused(request, []*Refusal{refusal})
	}
	// The host settles the whole load as it settles one document, so a table
	// two documents declare is one table, with the columns of both.
	settled := reading.clone()
	finalize(settled)
	reply, gaps := wire(request, settled)
	if len(gaps) != 0 {
		return refused(request, gaps)
	}
	reply.Coverage = coverage
	return reply
}

// refused is the reply to a refused load.
func refused(request *protocol.ReadSQLSchemaRequest, refusals []*Refusal) *protocol.SQLSchema {
	slices.SortStableFunc(refusals, func(a, b *Refusal) int {
		if a.Document != b.Document {
			return a.Document - b.Document
		}
		return a.Start - b.Start
	})
	reply := &protocol.SQLSchema{}
	undeclared := slices.Contains(request.GetTargetFeatures(), provider.SQLUndeclaredFeature)
	for _, refusal := range refusals {
		document := request.GetDocuments()[refusal.Document]
		reply.Diagnostics = append(reply.Diagnostics, &protocol.SQLDiagnostic{
			Code: diagnosticCode(refusal.Code, undeclared), Severity: protocol.SQLDiagnostic_ERROR, DocumentId: document.GetId(),
			Span: sourceSpan(document.GetText(), refusal.Start, refusal.End), Message: boundMessage(refusal.Message),
		})
	}
	return reply
}

// diagnosticCode is the code a refusal of code is reported with. An
// undeclared statement is UNDECLARED where the host negotiated
// provider.SQLUndeclaredFeature, so the host reads it as its own reader's
// refusal and says why a load beside a source in another format cannot
// change what that source declares, and INVALID otherwise.
func diagnosticCode(code RefusalCode, undeclared bool) protocol.SQLDiagnostic_Code {
	switch {
	case code == Syntax:
		return protocol.SQLDiagnostic_SYNTAX
	case code == Unsupported:
		return protocol.SQLDiagnostic_UNSUPPORTED
	case code == Undeclared && undeclared:
		return protocol.SQLDiagnostic_UNDECLARED
	default:
		return protocol.SQLDiagnostic_INVALID
	}
}

// boundMessage writes a refusal as one line of at most
// provider.MaxSQLDiagnosticMessage bytes: control characters as spaces, text
// that is not UTF-8 replaced, and a message that is too long cut at a
// character and marked as cut.
func boundMessage(message string) string {
	message = strings.ToValidUTF8(message, "�")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, message)
	message = strings.TrimSpace(message)
	if len(message) <= provider.MaxSQLDiagnosticMessage {
		return message
	}
	cut := provider.MaxSQLDiagnosticMessage - len("...")
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return strings.TrimSpace(message[:cut]) + "..."
}

// sourceSpan is the span of the bytes from start to end of text, with the
// line and the byte column of each end, both counted from 1.
func sourceSpan(text string, start, end int) *protocol.SourceSpan {
	start = min(max(start, 0), len(text))
	end = min(max(end, start), len(text))
	return &protocol.SourceSpan{Start: sourcePosition(text, start), End: sourcePosition(text, end)}
}

func sourcePosition(text string, offset int) *protocol.SourcePosition {
	before := text[:offset]
	line := strings.Count(before, "\n") + 1
	column := offset - (strings.LastIndexByte(before, '\n') + 1) + 1
	// #nosec G115 -- the offset is within a request document, which runtime.MaxMessageBytes bounds to 4 MiB
	return &protocol.SourcePosition{ByteOffset: uint64(offset), Line: uint32(line), Column: uint32(column)}
}

// wire writes a reading as the reply's schema, and returns the refusals of
// everything in it a SQL schema reading has no home for: a table's settings
// and changefeeds among them where the host does not take the facets of a
// reading (see [takesFacets]), and a user's password where the host does not
// take the password a document gives a role (source.sql-role-passwords,
// contract 25.33). A host that takes it gets the password as the document
// writes it: a literal, a YDB password hash after HASH, or the reference a
// migration file Ptah generated names it by. The host binds it at execution
// as it binds a password a YAML or HCL source declares, and the provider's
// execution writes HASH for a hash, as the host's embedded engine wrote it.
func wire(request *protocol.ReadSQLSchemaRequest, reading *Reading) (*protocol.SQLSchema, []*Refusal) {
	w := &wiring{request: request, reply: &protocol.SQLSchema{Complete: true}, facets: takesFacets(request), indexFacets: takesIndexFacets(request),
		nativeIndexes: takesNativeIndexes(request)}
	w.tables(reading)
	for _, view := range reading.Views {
		w.trace(protocol.SQLObjectTrace_VIEWS, len(w.reply.Views), view.document, view.span)
		w.reply.Views = append(w.reply.Views, &protocol.SQLView{Schema: view.schema, Name: view.leaf, Body: view.Body, Comment: view.Comment})
	}
	passwords := slices.Contains(request.GetTargetFeatures(), provider.SQLRolePasswordsFeature)
	for _, role := range reading.Roles {
		if role.passwordAt != nil && !passwords {
			w.gaps = append(w.gaps, gap(role.passwordDocument, *role.passwordAt, fmt.Sprintf("user %s declares a password, which a SQL schema reading has no home for: "+
				"the host compares no secret a schema file holds", quoteName(role.Name))))
		}
		w.trace(protocol.SQLObjectTrace_ROLES, len(w.reply.Roles), role.document, role.span)
		declared := &protocol.SQLRole{Name: role.Name, Login: role.Login, Inherit: role.Inherit, Group: role.Group, MemberOf: slices.Clone(role.MemberOf)}
		if role.passwordAt != nil && passwords {
			declared.PasswordDeclared, declared.Password = true, role.Password
		}
		w.reply.Roles = append(w.reply.Roles, declared)
	}
	w.grants(reading)
	var feeds []Object
	for _, object := range SortedObjects(reading) {
		if object.Kind == KindChangefeed {
			feeds = append(feeds, object)
			continue
		}
		envelope, refusal := nativeObject(object)
		if refusal != nil {
			w.gaps = append(w.gaps, refusal)
			continue
		}
		w.trace(protocol.SQLObjectTrace_NATIVE_OBJECTS, len(w.reply.NativeObjects), object.document, object.span)
		w.reply.NativeObjects = append(w.reply.NativeObjects, envelope)
	}
	w.changefeeds(feeds)
	for i, file := range reading.Files {
		w.reply.Declarations = append(w.reply.Declarations, documentDeclarations(request.GetDocuments()[i].GetId(), file))
	}
	return w.reply, w.gaps
}

// wiring is the reply wire writes: the request it answers, the refusals it
// collects, whether the host takes the facets of a reading, whether it takes
// an index's own (see [takesIndexFacets]), and whether it takes a table's
// vector, full-text and local indexes (see [takesNativeIndexes]).
type wiring struct {
	request       *protocol.ReadSQLSchemaRequest
	reply         *protocol.SQLSchema
	gaps          []*Refusal
	facets        bool
	indexFacets   bool
	nativeIndexes bool
}

// trace traces the object at index of a reply collection to the text of its
// statement in a document of the request.
func (w *wiring) trace(collection protocol.SQLObjectTrace_Collection, index, document int, text span) {
	source := w.request.GetDocuments()[document]
	// #nosec G115 -- index counts the objects read from the request's documents, which runtime.MaxMessageBytes bounds to 4 MiB
	w.reply.Traces = append(w.reply.Traces, &protocol.SQLObjectTrace{Collection: collection, Index: uint32(index),
		Source: &protocol.SourceTrace{DocumentId: source.GetId(), Span: sourceSpan(source.GetText(), text.start, text.end)}})
}

// tables writes the tables of the reading, and the facets of their settings
// and of their indexes' settings for a host that takes them. Settings a host
// without the facets has no home for are refused.
func (w *wiring) tables(reading *Reading) {
	for _, table := range reading.Tables {
		if settings := tableSettings(table.Facets); len(settings) != 0 && !w.facets {
			w.gaps = append(w.gaps, gap(table.document, table.span, fmt.Sprintf("table %s declares %s, which a SQL schema reading has no home for: "+
				"the host does not take the facets of a table in a SQL schema reading (%s)", quoteName(ydbPath(table.Schema, table.Name)), strings.Join(settings, ", "),
				provider.SQLFacetsFeature)))
		}
		declared, indexes, refused := w.declaredTable(reading, table)
		w.gaps = append(w.gaps, refused...)
		w.trace(protocol.SQLObjectTrace_TABLES, len(w.reply.Tables), table.document, table.span)
		w.reply.Tables = append(w.reply.Tables, declared)
		owned, refused := w.ownedIndexes(reading, table)
		w.gaps = append(w.gaps, refused...)
		if !w.facets {
			continue
		}
		envelopes, err := tableFacets(table, owned.locals, w.nativeIndexes)
		if err != nil {
			w.gaps = append(w.gaps, &Refusal{Document: table.document, Start: table.span.start, End: table.span.end, Code: Invalid,
				Message: fmt.Sprintf("table %s: %v", quoteName(ydbPath(table.Schema, table.Name)), err)})
			continue
		}
		for _, envelope := range envelopes {
			w.trace(protocol.SQLObjectTrace_FACETS, len(w.reply.Facets), table.document, table.span)
			w.reply.Facets = append(w.reply.Facets, envelope)
		}
		for _, index := range indexes {
			w.trace(protocol.SQLObjectTrace_FACETS, len(w.reply.Facets), index.document, index.text)
			w.reply.Facets = append(w.reply.Facets, index.envelope)
		}
		if owned.set != nil {
			w.trace(protocol.SQLObjectTrace_FACETS, len(w.reply.Facets), owned.set.document, owned.set.text)
			w.reply.Facets = append(w.reply.Facets, owned.set.envelope)
		}
	}
}

// ownedIndexSet is what a table's vector, full-text and local indexes give
// the reply: the facet of its vector and full-text indexes, nil where it
// declares none, and its local indexes, which its storage facet holds.
type ownedIndexSet struct {
	set    *indexFacet
	locals []*native.LocalIndexObservation
}

// ownedIndexes reads the vector, full-text and local indexes of table as a
// YAML or Go source's declarations of the same indexes state them, for a host
// that takes them (see [takesNativeIndexes]), and refuses each one for a host
// that does not. A vector or full-text index of a column table and a local
// index of a row table are refused with the words a YAML or Go source of
// them is refused with, and so is a comment on one or a vector index's
// partitioning, which no model of them holds.
func (w *wiring) ownedIndexes(reading *Reading, table Table) (ownedIndexSet, []*Refusal) {
	var result ownedIndexSet
	var gaps []*Refusal
	var set native.IndexSetIntent
	var first *Index
	label := quoteName(ydbPath(table.Schema, table.Name))
	for _, index := range reading.Indexes {
		kind, _ := ydbdecl.IndexKindOf(index.Type)
		if index.TableName != table.Qualified() || !ownedKind(kind) {
			continue
		}
		refuse := func(message string) {
			gaps = append(gaps, &Refusal{Document: index.document, Start: index.span.start, End: index.span.end, Code: Invalid,
				Message: fmt.Sprintf("table %s: index %s: %s", label, quoteName(index.Name), message)})
		}
		switch {
		case !w.nativeIndexes:
			gaps = append(gaps, gap(index.document, index.span, fmt.Sprintf("index %s of table %s is a %s index, which a SQL schema reading has no home for: "+
				"the host does not take a table's vector, full-text and local indexes in a SQL schema reading (%s and %s)", quoteName(index.Name), label, kind,
				provider.SQLFacetsFeature, provider.IndexKindsFeature)))
			continue
		case index.Comment != "":
			refuse("a comment on a vector, full-text or local index has no place this provider reads back")
			continue
		case kind.IsLocal() && table.Facets.ColumnStore == nil:
			refuse("a local index belongs to a column table, and the table is declared as a row table")
			continue
		case !kind.IsLocal() && table.Facets.ColumnStore != nil:
			refuse(fmt.Sprintf("a column table takes local indexes only; a %s index belongs to a row table", kind))
			continue
		case index.Partitioning != nil && kind == ydbdecl.IndexVector:
			refuse("a vector index states no partitioning this provider keeps")
			continue
		case kind.IsLocal():
			local, err := tablesource.LocalIndex(index.Name, kind, index.Fields, index.IncludeColumns, index.Options)
			if err != nil {
				refuse(err.Error())
				continue
			}
			result.locals = append(result.locals, local)
			continue
		}
		spec, err := tablesource.NativeIndex(index.Name, kind, index.Fields, index.IncludeColumns, index.Options)
		if err != nil {
			refuse(err.Error())
			continue
		}
		if first == nil {
			first = &index
		}
		set.Indexes = append(set.Indexes, &native.IndexDeclaration{Specification: spec})
	}
	if first == nil || len(gaps) != 0 {
		return result, gaps
	}
	envelope, err := schema.CaptureIndexSetIntent(&protocol.Reference{Engine: "ydb", Kind: "table", Schema: table.Schema, Name: table.Name}, &set)
	if err != nil {
		return result, []*Refusal{{Document: first.document, Start: first.span.start, End: first.span.end, Code: Invalid,
			Message: fmt.Sprintf("table %s: the vector and full-text indexes: %v", label, err)}}
	}
	result.set = &indexFacet{envelope: envelope, document: first.document, text: first.span}
	return result, nil
}

// ownedKind reports whether an index of kind is one a YAML or Go source
// declares in place of a common index: a vector, full-text or local one.
func ownedKind(kind ydbdecl.IndexKind) bool {
	return kind == ydbdecl.IndexVector || kind.IsFullText() || kind.IsLocal()
}

// grants writes the grants of the reading, then its revocations.
func (w *wiring) grants(reading *Reading) {
	for _, revoked := range []bool{false, true} {
		grants := reading.Grants
		if revoked {
			grants = reading.RevokedGrants
		}
		for _, grant := range grants {
			w.trace(protocol.SQLObjectTrace_GRANTS, len(w.reply.Grants), grant.document, grant.span)
			w.reply.Grants = append(w.reply.Grants, &protocol.SQLGrant{Role: grant.Role, Privileges: slices.Clone(grant.Privileges), Revoked: revoked,
				Target: &protocol.Reference{Kind: grant.target.kind, Schema: grant.target.schema, Name: grant.target.name}})
		}
	}
}

// changefeeds writes the changefeeds of each table as one facet of the table
// for a host that takes the facets of a reading, and refuses each of them for
// a host that does not.
func (w *wiring) changefeeds(feeds []Object) {
	for _, table := range changefeedTables(feeds) {
		if !w.facets {
			for _, feed := range table {
				w.gaps = append(w.gaps, gap(feed.document, feed.span, fmt.Sprintf("changefeed %s of table %s has no home in a SQL schema reading: "+
					"the host does not take the facets of a table in a SQL schema reading (%s)", quoteName(feed.Name), quoteName(ydbPath(feed.Schema, feed.Parent)),
					provider.SQLFacetsFeature)))
			}
			continue
		}
		envelope, refusal := changefeedFacet(table)
		if refusal != nil {
			w.gaps = append(w.gaps, refusal)
			continue
		}
		w.trace(protocol.SQLObjectTrace_FACETS, len(w.reply.Facets), table[0].document, table[0].span)
		w.reply.Facets = append(w.reply.Facets, envelope)
	}
}

// indexFacet is the facet of one index's settings, with the text of the
// index's declaration a trace points at.
type indexFacet struct {
	envelope *protocol.Envelope
	document int
	text     span
}

// declaredTable writes a table of the reading as the reply declares it, with
// its key parts, columns and global indexes, and returns the facets of those
// indexes' settings where the host takes them (see
// [wiring.indexSettingsFacet]) and the refusals of the index settings a SQL
// schema reading has no home for. Its vector, full-text and local indexes are
// no common index (see [wiring.ownedIndexes]).
func (w *wiring) declaredTable(reading *Reading, table Table) (*protocol.DeclaredTable, []indexFacet, []*Refusal) {
	var gaps []*Refusal
	var facets []indexFacet
	declared := &protocol.DeclaredTable{Name: table.Name, Schema: table.Schema, Comment: table.Comment, PrimaryKey: slices.Clone(table.PrimaryKey)}
	for _, part := range table.PrimaryKeyParts {
		declared.PrimaryKeyParts = append(declared.PrimaryKeyParts, &protocol.DeclaredKeyPart{Column: part.Name, Prefix: part.Prefix, Descending: part.Desc})
	}
	for _, field := range reading.Fields {
		if field.Table != table.Qualified() {
			continue
		}
		declared.Columns = append(declared.Columns, &protocol.DeclaredColumn{Name: field.Name, Type: field.Type, Nullable: field.Nullable, Primary: field.Primary,
			IdentityStart: field.IdentityStart, IdentityIncrement: field.IdentityIncrement, DefaultValue: field.Default, DefaultSet: field.DefaultSet, Comment: field.Comment})
	}
	for _, index := range reading.Indexes {
		if kind, _ := ydbdecl.IndexKindOf(index.Type); index.TableName != table.Qualified() || ownedKind(kind) {
			continue
		}
		envelope, refusal := w.indexSettingsFacet(table, index)
		switch {
		case refusal != nil:
			gaps = append(gaps, refusal)
		case envelope != nil:
			facets = append(facets, indexFacet{envelope: envelope, document: index.document, text: index.span})
		}
		declared.Indexes = append(declared.Indexes, &protocol.DeclaredIndex{Name: index.Name, Fields: slices.Clone(index.Fields), Unique: index.Unique, Comment: index.Comment,
			Type: index.Type, IncludeColumns: slices.Clone(index.IncludeColumns), StorageParameters: index.StorageParams, TableName: index.TableName})
	}
	return declared, facets, gaps
}

// indexSettingsFacet is the facet of the partitioning and the read replicas a
// global sync, async or unique index declares, owned by the index as a YAML or
// Go source's index entry owns it, or nil where it declares none. The facet
// needs a host that takes it (see [takesIndexFacets]); without one the
// settings are refused.
func (w *wiring) indexSettingsFacet(table Table, index Index) (*protocol.Envelope, *Refusal) {
	settings := indexSettings(index)
	if len(settings) == 0 {
		return nil, nil
	}
	named := fmt.Sprintf("index %s of table %s", quoteName(index.Name), quoteName(ydbPath(table.Schema, table.Name)))
	declares := fmt.Sprintf("%s declares %s, which a SQL schema reading has no home for: ", named, strings.Join(settings, ", "))
	if !w.indexFacets {
		return nil, gap(index.document, index.span, declares+fmt.Sprintf("the host does not take the facets of an index in a SQL schema reading (%s and %s)",
			provider.SQLFacetsFeature, provider.ObservedIndexFacetsFeature))
	}
	invalid := func(err error) *Refusal {
		return &Refusal{Document: index.document, Start: index.span.start, End: index.span.end, Code: Invalid, Message: fmt.Sprintf("%s: %v", named, err)}
	}
	intent, err := indexSettingsIntent(*index.Partitioning)
	if err != nil {
		return nil, invalid(err)
	}
	envelope, err := schema.CaptureIndexSettingsIntent(&protocol.Reference{Engine: "ydb", Kind: "index", Schema: table.Schema, Path: []string{table.Name}, Name: index.Name}, intent)
	if err != nil {
		return nil, invalid(err)
	}
	return envelope, nil
}

// documentDeclarations writes what one document of the load declares, in its
// order: the objects, the guards, and the table statements.
func documentDeclarations(documentID string, file FileDeclarations) *protocol.SQLDocumentDeclarations {
	declared := &protocol.SQLDocumentDeclarations{DocumentId: documentID}
	for _, object := range file.Objects {
		declared.Objects = append(declared.Objects, &protocol.SQLDeclaration{Kind: object.Kind, Name: object.Name})
	}
	for _, object := range file.Guards {
		declared.Guards = append(declared.Guards, &protocol.SQLDeclaration{Kind: object.Kind, Name: object.Name})
	}
	for _, statement := range file.Tables {
		declared.Tables = append(declared.Tables, &protocol.SQLTableStatement{Name: statement.Name, Drop: statement.Drop, Guarded: statement.Guarded})
	}
	return declared
}

func gap(document int, text span, message string) *Refusal {
	return &Refusal{Document: document, Start: text.start, End: text.end, Code: Unsupported, Message: message}
}

// ydbPath writes a directory and a name as one YDB path.
func ydbPath(directory, name string) string {
	if directory == "" {
		return name
	}
	return directory + "/" + name
}

// tableSettings names the YDB settings a table declares.
func tableSettings(facets ydbdecl.TableFacets) []string {
	var settings []string
	if facets.Partitioning != nil {
		settings = append(settings, "partitioning settings")
	}
	if len(facets.Families) != 0 {
		settings = append(settings, "column families")
	}
	if facets.TTL != nil {
		settings = append(settings, "a TTL")
	}
	if facets.ColumnStore != nil {
		settings = append(settings, "column storage")
	}
	return settings
}

// indexSettings names the YDB settings a global index declares beyond its
// options.
func indexSettings(index Index) []string {
	var settings []string
	if index.Partitioning != nil {
		settings = append(settings, "partitioning settings")
	}
	return settings
}

// nativeObject is the reply's envelope for a YDB object, or the refusal of
// one the reading cannot write. It reads the object by its specification,
// whose type the reading sets from the object's kind. A changefeed is a facet
// of its table and never reaches it.
func nativeObject(object Object) (*protocol.Envelope, *Refusal) {
	switch spec := object.Spec.(type) {
	case *secrets.Spec:
		value, err := schema.CaptureSecretIntent(&protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Schema: object.Schema, Name: object.Name}, &native.SecretIntent{ValueEnv: spec.ValueEnv})
		if err != nil {
			return nil, &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid, Message: "invalid secret declaration"}
		}
		return value, nil
	case *ydbdecl.CoordinationNode:
		owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: object.Schema, Name: object.Name}
		if err := schema.ValidateManagedCoordinationOwner(owner); err != nil {
			return nil, &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid,
				Message: fmt.Sprintf("coordination node %s: %v", quoteName(ydbPath(object.Schema, object.Name)), err)}
		}
		envelope, err := schema.CaptureCoordinationIntent(owner, coordinationIntent(*spec))
		if err != nil {
			return nil, &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid,
				Message: fmt.Sprintf("coordination node %s: %v", quoteName(ydbPath(object.Schema, object.Name)), err)}
		}
		return envelope, nil
	case *replication.ReplicationSpec:
		envelope, err := schema.CaptureReplicationIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ReplicationProperties, Schema: object.Schema, Name: object.Name}, *spec)
		if err != nil {
			return nil, &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid, Message: replicationRefusal}
		}
		return envelope, nil
	case *replication.TransferSpec:
		envelope, err := schema.CaptureTransferIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TransferProperties, Schema: object.Schema, Name: object.Name}, *spec)
		if err != nil {
			return nil, &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid, Message: replicationRefusal}
		}
		return envelope, nil
	case *createStreamingQuery:
		envelope, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: object.Schema, Name: object.Name}, spec.spec, spec.allowStateReset)
		if err != nil {
			return nil, &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid, Message: err.Error()}
		}
		return envelope, nil
	case *ydbdecl.Topic:
		invalid := func(err error) *Refusal {
			return &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid,
				Message: fmt.Sprintf("topic %s: %v", quoteName(ydbPath(object.Schema, object.Name)), err)}
		}
		intent, err := topics.Intent(*spec)
		if err != nil {
			return nil, invalid(err)
		}
		envelope, err := schema.CaptureTopicIntent(&protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: object.Schema, Name: object.Name}, intent)
		if err != nil {
			return nil, invalid(err)
		}
		return envelope, nil
	case *workload.PoolSpec:
		return workloadObject(object, schema.ResourcePoolProperties, schema.PoolSettings(*spec), "resource pool")
	case *workload.ClassifierSpec:
		return workloadObject(object, schema.ResourcePoolClassifierProperties, schema.ClassifierSettings(*spec), "resource pool classifier")
	default:
		return externalObject(object)
	}
}

// workloadObject is the reply's envelope for a resource pool or a
// classifier, an object of model kind with settings, or the refusal that
// names it as noun.
func workloadObject(object Object, kind string, settings proto.Message, noun string) (*protocol.Envelope, *Refusal) {
	envelope, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: kind, Name: object.Name}, settings)
	if err != nil {
		return nil, &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid,
			Message: fmt.Sprintf("%s %s: %v", noun, quoteName(object.Name), err)}
	}
	return envelope, nil
}

// changefeedTables groups the changefeeds of a reading by their table, in the
// order of the reading's objects, which keeps a table's changefeeds by name.
func changefeedTables(feeds []Object) [][]Object {
	var tables [][]Object
	for _, feed := range feeds {
		last := len(tables) - 1
		if last >= 0 && tables[last][0].Schema == feed.Schema && tables[last][0].Parent == feed.Parent {
			tables[last] = append(tables[last], feed)
			continue
		}
		tables = append(tables, []Object{feed})
	}
	return tables
}

// changefeedFacet is the reply's facet of one table's changefeeds, with the
// consumers each one's topic gets: the value a YAML source of the table
// declares for the same changefeeds, owned by the table.
func changefeedFacet(feeds []Object) (*protocol.Envelope, *Refusal) {
	first := feeds[0]
	set := &native.ChangefeedSetIntent{}
	for _, feed := range feeds {
		intent, err := changefeeds.Intent(*feed.Spec.(*ydbdecl.Changefeed))
		if err != nil {
			return nil, &Refusal{Document: feed.document, Start: feed.span.start, End: feed.span.end, Code: Invalid,
				Message: fmt.Sprintf("table %s: %v", quoteName(ydbPath(feed.Schema, feed.Parent)), err)}
		}
		set.Changefeeds = append(set.Changefeeds, intent)
	}
	envelope, err := schema.CaptureChangefeedIntent(&protocol.Reference{Engine: "ydb", Kind: "table", Schema: first.Schema, Name: first.Parent}, set)
	if err != nil {
		return nil, &Refusal{Document: first.document, Start: first.span.start, End: first.span.end, Code: Invalid,
			Message: fmt.Sprintf("the changefeeds of table %s: %v", quoteName(ydbPath(first.Schema, first.Parent)), err)}
	}
	return envelope, nil
}

// externalObject is the reply's envelope for an external data source or an
// external table. CREATE OR REPLACE says nothing the declaration keeps:
// whether a plan replaces the object is the plan's decision.
func externalObject(object Object) (*protocol.Envelope, *Refusal) {
	kind, noun := schema.ExternalTableProperties, "external table"
	if object.Kind == KindExternalSource {
		kind, noun = schema.ExternalSourceProperties, "external data source"
	}
	invalid := func(err error) *Refusal {
		return &Refusal{Document: object.document, Start: object.span.start, End: object.span.end, Code: Invalid,
			Message: fmt.Sprintf("%s %s: %v", noun, quoteName(ydbPath(object.Schema, object.Name)), err)}
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: kind, Schema: object.Schema, Name: object.Name}
	if err := schema.ValidateNativeOwner(owner); err != nil {
		return nil, invalid(err)
	}
	var envelope *protocol.Envelope
	var err error
	if object.Kind == KindExternalSource {
		intent, intentErr := externals.SourceIntent(*object.Spec.(*ydbdecl.ExternalDataSource))
		if intentErr != nil {
			return nil, invalid(intentErr)
		}
		envelope, err = schema.CaptureExternalIntent(owner, intent)
	} else {
		intent, intentErr := externals.TableIntent(*object.Spec.(*ydbdecl.ExternalTable))
		if intentErr != nil {
			return nil, invalid(intentErr)
		}
		envelope, err = schema.CaptureExternalIntent(owner, intent)
	}
	if err != nil {
		return nil, invalid(err)
	}
	return envelope, nil
}

// coordinationIntent is a node's declared settings as the provider's model
// keeps them: a setting the declaration leaves out stays out.
func coordinationIntent(spec ydbdecl.CoordinationNode) schema.CoordinationIntent {
	var intent schema.CoordinationIntent
	if spec.SelfCheckPeriodMillis != 0 {
		intent.SelfCheckPeriodMillis = new(spec.SelfCheckPeriodMillis)
	}
	if spec.SessionGracePeriodMillis != 0 {
		intent.SessionGracePeriodMillis = new(spec.SessionGracePeriodMillis)
	}
	for _, mode := range []struct {
		value  string
		target **string
	}{
		{spec.ReadConsistencyMode, &intent.ReadConsistencyMode},
		{spec.AttachConsistencyMode, &intent.AttachConsistencyMode},
		{spec.RateLimiterCountersMode, &intent.RateLimiterCountersMode},
	} {
		if mode.value != "" {
			*mode.target = new(mode.value)
		}
	}
	return intent
}

// limitedKinds are the header kinds a load's or a source's claim can be
// limited by, with the model each limits, how a limit names an object of it,
// and the reason a limit of the whole kind gives. A YQL load claims no async
// replication or transfer, so their limits change only a source's claim.
var limitedKinds = map[string]struct {
	model  string
	path   func(string) (string, string)
	reason string
}{
	"secret":                   {schema.SecretProperties, topicLimit, "the source leaves secrets unmanaged"},
	"coordination_node":        {schema.CoordinationProperties, coordinationLimit, "the source leaves coordination nodes unmanaged"},
	"streaming_query":          {schema.StreamingQueryProperties, coordinationLimit, "the source leaves streaming queries unmanaged"},
	"topic":                    {schema.TopicProperties, topicLimit, "the source leaves topics unmanaged"},
	"external_data_source":     {schema.ExternalSourceProperties, externalLimit, "the source leaves external data sources unmanaged"},
	"external_table":           {schema.ExternalTableProperties, externalLimit, "the source leaves external tables unmanaged"},
	"resource_pool":            {schema.ResourcePoolProperties, workloadLimit, "the source leaves resource pools unmanaged"},
	"resource_pool_classifier": {schema.ResourcePoolClassifierProperties, workloadLimit, "the source leaves resource pool classifiers unmanaged"},
	"replication":              {schema.ReplicationProperties, topicLimit, "the source leaves async replications unmanaged"},
	"transfer":                 {schema.TransferProperties, topicLimit, "the source leaves transfers unmanaged"},
}

// claim is the coverage a load claims: every claimed model complete, less
// the coordination nodes, topics and external objects the documents' headers
// leave unmanaged.
// A limit whose name no object of its kind can take is refused as the host
// refuses it.
func claim(limits []*protocol.SchemaCoverageLimit, models []*protocol.ValueModel) (*protocol.ModelCoverage, *Refusal) {
	coverage := &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED}
	whole := make(map[string]*protocol.ModelKnowledge)
	var subjects []*protocol.ModelSubjectCoverage
	seen := make(map[string]bool)
	for _, limit := range limits {
		if refusal := checkLimit(limit); refusal != nil {
			return nil, refusal
		}
		kind, limited := limitedKinds[limit.GetKind()]
		if !limited {
			continue
		}
		if limit.GetName() == "" {
			whole[kind.model] = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: kind.reason}
			continue
		}
		schemaName, name := kind.path(limit.GetName())
		key := kind.model + "\x00" + schemaName + "\x00" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		owner := &protocol.Reference{Engine: "ydb", Kind: kind.model, Schema: schemaName, Name: name}
		subjects = append(subjects, &protocol.ModelSubjectCoverage{Kind: kind.model, Subject: owner, NormalizedSubject: proto.CloneOf(owner),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: unmanagedObject}})
	}
	for _, model := range models {
		knowledge := &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}
		if limited, found := whole[model.GetKind()]; found {
			knowledge = proto.CloneOf(limited)
		}
		coverage.Kinds = append(coverage.Kinds, &protocol.ModelKindCoverage{Model: model, Knowledge: knowledge})
	}
	coverage.Subjects = subjects
	return coverage, nil
}

// CheckLimits refuses, with the host's words, a `ptah:not-described` limit
// whose name no object of its kind can take, as a YQL document's header is
// read. A source of another format names its limits alike.
func CheckLimits(limits []*protocol.SchemaCoverageLimit) error {
	for _, limit := range limits {
		if refusal := checkLimit(limit); refusal != nil {
			return errors.New(refusal.Message)
		}
	}
	return nil
}

// workloadLimit is the name of the pool or the classifier a header names,
// which belongs to the database and has no directory.
func workloadLimit(written string) (directory, name string) { return "", written }

// topicLimit is the directory and the name of the topic a header names: a
// path relative to the database root, a dot being part of a name.
func topicLimit(written string) (directory, name string) {
	path, err := ydbdecl.ParseTopicPath(written)
	if err != nil {
		return "", written
	}
	return path.Schema, path.Name
}

// externalLimit is the directory and the name of the external object a
// header names: a path relative to the database root, a dot being part of a
// name.
func externalLimit(written string) (directory, name string) {
	path, err := ydbdecl.ParseExternalPath(written)
	if err != nil {
		return "", written
	}
	return path.Schema, path.Name
}

// coordinationLimit is the directory and name of a coordination node or
// streaming query named by a path or a dot-qualified directory.
func coordinationLimit(written string) (directory, name string) {
	physical := written
	if !strings.Contains(written, "/") {
		if ref, ok := parseTableRef(written); ok {
			physical = ref.name
			if ref.schema != "" {
				physical = strings.TrimRight(ref.schema, "/") + "/" + ref.name
			}
		}
	}
	if slash := strings.LastIndex(physical, "/"); slash >= 0 {
		return physical[:slash], physical[slash+1:]
	}
	return "", physical
}

// limitDocument finds the header line of the load that wrote limit: the
// first `ptah:not-described` line naming its kind and its name. A limit
// arrives without its document, and the load's first document stands in when
// no line is found.
func limitDocument(request *protocol.ReadSQLSchemaRequest, limit *protocol.SchemaCoverageLimit) (document, start, end int) {
	for index, source := range request.GetDocuments() {
		text := source.GetText()
		offset := 0
		for line := range strings.SplitAfterSeq(text, "\n") {
			if strings.Contains(line, "ptah:not-described") && strings.Contains(strings.ToLower(line), strings.ToLower(limit.GetKind())) &&
				strings.Contains(line, strconv.Quote(limit.GetName())) {
				return index, offset, offset + len(strings.TrimRight(line, "\r\n"))
			}
			offset += len(line)
		}
	}
	return 0, 0, 0
}

// checkLimit refuses a header limit whose name no object of its kind can
// take, with the host's words, the host's owner named by this provider's
// identity.
func checkLimit(limit *protocol.SchemaCoverageLimit) *Refusal {
	name := limit.GetName()
	if name == "" {
		return nil
	}
	refuse := func(err error) *Refusal {
		return &Refusal{Code: Invalid, Message: "coverage of " + schema.ProviderID + ": " + err.Error(), limit: limit}
	}
	switch limit.GetKind() {
	case "topic":
		if _, err := ydbdecl.ParseTopicPath(name); err != nil {
			return refuse(fmt.Errorf("invalid feature value: topic limit: %w", err))
		}
	case "external_data_source", "external_table":
		if _, err := ydbdecl.ParseExternalPath(name); err != nil {
			label := strings.ReplaceAll(limit.GetKind(), "_", " ")
			return refuse(fmt.Errorf("invalid feature value: %s limit: %w", label, err))
		}
	case "coordination_node":
		schemaName, leaf := coordinationLimit(name)
		if err := ydbdecl.CoordinationIdentity(schemaName, leaf); err != nil {
			return refuse(err)
		}
	}
	return nil
}
