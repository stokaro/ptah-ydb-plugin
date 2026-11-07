// Package schemavalidation checks a captured desired schema against what the
// YDB provider can carry, offline. It reads only the request: it opens no
// session, loads no source document and builds no operation. The checks are
// the ones planning would refuse the schema for, reported up front as typed
// diagnostics so a refusal does not wait for the plan.
package schemavalidation

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Diagnostic kinds name the kind of declaration a diagnostic is about.
const (
	kindTable     = "table"
	kindNamespace = "schema"
	kindEnum      = "enum"
	kindPaths     = "table path"
	kindView      = "view"
)

// Validate checks every captured declaration and returns one completed result.
// A refusal of the declaration is a diagnostic. Failure to perform the check,
// a canceled context or an identifier context the provider cannot interpret,
// is an error and carries no diagnostics. Diagnostics are sorted, so the same
// schema yields the same result however the host ordered its capture.
//
// Table declarations are held to the rules CREATE compiles with, except the
// policy that only creation needs: access requirements and session-scoped
// tables are valid declarations. Standalone enums are unsupported and namespace
// comments, character sets and collations are dropped by YDB directories; both
// are reported only when the request asks for no skipped declarations, as the
// host's bundled validation does.
func Validate(ctx context.Context, request *protocol.ValidateSchemaRequest) (*protocol.SchemaValidation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := tablecreate.ValidateIdentifiers(request.GetIdentifiers()); err != nil {
		return nil, err
	}
	result := &protocol.SchemaValidation{Complete: true}
	add := func(code protocol.SchemaDiagnostic_Code, kind, object, feature, format string, args ...any) {
		result.Diagnostics = append(result.Diagnostics, &protocol.SchemaDiagnostic{Code: code, Kind: kind, Object: object, Feature: feature, Message: fmt.Sprintf(format, args...)})
	}
	tablePaths, err := validateTables(ctx, request.Tables, request.TargetFeatures, request.Capabilities, add)
	if err != nil {
		return nil, err
	}
	validateCommonObjects(request.GetCommonObjects(), tablePaths, add)
	validateNamespaces(request, add)
	validateEnums(request, add)
	if err := validateObjects(ctx, request, add); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(result.Diagnostics, func(a, b *protocol.SchemaDiagnostic) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Object, b.Object), cmp.Compare(a.Code, b.Code), cmp.Compare(a.Message, b.Message))
	})
	return result, nil
}

type addFunc func(code protocol.SchemaDiagnostic_Code, kind, object, feature, format string, args ...any)

// validateTables checks each table declaration. Where the host negotiated
// the common wire, carried, a comment a table declares on itself, a column or
// an index is the comment's own change: it is held to what YDB keeps in a
// table attribute, and the table is checked without it. It returns the path
// of each valid table relative to its database, keyed by the database the
// table names, with the table's name.
func validateTables(ctx context.Context, tables []*protocol.ValidationTable, features []string, caps map[string]bool, add addFunc) (map[string]map[string]string, error) {
	carried := slices.Contains(features, provider.CommonObjectsFeature)
	paths := make(map[string]map[string]string)
	for _, table := range tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := tableName(table.GetDeclaration())
		code := protocol.SchemaDiagnostic_INVALID_SCHEMA
		declaration := table.GetDeclaration()
		if carried {
			if err := commonobject.TableCommentRefusal(declaration); err != nil {
				add(protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, kindTable, name, "", "%s", err)
				continue
			}
			declaration = commonobject.WithoutComments(declaration)
		}
		if err := tablecreate.ValidateDeclaration(table.Subject, declaration, caps); err != nil {
			if tablecreate.IsUnsupported(err) {
				code = protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE
			}
			add(code, kindTable, name, "", "%s", err)
			continue
		}
		database, path, err := tablecreate.TablePath(table.Subject)
		if err != nil {
			add(code, kindTable, name, "", "%s", err)
			continue
		}
		if paths[database] == nil {
			paths[database] = make(map[string]string)
		}
		paths[database][path] = name
	}
	if len(paths) > 1 {
		if _, unbound := paths[""]; unbound {
			add(protocol.SchemaDiagnostic_INVALID_SCHEMA, kindPaths, "", "", "YDB table context has an unresolved database binding")
		}
	}
	for _, taken := range paths {
		for path, name := range taken {
			for parent := path; strings.Contains(parent, "/"); {
				parent = parent[:strings.LastIndex(parent, "/")]
				if other, nested := taken[parent]; nested {
					add(protocol.SchemaDiagnostic_INVALID_SCHEMA, kindPaths, name, "", "YDB table %s lies inside table %s; a table holds no children", path, other)
				}
			}
		}
	}
	return paths, nil
}

func tableName(table *protocol.DeclaredTable) string {
	if table.GetSchema() == "" {
		return table.GetName()
	}
	return table.GetSchema() + "/" + table.GetName()
}

// validateCommonObjects checks the views a declaration holds against what a
// YDB view can be, and against tablePaths, the paths of the declared tables
// by database: YDB keeps one object at a path, so CREATE VIEW at a table's
// path fails (`unexpected path type`). The SDK has refused every kind the
// engine does not carry.
func validateCommonObjects(declared *protocol.DeclaredCommonObjects, tablePaths map[string]map[string]string, add addFunc) {
	for _, role := range declared.GetRoles() {
		if err := commonobject.DeclaredRoleRefusal(role, declared.GetRoles()); err != nil {
			add(protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, "role", role.GetName(), "", "%s", err)
		}
	}
	for _, grant := range declared.GetGrants() {
		if err := commonobject.GrantRefusal(grant); err != nil {
			add(protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, "grant", grant.GetRole(), "", "%s", err)
		}
	}
	for _, view := range declared.GetViews() {
		if err := commonobject.Refusal(view); err != nil {
			add(protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, kindView, tableName(&protocol.DeclaredTable{Schema: view.GetSchema(), Name: view.GetName()}), "", "%s", err)
		}
		path := commonobject.ViewPath(view)
		for _, taken := range tablePaths {
			if _, held := taken[path]; held {
				add(protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, kindView, path, "",
					"view %s has the path of a declared table, and YDB keeps one object at a path (`unexpected path type`)", path)
				break
			}
		}
	}
}

func validateNamespaces(request *protocol.ValidateSchemaRequest, add addFunc) {
	for _, namespace := range request.Namespaces {
		valid := namespace.Name != ""
		for part := range strings.SplitSeq(namespace.Name, "/") {
			valid = valid && tablecreate.ValidName(part)
		}
		if !valid {
			add(protocol.SchemaDiagnostic_INVALID_SCHEMA, kindNamespace, namespace.Name, "", "invalid YDB directory declaration")
			continue
		}
		if !request.NoSkipped {
			continue
		}
		for _, setting := range []struct{ name, value string }{{"comment", namespace.Comment}, {"charset", namespace.Charset}, {"collation", namespace.Collation}} {
			if setting.value != "" {
				add(protocol.SchemaDiagnostic_OMITTED_DECLARATION, kindNamespace, namespace.Name, "", "%s=%s would be skipped", setting.name, setting.value)
			}
		}
	}
}

func validateEnums(request *protocol.ValidateSchemaRequest, add addFunc) {
	if !request.NoSkipped {
		return
	}
	for _, enum := range request.Enums {
		add(protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, kindEnum, enum.Name, "enum_custom_type", "enum %s, which requires target capability enum_custom_type, unavailable on this ydb target", enum.Name)
	}
}

// validateObjects checks the native models the whole-schema capture carries
// beyond the tables' own settings, which table validation already reads. A
// model the provider cannot normalize is an invalid declaration. A record whose
// receipt differs from the provider's own normalization is not a schema problem
// but a forged or stale capture, and fails the check instead.
func validateObjects(ctx context.Context, request *protocol.ValidateSchemaRequest, add addFunc) error {
	for _, record := range request.GetFeatures().GetObjects().GetRecords() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(record.Targets) > 0 && !slices.Contains(record.Targets, "ydb") {
			continue
		}
		object := record.GetSubject().GetName().GetSource()
		if record.Value == nil {
			add(protocol.SchemaDiagnostic_INVALID_SCHEMA, record.Kind, object, "", "YDB cannot satisfy an excluded object")
			continue
		}
		if !schema.IsNativeObject(record.Kind) && !schema.IsWorkloadObject(record.Kind) {
			add(protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, record.Kind, object, "", "YDB does not implement object model %s", record.Kind)
			continue
		}
		value, err := schema.NormalizeDesired(record.Value.GetValue())
		if err != nil {
			add(protocol.SchemaDiagnostic_INVALID_SCHEMA, record.Kind, object, "", "invalid YDB desired model: %s", err)
			continue
		}
		if !proto.Equal(value, record.Value.GetValue()) {
			return fmt.Errorf("YDB validation found a %s object whose normalization differs from its capture", record.Kind)
		}
		if record.Kind == schema.SecretProperties && !request.Capabilities["secrets"] {
			// The refusal names the secret by its path, as the secret's other
			// refusals do: two directories may hold secrets of one name.
			add(protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, record.Kind, object, "secrets", "secret %s requires the unavailable target capability secrets%s",
				secretPath(record.GetSubject().GetSchema().GetSource(), object), featureflags.Clause("secrets"))
		}
	}
	return nil
}

// secretPath writes a secret's directory and name as the one path its
// statements name.
func secretPath(directory, name string) string {
	if directory == "" {
		return name
	}
	return directory + "/" + name
}
