package yqlschema

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// HeaderKinds are the kinds a `ptah:not-described` header of a YQL document
// may name, as the host's YDB owner reads them. The host hands this provider
// the header's entries of these kinds as limits on its claim.
func HeaderKinds() []string {
	return []string{"coordination_node", "streaming_query", "resource_pool", "resource_pool_classifier", "secret", "topic",
		"external_data_source", "external_table", "replication", "transfer"}
}

// ClaimedModels are the desired models whose coverage a YQL document claims:
// a row table's settings, a column's, a column table's storage and a
// coordination node. Their source formats include SQL.
func ClaimedModels() []*protocol.ValueModel {
	return []*protocol.ValueModel{schema.TableDesiredModel(), schema.ColumnDesiredModel(), schema.NativeDesiredModel(schema.ColumnStorageProperties), schema.CoordinationDesiredModel()}
}

// Reply reads the documents of a ReadSQLSchema request and answers with the
// reading: the tables, views, principals, grants and coordination nodes the
// load declares, what each document declares, the coverage the load claims,
// and a trace for every object. A refused load, and a reading that declares
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
	coverage, refusal := claim(request.GetLimits())
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
	for _, refusal := range refusals {
		document := request.GetDocuments()[refusal.Document]
		reply.Diagnostics = append(reply.Diagnostics, &protocol.SQLDiagnostic{
			Code: diagnosticCode(refusal.Code), Severity: protocol.SQLDiagnostic_ERROR, DocumentId: document.GetId(),
			Span: sourceSpan(document.GetText(), refusal.Start, refusal.End), Message: boundMessage(refusal.Message),
		})
	}
	return reply
}

func diagnosticCode(code RefusalCode) protocol.SQLDiagnostic_Code {
	switch code {
	case Syntax:
		return protocol.SQLDiagnostic_SYNTAX
	case Unsupported:
		return protocol.SQLDiagnostic_UNSUPPORTED
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
	return &protocol.SourcePosition{ByteOffset: uint64(offset), Line: uint32(line), Column: uint32(column)}
}

// wire writes a reading as the reply's schema, and returns the refusals of
// everything in it a SQL schema reading has no home for.
func wire(request *protocol.ReadSQLSchemaRequest, reading *Reading) (*protocol.SQLSchema, []*Refusal) {
	reply := &protocol.SQLSchema{Complete: true}
	var gaps []*Refusal
	documents := request.GetDocuments()
	trace := func(collection protocol.SQLObjectTrace_Collection, index int, document int, text span) {
		source := documents[document]
		reply.Traces = append(reply.Traces, &protocol.SQLObjectTrace{Collection: collection, Index: uint32(index),
			Source: &protocol.SourceTrace{DocumentId: source.GetId(), Span: sourceSpan(source.GetText(), text.start, text.end)}})
	}
	for _, table := range reading.Tables {
		if settings := tableSettings(table.Facets); len(settings) != 0 {
			gaps = append(gaps, gap(table.document, table.span, fmt.Sprintf("table %s declares %s, which a SQL schema reading has no home for: "+
				"the host refuses the settings of a table in a SQL schema reading", quoteName(ydbPath(table.Schema, table.Name)), strings.Join(settings, ", "))))
		}
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
			if index.TableName != table.Qualified() {
				continue
			}
			if settings := indexSettings(index); len(settings) != 0 {
				gaps = append(gaps, gap(index.document, index.span, fmt.Sprintf("index %s of table %s declares %s, which a SQL schema reading has no home for",
					quoteName(index.Name), quoteName(ydbPath(table.Schema, table.Name)), strings.Join(settings, ", "))))
			}
			declared.Indexes = append(declared.Indexes, &protocol.DeclaredIndex{Name: index.Name, Fields: slices.Clone(index.Fields), Unique: index.Unique, Comment: index.Comment,
				Type: index.Type, IncludeColumns: slices.Clone(index.IncludeColumns), StorageParameters: index.StorageParams, TableName: index.TableName})
		}
		trace(protocol.SQLObjectTrace_TABLES, len(reply.Tables), table.document, table.span)
		reply.Tables = append(reply.Tables, declared)
	}
	for _, view := range reading.Views {
		trace(protocol.SQLObjectTrace_VIEWS, len(reply.Views), view.document, view.span)
		reply.Views = append(reply.Views, &protocol.SQLView{Schema: view.schema, Name: view.leaf, Body: view.Body, Comment: view.Comment})
	}
	for _, role := range reading.Roles {
		if role.passwordAt != nil {
			gaps = append(gaps, gap(role.passwordDocument, *role.passwordAt, fmt.Sprintf("user %s declares a password, which a SQL schema reading has no home for: "+
				"the host compares no secret a schema file holds", quoteName(role.Name))))
		}
		trace(protocol.SQLObjectTrace_ROLES, len(reply.Roles), role.document, role.span)
		reply.Roles = append(reply.Roles, &protocol.SQLRole{Name: role.Name, Login: role.Login, Inherit: role.Inherit, Group: role.Group, MemberOf: slices.Clone(role.MemberOf)})
	}
	for _, revoked := range []bool{false, true} {
		grants := reading.Grants
		if revoked {
			grants = reading.RevokedGrants
		}
		for _, grant := range grants {
			trace(protocol.SQLObjectTrace_GRANTS, len(reply.Grants), grant.document, grant.span)
			reply.Grants = append(reply.Grants, &protocol.SQLGrant{Role: grant.Role, Privileges: slices.Clone(grant.Privileges), Revoked: revoked,
				Target: &protocol.Reference{Kind: grant.target.kind, Schema: grant.target.schema, Name: grant.target.name}})
		}
	}
	for _, object := range SortedObjects(reading) {
		envelope, refusal := nativeObject(object)
		if refusal != nil {
			gaps = append(gaps, refusal)
			continue
		}
		trace(protocol.SQLObjectTrace_NATIVE_OBJECTS, len(reply.NativeObjects), object.document, object.span)
		reply.NativeObjects = append(reply.NativeObjects, envelope)
	}
	for i, file := range reading.Files {
		declared := &protocol.SQLDocumentDeclarations{DocumentId: documents[i].GetId()}
		for _, object := range file.Objects {
			declared.Objects = append(declared.Objects, &protocol.SQLDeclaration{Kind: object.Kind, Name: object.Name})
		}
		for _, object := range file.Guards {
			declared.Guards = append(declared.Guards, &protocol.SQLDeclaration{Kind: object.Kind, Name: object.Name})
		}
		for _, statement := range file.Tables {
			declared.Tables = append(declared.Tables, &protocol.SQLTableStatement{Name: statement.Name, Drop: statement.Drop, Guarded: statement.Guarded})
		}
		reply.Declarations = append(reply.Declarations, declared)
	}
	return reply, gaps
}

func gap(document int, text span, message string) *Refusal {
	return &Refusal{Document: document, Start: text.start, End: text.end, Code: Unsupported, Message: message}
}

// ydbPath writes a directory and a name as one YDB path.
func ydbPath(schema, name string) string {
	if schema == "" {
		return name
	}
	return schema + "/" + name
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

// indexSettings names the YDB settings an index declares beyond its options.
func indexSettings(index Index) []string {
	var settings []string
	if index.Partitioning != nil {
		settings = append(settings, "partitioning settings")
	}
	if index.Vector != nil {
		settings = append(settings, "vector settings")
	}
	return settings
}

// nativeObject is the reply's envelope for a YDB object, or the refusal of
// one a SQL schema reading has no home for.
func nativeObject(object Object) (*protocol.Envelope, *Refusal) {
	at := func(message string) *Refusal { return gap(object.document, object.span, message) }
	switch object.Kind {
	case KindCoordinationNode:
		spec := object.Spec.(*ydbdecl.CoordinationNode)
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
	case KindChangefeed:
		return nil, at(fmt.Sprintf("changefeed %s of table %s has no home in a SQL schema reading: "+
			"the host refuses the owned objects of a table in a SQL schema reading", quoteName(object.Name), quoteName(ydbPath(object.Schema, object.Parent))))
	case KindTopic:
		return nil, at(fmt.Sprintf("topic %s has no home in a SQL schema reading: this provider declares no topic from a source document", quoteName(ydbPath(object.Schema, object.Name))))
	case KindExternalSource:
		return nil, at(fmt.Sprintf("external data source %s has no home in a SQL schema reading: this provider declares no external data source from a source document",
			quoteName(ydbPath(object.Schema, object.Name))))
	default:
		return nil, at(fmt.Sprintf("external table %s has no home in a SQL schema reading: this provider declares no external table from a source document",
			quoteName(ydbPath(object.Schema, object.Name))))
	}
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

// claim is the coverage a load claims: every claimed model complete, less
// the coordination nodes the documents' headers leave unmanaged. A limit
// whose name no object of its kind can take is refused as the host refuses
// it.
func claim(limits []*protocol.SchemaCoverageLimit) (*protocol.ModelCoverage, *Refusal) {
	coverage := &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED}
	nodes := &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}
	var subjects []*protocol.ModelSubjectCoverage
	seen := map[string]bool{}
	for _, limit := range limits {
		if refusal := checkLimit(limit); refusal != nil {
			return nil, refusal
		}
		if limit.GetKind() != "coordination_node" {
			continue
		}
		if limit.GetName() == "" {
			nodes = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "the source leaves coordination nodes unmanaged"}
			continue
		}
		schemaName, name := coordinationLimit(limit.GetName())
		key := schemaName + "\x00" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: schemaName, Name: name}
		subjects = append(subjects, &protocol.ModelSubjectCoverage{Kind: schema.CoordinationProperties, Subject: owner, NormalizedSubject: proto.CloneOf(owner),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "the source leaves this object unmanaged"}})
	}
	for _, model := range ClaimedModels() {
		knowledge := &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}
		if model.GetKind() == schema.CoordinationProperties {
			knowledge = proto.CloneOf(nodes)
		}
		coverage.Kinds = append(coverage.Kinds, &protocol.ModelKindCoverage{Model: model, Knowledge: knowledge})
	}
	coverage.Subjects = subjects
	return coverage, nil
}

// coordinationLimit is the directory and the name of the node a header
// names: a path, or a name qualified by its directory with a dot.
func coordinationLimit(written string) (string, string) {
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
func limitDocument(request *protocol.ReadSQLSchemaRequest, limit *protocol.SchemaCoverageLimit) (int, int, int) {
	for index, document := range request.GetDocuments() {
		text := document.GetText()
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
