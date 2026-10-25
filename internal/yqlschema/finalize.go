package yqlschema

import (
	"fmt"
	"slices"
	"strings"
)

// finalize settles one document's reading as the host's model settles a
// document it read: each table, column, index, view, role and grant once,
// the first declaration of each kept, and the tables in name order, which is
// the order the host gives tables no key orders.
func finalize(r *Reading) {
	r.Tables = firstOf(r.Tables, func(table Table) string { return table.Qualified() })
	r.Fields = firstOf(r.Fields, func(field Field) string { return field.Table + "\x00" + field.Name })
	r.Indexes = firstOf(r.Indexes, func(index Index) string { return index.TableName + "\x00" + index.Name })
	r.Views = firstOf(r.Views, func(view View) string { return view.Name })
	r.Roles = firstOf(r.Roles, func(role Role) string { return role.Name })
	r.Grants = firstOf(canonicalGrants(r.Grants), grantKey)
	r.RevokedGrants = firstOf(canonicalGrants(r.RevokedGrants), grantKey)
	slices.SortStableFunc(r.Tables, func(a, b Table) int { return strings.Compare(a.Qualified(), b.Qualified()) })
}

func canonicalGrants(grants []Grant) []Grant {
	for i := range grants {
		grants[i].canonicalize()
	}
	return grants
}

// grantKey is the identity two grants are one grant by: the subject, the
// permissions in any order and the object.
func grantKey(g Grant) string {
	privileges := slices.Clone(g.Privileges)
	slices.Sort(privileges)
	return fmt.Sprintf("%q %q %q %q %v %q %q", g.Role, strings.Join(privileges, "\x00"), g.OnTable, g.OnSchema, g.OnDatabase, g.OnObject, g.ObjectKind)
}

func firstOf[T any](values []T, key func(T) string) []T {
	seen := make(map[string]bool, len(values))
	kept := make([]T, 0, len(values))
	for _, value := range values {
		if k := key(value); !seen[k] {
			seen[k] = true
			kept = append(kept, value)
		}
	}
	return kept
}

// append adds the reading of a later document to what the load has read: its
// declarations after the earlier ones, its objects merged by identity, and its
// grants and revocations folded into the earlier ones. It returns the object
// of the later document an earlier one already declares, which the load
// refuses, and nil otherwise.
func (r *Reading) append(file *Reading) *Object {
	for i, object := range file.Objects {
		if held := r.object(object); held != nil {
			return &file.Objects[i]
		}
		r.Objects = append(r.Objects, object)
	}
	r.Tables = append(r.Tables, file.Tables...)
	r.Fields = append(r.Fields, file.Fields...)
	r.Indexes = append(r.Indexes, file.Indexes...)
	r.Views = append(r.Views, file.Views...)
	r.Roles = append(r.Roles, file.Roles...)
	foldGrants(r, file.RevokedGrants, file.Grants)
	r.Files = append(r.Files, file.Files...)
	return nil
}

// declarationsOf is what one document declares once it has run: its tables,
// views, indexes, objects and roles by the names the host's directory check
// reads, the tables it creates under IF NOT EXISTS, and its CREATE TABLE
// statements in order.
func declarationsOf(file *Reading, statements []statement) FileDeclarations {
	var result FileDeclarations
	result.Objects = []Declaration{}
	for _, table := range file.Tables {
		result.Objects = append(result.Objects, Declaration{Kind: "table", Name: table.Qualified()})
	}
	for _, view := range file.Views {
		result.Objects = append(result.Objects, Declaration{Kind: "view", Name: view.Name})
	}
	for _, index := range file.Indexes {
		result.Objects = append(result.Objects, Declaration{Kind: "index", Name: index.TableName + "." + index.Name})
	}
	objects := slices.Clone(file.Objects)
	sortObjects(objects)
	for _, object := range objects {
		result.Objects = append(result.Objects, Declaration{Kind: objectNoun(object.Kind), Name: objectPath(object)})
	}
	for _, role := range file.Roles {
		result.Objects = append(result.Objects, Declaration{Kind: "role", Name: role.Name})
	}
	for _, statement := range statements {
		table, ok := statement.(*createTable)
		if !ok {
			continue
		}
		if table.ifNotExists {
			result.Guards = append(result.Guards, Declaration{Kind: "table", Name: table.name})
		}
		result.Tables = append(result.Tables, TableStatement{Name: table.name, Guarded: table.ifNotExists})
	}
	return result
}

// objectNoun names an object kind as the host's directory check does: the
// last segment of the kind, hyphens read as spaces.
func objectNoun(kind ObjectKind) string {
	noun := string(kind)
	if index := strings.LastIndex(noun, "/"); index >= 0 {
		noun = noun[index+1:]
	}
	return strings.ReplaceAll(noun, "-", " ")
}

// clone returns a reading that shares no slice with r.
func (r *Reading) clone() *Reading {
	return &Reading{Tables: slices.Clone(r.Tables), Fields: slices.Clone(r.Fields), Indexes: slices.Clone(r.Indexes), Views: slices.Clone(r.Views),
		Roles: slices.Clone(r.Roles), Grants: slices.Clone(r.Grants), RevokedGrants: slices.Clone(r.RevokedGrants), Objects: slices.Clone(r.Objects),
		Files: slices.Clone(r.Files)}
}
