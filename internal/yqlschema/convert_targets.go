package yqlschema

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// The object kinds a grant names beyond the database, a directory and a
// table, as the host's model spells them, with the host's object kind each
// is.
var grantObjectKinds = []struct {
	objectKind string
	kind       ObjectKind
}{
	{"TOPIC", KindTopic},
	{"COORDINATION NODE", KindCoordinationNode},
	{"EXTERNAL DATA SOURCE", KindExternalSource},
	{"EXTERNAL TABLE", KindExternalTable},
	{"RESOURCE POOL", KindResourcePool},
}

// poolDirectory is where YDB keeps a resource pool's access entries: the
// path a grant on the pool names.
const poolDirectory = ".metadata/workload_manager/pools"

// privilegeTarget resolves the path a GRANT or a REVOKE names to the object
// the load declares there: the database, a table, a view, another scheme
// object, or a directory, in the statement's document or an earlier one.
func (c *converter) privilegeTarget(name string) (Grant, error) {
	relative, err := c.relativePath(name)
	if err != nil {
		return Grant{}, err
	}
	if relative == "" {
		return Grant{OnDatabase: true, target: grantTarget{kind: "database"}}, nil
	}
	for _, reading := range c.readings() {
		for _, table := range reading.Tables {
			if path.Join(table.Schema, table.Name) == relative {
				schema, leaf := splitTableName(quoteName(relative))
				return Grant{OnTable: canonicalReference(schema, leaf), target: grantTarget{kind: "table", schema: schema, name: leaf}}, nil
			}
		}
		// A grant on a view is a grant on a table, as the reader reports it.
		for _, view := range reading.Views {
			if path.Join(view.schema, view.leaf) == relative {
				return Grant{OnTable: view.Name, target: grantTarget{kind: "table", schema: view.schema, name: view.leaf}}, nil
			}
		}
		if grant, ok := reading.declaredSchemeObject(relative); ok {
			return grant, nil
		}
		if reading.declaresDirectory(relative) {
			return Grant{OnSchema: relative, target: grantTarget{kind: "schema", name: relative}}, nil
		}
	}
	return Grant{}, fmt.Errorf("%w: YDB permission path %q must name a table, a view, a directory "+
		"or another scheme object, such as a topic or a resource pool, declared earlier in this desired schema",
		errUnmodeled, name)
}

// declaredSchemeObject is a grant on the scheme object the reading declares
// at relative, of a kind whose permissions the host's reader reads.
func (r *Reading) declaredSchemeObject(relative string) (Grant, bool) {
	objects := slices.Clone(r.Objects)
	sortObjects(objects)
	for _, object := range objects {
		index := slices.IndexFunc(grantObjectKinds, func(kind struct {
			objectKind string
			kind       ObjectKind
		}) bool {
			return kind.kind == object.Kind
		})
		if index < 0 {
			continue
		}
		if object.Kind == KindResourcePool {
			// A pool belongs to the database, and a grant names it by the
			// directory of its access entries; the host names the pool as
			// it is written.
			if path.Join(poolDirectory, object.Name) == relative {
				return Grant{OnObject: object.Name, ObjectKind: grantObjectKinds[index].objectKind,
					target: grantTarget{kind: strings.ToLower(grantObjectKinds[index].objectKind), name: object.Name}}, true
			}
			continue
		}
		if path.Join(object.Schema, object.Name) == relative {
			return Grant{OnObject: canonicalReference(object.Schema, object.Name), ObjectKind: grantObjectKinds[index].objectKind,
				target: grantTarget{kind: strings.ToLower(grantObjectKinds[index].objectKind), schema: object.Schema, name: object.Name}}, true
		}
	}
	return Grant{}, false
}

// declaresDirectory reports whether name is the directory of an object the
// reading declares, or a directory above one. The leaf object is not a
// directory.
func (r *Reading) declaresDirectory(name string) bool {
	var directories []string
	for _, table := range r.Tables {
		directories = append(directories, table.Schema)
	}
	for _, view := range r.Views {
		if ref, ok := parseTableRef(view.Name); ok {
			directories = append(directories, ref.schema)
		}
	}
	for _, object := range r.Objects {
		directories = append(directories, object.Schema)
	}
	return slices.ContainsFunc(directories, func(directory string) bool {
		return directory == name || strings.HasPrefix(directory, name+"/")
	})
}
