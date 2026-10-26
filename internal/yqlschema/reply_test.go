package yqlschema_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// readRequest is the request a provider-selected run of the host sends for a
// row: its documents in order, named by their place in the load, the
// database root, and the header entries the host reads for the engine.
func readRequest(r row) *protocol.ReadSQLSchemaRequest {
	description := backend.Description()
	engine := description.GetEngines()[0]
	request := &protocol.ReadSQLSchemaRequest{Engine: engine.GetId(), TargetFeatures: slices.Clone(engine.GetFeatures()),
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(engine.GetIdentifiers()), DefaultSchema: engine.GetDefaultSchema()},
		DatabasePath: r.golden.DatabasePath}
	for i, document := range r.documents {
		request.Documents = append(request.Documents, &protocol.SQLDocument{Id: strconv.Itoa(i + 1), Path: document.Path, Text: document.Text})
	}
	for _, limit := range r.golden.Limits {
		request.Limits = append(request.Limits, &protocol.SchemaCoverageLimit{Kind: limit.Kind, Name: limit.Name, Reason: limit.Reason, Provenance: limit.Provenance})
	}
	return request
}

// wireRows are the rows whose reading reaches the reply: the host's grammar
// accepts them, the host accepts the headers they hold, and this provider
// has a model for every statement.
func wireRows(c *qt.C) []row {
	c.Helper()
	listed := unsupportedRows(c)
	return corpus(c, func(r row) bool { return accepted(r) && r.golden.Load.Outcome == "accepted" && listed[r.name] == "" })
}

// gaps names what a reading declares that a SQL schema reading has no home
// for: a table's or an index's YDB settings, a password, and an object other
// than a coordination node, a topic, an external data source or an external
// table. A coordination node at a path the provider's model refuses counts as
// one too.
func gaps(reading *yqlschema.Reading) []string {
	var found []string
	for _, table := range reading.Tables {
		if !table.Facets.Empty() {
			found = append(found, "table "+table.Qualified())
		}
	}
	for _, index := range reading.Indexes {
		if len(yqlschema.IndexFacetPayloads(index)) != 0 {
			found = append(found, "index "+index.Name)
		}
	}
	for _, role := range reading.Roles {
		if role.Password != "" {
			found = append(found, "password "+role.Name)
		}
	}
	// The host names a pool it reads from a provider as it names a table, so
	// a pool whose name needs quoting would come back renamed.
	for _, grant := range append(slices.Clone(reading.Grants), reading.RevokedGrants...) {
		if grant.ObjectKind == "RESOURCE POOL" && strings.ContainsAny(grant.OnObject, ".\"`[]") {
			found = append(found, "grant on resource pool "+grant.OnObject)
		}
	}
	for _, object := range reading.Objects {
		switch object.Kind {
		case yqlschema.KindSecret:
			continue
		case yqlschema.KindCoordinationNode:
			owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: object.Schema, Name: object.Name}
			if schema.ValidateManagedCoordinationOwner(owner) == nil {
				continue
			}
		case yqlschema.KindTopic:
			if _, err := topics.Intent(*object.Spec.(*ydbdecl.Topic)); err == nil {
				continue
			}
		case yqlschema.KindExternalSource:
			if _, err := externals.SourceIntent(*object.Spec.(*ydbdecl.ExternalDataSource)); err == nil {
				continue
			}
		case yqlschema.KindExternalTable:
			if _, err := externals.TableIntent(*object.Spec.(*ydbdecl.ExternalTable)); err == nil {
				continue
			}
		case yqlschema.KindResourcePool, yqlschema.KindResourcePoolClassifier, yqlschema.KindStreamingQuery:
			continue
		}
		found = append(found, string(object.Kind)+" "+object.Name)
	}
	return found
}

func complete(c *qt.C, r row) bool {
	c.Helper()
	reading, refusal := yqlschema.Read(r.documents, r.golden.DatabasePath)
	c.Assert(refusal == nil, qt.IsTrue)
	return len(gaps(reading)) == 0
}

// TestReply_HappyPath answers every row whose reading has a home in a SQL
// schema reading, and requires a reply the SDK accepts that the host maps to
// the model its own reader makes: the common objects field for field, the
// coordination nodes, and the declarations of each document.
func TestReply_HappyPath(t *testing.T) {
	c := qt.New(t)
	rows := slices.DeleteFunc(wireRows(c), func(r row) bool { return !complete(c, r) })
	c.Assert(len(rows) >= 180, qt.IsTrue, qt.Commentf("%d rows reach a complete reply", len(rows)))
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			request := readRequest(r)
			reply := yqlschema.Reply(request)
			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsTrue, qt.Commentf("%v", reply.GetDiagnostics()))
			c.Assert(wireOrder(settle(c, hostMapping(c, reply))), qt.DeepEquals, wireOrder(settle(c, normalExternals(c, normalTopics(c, withoutFacets(hostMeaning(c, r.golden.Grammar)))))))
		})
	}
}

// TestReply_FailurePath answers every row whose reading declares something a
// SQL schema reading has no home for, and requires a refusal the SDK accepts
// that names each of them and exposes no schema.
func TestReply_FailurePath(t *testing.T) {
	c := qt.New(t)
	rows := slices.DeleteFunc(wireRows(c), func(r row) bool { return complete(c, r) })
	c.Assert(len(rows) >= 30, qt.IsTrue, qt.Commentf("%d rows reach a refusal", len(rows)))
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			request := readRequest(r)
			reply := yqlschema.Reply(request)
			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsFalse)
			reading, _ := yqlschema.Read(r.documents, r.golden.DatabasePath)
			c.Assert(reply.GetDiagnostics(), qt.HasLen, len(gaps(reading)))
		})
	}
}

// TestReply_RefusedLoad answers every row the host's grammar refuses with a
// reply the SDK accepts, whose first error is the refusal the host makes.
func TestReply_RefusedLoad(t *testing.T) {
	listed := unsupportedRows(qt.New(t))
	rows := corpus(qt.New(t), func(r row) bool { return refused(r) && listed[r.name] == "" })
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			request := readRequest(r)
			reply := yqlschema.Reply(request)
			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsFalse)
			c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
			want := expectedRefusal(c, r.golden.Grammar.Error)
			diagnostic := reply.GetDiagnostics()[0]
			got := hostRefusal{Document: documentIndex(c, diagnostic.GetDocumentId()), Offset: int(diagnostic.GetSpan().GetStart().GetByteOffset()),
				Message: diagnostic.GetMessage()}
			got.Offset = map[bool]int{true: -1, false: got.Offset}[want.Offset < 0]
			c.Assert(got, qt.DeepEquals, hostRefusal{Document: want.Document, Offset: want.Offset, Message: boundedMessage(want.Message)})
		})
	}
}

func documentIndex(c *qt.C, id string) int {
	c.Helper()
	number, err := strconv.Atoi(id)
	c.Assert(err, qt.IsNil)
	return number - 1
}

// boundedMessage is a host message as a diagnostic carries it: one line of
// at most provider.MaxSQLDiagnosticMessage bytes.
func boundedMessage(message string) string {
	message = strings.Join(strings.FieldsFunc(message, func(r rune) bool { return r == '\n' || r == '\r' || r == '\t' }), " ")
	if len(message) <= provider.MaxSQLDiagnosticMessage {
		return message
	}
	return message[:provider.MaxSQLDiagnosticMessage-3] + "..."
}

// settle is a meaning as the host settles a load's model once it is read:
// each table, column, index, view, role and grant once, the first of each
// kept, and the tables in the order of their qualified names.
func settle(c *qt.C, m meaning) meaning {
	c.Helper()
	first := func(values []map[string]any, key func(map[string]any) string) []map[string]any {
		seen := map[string]bool{}
		var kept []map[string]any
		for _, value := range values {
			if k := key(value); !seen[k] {
				seen[k] = true
				kept = append(kept, value)
			}
		}
		return kept
	}
	text := func(value map[string]any, key string) string {
		held, _ := value[key].(string)
		return held
	}
	qualified := func(table map[string]any) string { return canonical(text(table, "Schema"), text(table, "Name")) }
	m.Tables = first(m.Tables, qualified)
	slices.SortStableFunc(m.Tables, func(a, b map[string]any) int { return strings.Compare(qualified(a), qualified(b)) })
	m.Fields = first(m.Fields, func(field map[string]any) string { return text(field, "Table") + "\x00" + text(field, "Name") })
	m.Indexes = first(m.Indexes, func(index map[string]any) string { return text(index, "TableName") + "\x00" + text(index, "Name") })
	m.Views = first(m.Views, func(view map[string]any) string { return text(view, "Name") })
	m.Roles = first(m.Roles, func(role map[string]any) string { return text(role, "Name") })
	whole := func(value map[string]any) string { return string(marshal(c, value)) }
	m.Grants = first(m.Grants, whole)
	m.RevokedGrants = first(m.RevokedGrants, whole)
	return m
}

// withoutFacets is a meaning with the settings of tables and indexes left
// out, which a complete reply carries none of.
func withoutFacets(m meaning) meaning {
	m.TableFacets, m.IndexFacets = map[string]map[string]any{}, map[string]map[string]any{}
	return m
}

// wireOrder orders the columns of a meaning by table. The host's reader
// keeps the columns of a load in the order the documents create them; a
// reply lists each table's columns with the table, and the host keeps the
// order of a table's own columns either way.
func wireOrder(m meaning) meaning {
	slices.SortStableFunc(m.Fields, func(a, b map[string]any) int { return strings.Compare(a["Table"].(string), b["Table"].(string)) })
	slices.SortStableFunc(m.Indexes, func(a, b map[string]any) int {
		return strings.Compare(a["TableName"].(string), b["TableName"].(string))
	})
	return m
}

// hostMapping is the meaning of the model the host maps a complete reply to,
// as internal/providerhost maps a SQL schema reading: each table at its
// qualified name with its columns and indexes, each view at its qualified
// name, each grant at the field of the host's model its target's kind names,
// and each coordination node with the settings its envelope holds.
func hostMapping(c *qt.C, reply *protocol.SQLSchema) meaning {
	c.Helper()
	result := meaning{TableFacets: map[string]map[string]any{}, IndexFacets: map[string]map[string]any{}}
	for _, table := range reply.GetTables() {
		qualified := canonical(table.GetSchema(), table.GetName())
		object := map[string]any{"Name": table.GetName(), "Schema": table.GetSchema(), "Comment": table.GetComment(), "PrimaryKey": table.GetPrimaryKey()}
		var parts []map[string]any
		for _, part := range table.GetPrimaryKeyParts() {
			parts = append(parts, map[string]any{"Name": part.GetColumn(), "Prefix": part.GetPrefix(), "Desc": part.GetDescending()})
		}
		object["PrimaryKeyParts"] = parts
		result.Tables = append(result.Tables, element(c, marshal(c, object)))
		for _, column := range table.GetColumns() {
			field := element(c, marshal(c, map[string]any{"Name": column.GetName(), "Type": column.GetType(), "Nullable": column.GetNullable(),
				"Primary": column.GetPrimary(), "IdentityStart": column.GetIdentityStart(), "IdentityIncrement": column.GetIdentityIncrement(),
				"Default": column.GetDefaultValue(), "DefaultSet": column.GetDefaultSet(), "Comment": column.GetComment()}))
			field["Table"] = qualified
			result.Fields = append(result.Fields, field)
		}
		for _, index := range table.GetIndexes() {
			tableName := index.GetTableName()
			if tableName == "" {
				tableName = qualified
			}
			result.Indexes = append(result.Indexes, element(c, marshal(c, map[string]any{"Name": index.GetName(), "Fields": index.GetFields(),
				"Unique": index.GetUnique(), "Comment": index.GetComment(), "Type": index.GetType(), "IncludeColumns": index.GetIncludeColumns(),
				"StorageParams": index.GetStorageParameters(), "TableName": tableName})))
		}
	}
	for _, view := range reply.GetViews() {
		result.Views = append(result.Views, element(c, marshal(c, map[string]any{"Name": canonical(view.GetSchema(), view.GetName()), "Body": view.GetBody(),
			"Comment": view.GetComment()})))
	}
	for _, role := range reply.GetRoles() {
		result.Roles = append(result.Roles, element(c, marshal(c, map[string]any{"Name": role.GetName(), "Login": role.GetLogin(), "Inherit": role.GetInherit(),
			"Group": role.GetGroup(), "MemberOf": role.GetMemberOf()})))
	}
	for _, grant := range reply.GetGrants() {
		object := map[string]any{"Role": grant.GetRole(), "Privileges": grant.GetPrivileges()}
		target := grant.GetTarget()
		switch kind := strings.ToLower(target.GetKind()); kind {
		case "table":
			object["OnTable"] = canonical(target.GetSchema(), target.GetName())
		case "schema":
			object["OnSchema"] = target.GetName()
		case "database":
			object["OnDatabase"] = true
		default:
			object["OnObject"], object["ObjectKind"] = canonical(target.GetSchema(), target.GetName()), strings.ToUpper(kind)
		}
		mapped := element(c, marshal(c, object))
		if grant.GetRevoked() {
			result.RevokedGrants = append(result.RevokedGrants, mapped)
		} else {
			result.Grants = append(result.Grants, mapped)
		}
	}
	for _, envelope := range reply.GetNativeObjects() {
		owner := envelope.GetOwner()
		if envelope.GetKind() == schema.TopicProperties {
			intent, err := schema.DesiredTopic(envelope)
			c.Assert(err, qt.IsNil)
			spec, err := topics.FromIntent(intent)
			c.Assert(err, qt.IsNil)
			result.Objects = append(result.Objects, objectMeaning{Kind: string(yqlschema.KindTopic), Schema: owner.GetSchema(), Name: owner.GetName(),
				Payload: topicPayload(c, spec)})
			continue
		}
		if envelope.GetKind() == schema.ExternalSourceProperties {
			intent, err := schema.DesiredExternalSource(envelope)
			c.Assert(err, qt.IsNil)
			spec, err := externals.SourceFromIntent(intent, "")
			c.Assert(err, qt.IsNil)
			result.Objects = append(result.Objects, objectMeaning{Kind: string(yqlschema.KindExternalSource), Schema: owner.GetSchema(), Name: owner.GetName(),
				Payload: decodeAny(c, marshal(c, map[string]any{"spec": spec}))})
			continue
		}
		if envelope.GetKind() == schema.ExternalTableProperties {
			intent, err := schema.DesiredExternalTable(envelope)
			c.Assert(err, qt.IsNil)
			spec, err := externals.TableFromIntent(intent, "")
			c.Assert(err, qt.IsNil)
			result.Objects = append(result.Objects, objectMeaning{Kind: string(yqlschema.KindExternalTable), Schema: owner.GetSchema(), Name: owner.GetName(),
				Payload: decodeAny(c, marshal(c, map[string]any{"spec": spec}))})
			continue
		}
		switch envelope.GetKind() {
		case schema.SecretProperties:
			intent, err := schema.DesiredSecret(envelope)
			c.Assert(err, qt.IsNil)
			result.Objects = append(result.Objects, objectMeaning{Kind: string(yqlschema.KindSecret), Schema: owner.GetSchema(), Name: owner.GetName(), Payload: map[string]any{"value_env": intent.ValueEnv}})
			continue
		case schema.StreamingQueryProperties:
			spec, allow, err := schema.DesiredStreamingQuery(envelope)
			c.Assert(err, qt.IsNil)
			result.Objects = append(result.Objects, objectMeaning{Kind: string(yqlschema.KindStreamingQuery), Schema: owner.GetSchema(), Name: owner.GetName(), Payload: decodeAny(c, marshal(c, map[string]any{"spec": spec, "allow_state_reset": allow}))})
			continue
		case schema.ResourcePoolProperties:
			spec, err := schema.WorkloadPool(envelope)
			c.Assert(err, qt.IsNil)
			result.Objects = append(result.Objects, objectMeaning{Kind: string(yqlschema.KindResourcePool), Name: owner.GetName(),
				Payload: decodeAny(c, marshal(c, map[string]any{"spec": spec}))})
			continue
		case schema.ResourcePoolClassifierProperties:
			spec, err := schema.WorkloadClassifier(envelope)
			c.Assert(err, qt.IsNil)
			result.Objects = append(result.Objects, objectMeaning{Kind: string(yqlschema.KindResourcePoolClassifier), Name: owner.GetName(),
				Payload: decodeAny(c, marshal(c, map[string]any{"spec": spec}))})
			continue
		}
		c.Assert(envelope.GetKind(), qt.Equals, schema.CoordinationProperties)
		var spec map[string]any
		c.Assert(json.Unmarshal(envelope.GetPayload(), &spec), qt.IsNil)
		result.Objects = append(result.Objects, objectMeaning{Kind: string(yqlschema.KindCoordinationNode), Schema: owner.GetSchema(), Name: owner.GetName(),
			Payload: map[string]any{"spec": spec}})
	}
	for _, declared := range reply.GetDeclarations() {
		var file fileMeaning
		for _, object := range declared.GetObjects() {
			file.Objects = append(file.Objects, yqlschema.Declaration{Kind: object.GetKind(), Name: object.GetName()})
		}
		for _, object := range declared.GetGuards() {
			file.Guards = append(file.Guards, yqlschema.Declaration{Kind: object.GetKind(), Name: object.GetName()})
		}
		for _, statement := range declared.GetTables() {
			file.Tables = append(file.Tables, yqlschema.TableStatement{Name: statement.GetName(), Drop: statement.GetDrop(), Guarded: statement.GetGuarded()})
		}
		file.Objects = append([]yqlschema.Declaration{}, file.Objects...)
		result.Files = append(result.Files, file)
	}
	return result
}

// topicPayload is a topic as the host's payload holds it, in the spelling the
// provider's model keeps: intervals as whole seconds written the host's way,
// and consumers sorted by name.
func topicPayload(c *qt.C, spec ydbdecl.Topic) any {
	c.Helper()
	intent, err := topics.Intent(spec)
	c.Assert(err, qt.IsNil)
	read, err := topics.FromIntent(intent)
	c.Assert(err, qt.IsNil)
	slices.SortFunc(read.Consumers, func(a, b ydbdecl.Consumer) int { return strings.Compare(a.Name, b.Name) })
	return decodeAny(c, marshal(c, map[string]any{"spec": read}))
}

// normalTopics writes the host's topics in the spelling the provider's model
// keeps, so a reply is held to their meaning rather than to their spelling:
// PT36H and P1DT12H are one retention period.
func normalTopics(c *qt.C, m meaning) meaning {
	c.Helper()
	for i, object := range m.Objects {
		if object.Kind != string(yqlschema.KindTopic) {
			continue
		}
		var held struct {
			Spec ydbdecl.Topic `json:"spec"`
		}
		c.Assert(json.Unmarshal(marshal(c, object.Payload), &held), qt.IsNil)
		m.Objects[i].Payload = topicPayload(c, held.Spec)
	}
	return m
}

// normalExternals writes the host's external tables in the spelling the
// provider's model keeps, so a reply is held to their meaning rather than to
// their spelling: a column type is written as YDB writes it, so
// Decimal(22, 9) and Decimal(22,9) are one type, and a PARTITIONED_BY list
// is written without spaces.
func normalExternals(c *qt.C, m meaning) meaning {
	c.Helper()
	for i, object := range m.Objects {
		if object.Kind != string(yqlschema.KindExternalTable) {
			continue
		}
		var held struct {
			Spec ydbdecl.ExternalTable `json:"spec"`
		}
		c.Assert(json.Unmarshal(marshal(c, object.Payload), &held), qt.IsNil)
		intent, err := externals.TableIntent(held.Spec)
		c.Assert(err, qt.IsNil)
		spec, err := externals.TableFromIntent(intent, "")
		c.Assert(err, qt.IsNil)
		m.Objects[i].Payload = decodeAny(c, marshal(c, map[string]any{"spec": spec}))
	}
	return m
}

// TestReply_Coverage requires a complete reply to claim what the host's YDB
// owner claims for the load about the models this provider claims: every
// coordination node complete, less the nodes and the namespace the headers
// leave unmanaged, and a table's settings, its columns' and its storage
// complete.
func TestReply_Coverage(t *testing.T) {
	c := qt.New(t)
	rows := slices.DeleteFunc(wireRows(c), func(r row) bool { return !complete(c, r) })
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			reply := yqlschema.Reply(readRequest(r))
			c.Assert(pluginCoverage(reply.GetCoverage()), qt.DeepEquals, hostCoverage(c, r.golden.Load.Coverage))
		})
	}
}

// coverageMeaning is what a claim says about the models both owners claim:
// the knowledge of each, in the host's model kind, and the coordination
// nodes it leaves unmanaged.
type coverageMeaning struct {
	Kinds    map[string]string
	Subjects []string
}

// claimedHostKinds are the host's models a claim of this provider's models
// stands for.
var claimedHostKinds = map[string][]string{
	schema.SecretProperties:                 {"ptah.run/ydb/secret"},
	schema.TableProperties:                  {"ptah.run/ydb/table-partitioning", "ptah.run/ydb/ttl", "ptah.run/ydb/column-families"},
	schema.ColumnProperties:                 {"ptah.run/ydb/column-families"},
	schema.ColumnStorageProperties:          {"ptah.run/ydb/column-store"},
	schema.CoordinationProperties:           {"ptah.run/ydb/coordination-node"},
	schema.TopicProperties:                  {"ptah.run/ydb/topic"},
	schema.ExternalSourceProperties:         {"ptah.run/ydb/external-data-source"},
	schema.ExternalTableProperties:          {"ptah.run/ydb/external-table"},
	schema.ResourcePoolProperties:           {"ptah.run/ydb/resource-pool"},
	schema.ResourcePoolClassifierProperties: {"ptah.run/ydb/resource-pool-classifier"},
}

func pluginCoverage(coverage *protocol.ModelCoverage) coverageMeaning {
	result := coverageMeaning{Kinds: map[string]string{}}
	for _, record := range coverage.GetKinds() {
		for _, kind := range claimedHostKinds[record.GetModel().GetKind()] {
			result.Kinds[kind] = strings.ToLower(record.GetKnowledge().GetState().String()) + " " + record.GetKnowledge().GetReason()
		}
	}
	for _, subject := range coverage.GetSubjects() {
		result.Subjects = append(result.Subjects, subject.GetSubject().GetSchema()+"/"+subject.GetSubject().GetName()+" "+subject.GetKnowledge().GetReason())
	}
	return result
}

func hostCoverage(c *qt.C, coverage *goldenCoverage) coverageMeaning {
	c.Helper()
	result := coverageMeaning{Kinds: map[string]string{}}
	claimed := map[string]bool{}
	for _, kinds := range claimedHostKinds {
		for _, kind := range kinds {
			claimed[kind] = true
		}
	}
	for _, record := range coverage.Kinds {
		if !claimed[record.Model.Kind] {
			continue
		}
		c.Assert(record.Knowledge, qt.HasLen, 1)
		var knowledge struct {
			Outside struct {
				State  string `json:"state"`
				Reason string `json:"reason"`
			} `json:"outside"`
		}
		c.Assert(json.Unmarshal(record.Knowledge[0], &knowledge), qt.IsNil)
		state := map[string]string{"complete": "complete", "": "uninspected"}[knowledge.Outside.State]
		result.Kinds[record.Model.Kind] = state + " " + knowledge.Outside.Reason
	}
	for _, subject := range coverage.Subjects {
		if !slices.Contains([]string{"ptah.run/ydb/secret", "ptah.run/ydb/coordination-node", "ptah.run/ydb/topic", "ptah.run/ydb/external-data-source", "ptah.run/ydb/external-table", "ptah.run/ydb/resource-pool", "ptah.run/ydb/resource-pool-classifier"}, subject.Kind) {
			continue
		}
		var knowledge struct {
			Reason string `json:"reason"`
		}
		c.Assert(json.Unmarshal(subject.Knowledge, &knowledge), qt.IsNil)
		result.Subjects = append(result.Subjects, partSource(subject.Subject.Schema)+"/"+subject.Subject.Name.Source+" "+knowledge.Reason)
	}
	return result
}

// TestReply_HeaderLimits requires the provider to refuse a header entry the
// host's YDB owner refuses for a load its grammar reads, at the header line
// that wrote it, with the host's words and this provider's identity in place
// of the host owner's.
func TestReply_HeaderLimits(t *testing.T) {
	c := qt.New(t)
	rows := corpus(c, func(r row) bool {
		return accepted(r) && r.golden.Load.Outcome == "refused" && strings.Contains(r.golden.Load.Error, "coverage of ptah.run/ydb")
	})
	c.Assert(len(rows) >= 1, qt.IsTrue)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			request := readRequest(r)
			reply := yqlschema.Reply(request)
			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
			want := expectedRefusal(c, strings.Replace(r.golden.Load.Error, "coverage of ptah.run/ydb", "coverage of "+schema.ProviderID, 1))
			diagnostic := reply.GetDiagnostics()[0]
			c.Assert(hostRefusal{Document: documentIndex(c, diagnostic.GetDocumentId()), Offset: -1, Message: diagnostic.GetMessage()}, qt.DeepEquals, want)
		})
	}
}
