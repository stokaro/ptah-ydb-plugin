package yqlschema

import (
	"cmp"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// Reading is what a load of YQL schema documents declares, in the terms of
// the Ptah host's model: what its own reader of YQL returns. Each element's
// JSON form uses the field names of that model, so the corpus compares the two
// readings field by field. A table's columns and indexes name it by its
// qualified name ([Table.Qualified]).
type Reading struct {
	Tables        []Table  `json:"Tables"`
	Fields        []Field  `json:"Fields"`
	Indexes       []Index  `json:"Indexes"`
	Views         []View   `json:"Views"`
	Roles         []Role   `json:"Roles"`
	Grants        []Grant  `json:"Grants"`
	RevokedGrants []Grant  `json:"RevokedGrants"`
	Objects       []Object `json:"-"`
	// Files is what each document declares, in load order, for the host's
	// check that a later file of a schema directory does not declare again
	// what an earlier one did.
	Files []FileDeclarations `json:"-"`
}

// Table is a row or column table.
type Table struct {
	Name            string           `json:"Name"`
	Schema          string           `json:"Schema"`
	Comment         string           `json:"Comment"`
	PrimaryKey      []string         `json:"PrimaryKey"`
	PrimaryKeyParts []PrimaryKeyPart `json:"PrimaryKeyParts"`
	// Facets are the YDB settings the table declares.
	Facets ydbdecl.TableFacets `json:"-"`
	// span is the CREATE TABLE that declared the table.
	span span
	// document is the place of that statement's document in the load.
	document int
}

// PrimaryKeyPart is one column of a primary key.
type PrimaryKeyPart struct {
	Name   string `json:"Name"`
	Prefix string `json:"Prefix"`
	Desc   bool   `json:"Desc"`
}

// Qualified is the table's name qualified by its directory, as the host's
// model writes it.
func (t Table) Qualified() string { return canonicalReference(t.Schema, t.Name) }

// Field is one column of a table.
type Field struct {
	// Table is the qualified name of the column's table.
	Table             string `json:"-"`
	Name              string `json:"Name"`
	Type              string `json:"Type"`
	Nullable          bool   `json:"Nullable"`
	Primary           bool   `json:"Primary"`
	IdentityStart     string `json:"IdentityStart"`
	IdentityIncrement string `json:"IdentityIncrement"`
	Default           string `json:"Default"`
	DefaultSet        bool   `json:"DefaultSet"`
	Comment           string `json:"Comment"`
}

// Index is a secondary index of a table.
type Index struct {
	Name           string            `json:"Name"`
	Fields         []string          `json:"Fields"`
	Unique         bool              `json:"Unique"`
	Comment        string            `json:"Comment"`
	Type           string            `json:"Type"`
	IncludeColumns []string          `json:"IncludeColumns"`
	StorageParams  map[string]string `json:"StorageParams"`
	// TableName is the qualified name of the index's table.
	TableName    string                     `json:"TableName"`
	Partitioning *ydbdecl.IndexPartitioning `json:"-"`
	Vector       *ydbdecl.VectorIndex       `json:"-"`
	span         span
	document     int
}

// View is a view.
type View struct {
	// Name is the view's name qualified by its directory.
	Name     string `json:"Name"`
	Body     string `json:"Body"`
	Comment  string `json:"Comment"`
	schema   string
	leaf     string
	span     span
	document int
}

// Role is a YDB user or group.
type Role struct {
	Name     string   `json:"Name"`
	Login    bool     `json:"Login"`
	Password string   `json:"Password"`
	Inherit  bool     `json:"Inherit"`
	Group    bool     `json:"Group"`
	MemberOf []string `json:"MemberOf"`
	span     span
	document int
	// passwordAt is the statement that set the password, when one did, and
	// passwordDocument the document that holds it.
	passwordAt       *span
	passwordDocument int
}

// Grant is permissions a subject holds on one object, or, among the revoked
// grants, one permission it does not.
type Grant struct {
	Role       string   `json:"Role"`
	Privileges []string `json:"Privileges"`
	OnTable    string   `json:"OnTable"`
	OnSchema   string   `json:"OnSchema"`
	OnDatabase bool     `json:"OnDatabase"`
	OnObject   string   `json:"OnObject"`
	ObjectKind string   `json:"ObjectKind"`
	// target names the object as the reply's reference does.
	target   grantTarget
	span     span
	document int
}

// grantTarget is the object of a grant, by kind, directory and name.
type grantTarget struct {
	kind, schema, name string
}

// ObjectKind is the kind of a YDB object the host's YDB owner models, by its
// model kind in the host.
type ObjectKind string

// The kinds of objects a YQL document declares beside tables, views and
// principals.
const (
	KindChangefeed       ObjectKind = "ptah.run/ydb/changefeed"
	KindCoordinationNode ObjectKind = "ptah.run/ydb/coordination-node"
	KindExternalSource   ObjectKind = "ptah.run/ydb/external-data-source"
	KindExternalTable    ObjectKind = "ptah.run/ydb/external-table"
	KindStreamingQuery   ObjectKind = "ptah.run/ydb/streaming-query"
	KindTopic            ObjectKind = "ptah.run/ydb/topic"
	// KindResourcePool and KindResourcePoolClassifier belong to the whole
	// database: their objects have no directory.
	KindResourcePool           ObjectKind = "ptah.run/ydb/resource-pool"
	KindResourcePoolClassifier ObjectKind = "ptah.run/ydb/resource-pool-classifier"
)

// Object is one YDB object the load declares: its kind, its directory, its
// parent table for a changefeed, its name and its specification.
type Object struct {
	Kind   ObjectKind
	Schema string
	Parent string
	Name   string
	// Spec is a *ydbdecl.Topic, *ydbdecl.CoordinationNode,
	// *ydbdecl.ExternalDataSource, *ydbdecl.ExternalTable,
	// *ydbdecl.Changefeed, *workload.PoolSpec or *workload.ClassifierSpec.
	Spec     any
	span     span
	document int
}

// compareObjects orders objects as the host orders feature objects: by kind,
// directory, parent and name.
func compareObjects(a, b Object) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Parent, b.Parent), cmp.Compare(a.Name, b.Name))
}

func sortObjects(objects []Object) { slices.SortStableFunc(objects, compareObjects) }

// FileDeclarations is what one document declares.
type FileDeclarations struct {
	Objects []Declaration    `json:"Objects"`
	Guards  []Declaration    `json:"Guards"`
	Tables  []TableStatement `json:"Tables"`
}

// Declaration is one object a document declares: its kind and its name as
// the document writes it.
type Declaration struct {
	Kind string `json:"Kind"`
	Name string `json:"Name"`
}

// TableStatement is one CREATE TABLE a document runs.
type TableStatement struct {
	Name    string `json:"Name"`
	Drop    bool   `json:"Drop"`
	Guarded bool   `json:"Guarded"`
}

// TableFacetPayloads are the YDB settings a table declares, each by the
// host's model kind and in the form the host's codec writes its payload.
func TableFacetPayloads(table Table) map[string]any {
	result := map[string]any{}
	facets := table.Facets
	if facets.Partitioning != nil {
		result["ptah.run/ydb/table-partitioning"] = facets.Partitioning
	}
	if len(facets.Families) > 0 {
		result["ptah.run/ydb/column-families"] = ydbdecl.ColumnFamilies{Families: facets.Families}
	}
	if facets.TTL != nil {
		result["ptah.run/ydb/ttl"] = facets.TTL
	}
	if facets.ColumnStore != nil {
		result["ptah.run/ydb/column-store"] = facets.ColumnStore
	}
	return result
}

// IndexFacetPayloads are the YDB settings an index declares, as
// [TableFacetPayloads] writes a table's.
func IndexFacetPayloads(index Index) map[string]any {
	result := map[string]any{}
	if index.Partitioning != nil {
		result["ptah.run/ydb/index-partitioning"] = index.Partitioning
	}
	if index.Vector != nil {
		result["ptah.run/ydb/vector-index"] = index.Vector
	}
	return result
}

// SortedObjects are the objects a reading declares in the host's order.
func SortedObjects(r *Reading) []Object {
	objects := slices.Clone(r.Objects)
	sortObjects(objects)
	return objects
}

// ObjectPayload is an object's specification in the form the host's codec
// writes its payload.
func ObjectPayload(object Object) any {
	if object.Kind == KindStreamingQuery {
		node := object.Spec.(*createStreamingQuery)
		return struct {
			Spec            any  `json:"spec"`
			AllowStateReset bool `json:"allow_state_reset"`
		}{node.spec, node.allowStateReset}
	}
	return struct {
		Spec any `json:"spec"`
	}{Spec: object.Spec}
}
