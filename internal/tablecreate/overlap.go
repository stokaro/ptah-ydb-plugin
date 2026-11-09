package tablecreate

import (
	"cmp"
	"slices"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
)

// overlapIndex holds what disjoint reads of each table of a request, read
// once. Asking disjoint of every other table for each creation bound both
// owners again for every pair: 250,000 pairs and 0.7 s of CPU in a request of
// 500 new tables, growing with the square of the tables.
type overlapIndex struct {
	tables []*protocol.PlanningTable
	// unbound are the tables whose subject binds to no owner, in request
	// order.
	unbound []int
	// rootless and rooted are the bound tables whose database binding is
	// empty and set, in request order.
	rootless, rooted []int
	// paths are the bound tables by database and path, each list in request
	// order, and sorted is every bound table ordered by database, path and
	// request order, which holds the tables below a path together.
	paths  map[boundPath][]int
	sorted []boundEntry
}

// boundPath is the database binding of a bound table and its path there, as
// disjoint compares them.
type boundPath struct{ database, path string }

// boundEntry is one bound table of the request at its path.
type boundEntry struct {
	at    boundPath
	table int
}

func newOverlapIndex(tables []*protocol.PlanningTable) *overlapIndex {
	index := &overlapIndex{tables: tables, paths: make(map[boundPath][]int)}
	for i, table := range tables {
		owner, err := boundOwner(table.GetSubject())
		if err != nil {
			index.unbound = append(index.unbound, i)
			continue
		}
		if owner.Database == "" {
			index.rootless = append(index.rootless, i)
		} else {
			index.rooted = append(index.rooted, i)
		}
		at := boundPath{database: owner.Database, path: ownerPath(owner)}
		index.paths[at] = append(index.paths[at], i)
		index.sorted = append(index.sorted, boundEntry{at: at, table: i})
	}
	slices.SortFunc(index.sorted, func(a, b boundEntry) int {
		return cmp.Or(strings.Compare(a.at.database, b.at.database), strings.Compare(a.at.path, b.at.path), cmp.Compare(a.table, b.table))
	})
	return index
}

// ownerPath is the path of a bound owner within its database, as disjoint
// compares it.
func ownerPath(owner *protocol.Reference) string {
	if owner.Schema != "" {
		return owner.Schema + "/" + owner.Name
	}
	return owner.Name
}

// firstConflict returns the first table of the request, in request order,
// that is not subject and that disjoint refuses beside subject, or -1. It
// answers as asking disjoint of each table in turn would.
func (x *overlapIndex) firstConflict(subject *protocol.ComparisonIdentity) int {
	first := -1
	consider := func(i int) {
		if (first < 0 || i < first) && provider.CompareIdentities(x.tables[i].GetSubject(), subject) != 0 {
			first = i
		}
	}
	owner, err := boundOwner(subject)
	if err != nil {
		// disjoint refuses every pair with an unbound subject.
		for i := range x.tables {
			consider(i)
			if first >= 0 {
				break
			}
		}
		return first
	}
	for _, list := range [][]int{x.unbound, x.opposite(owner)} {
		for _, i := range list {
			consider(i)
		}
	}
	at := boundPath{database: owner.Database, path: ownerPath(owner)}
	for _, i := range x.paths[at] {
		consider(i)
	}
	// The tables below the path, and the tables the path is below.
	below := boundPath{database: at.database, path: at.path + "/"}
	start, _ := slices.BinarySearchFunc(x.sorted, below, func(entry boundEntry, target boundPath) int {
		return cmp.Or(strings.Compare(entry.at.database, target.database), strings.Compare(entry.at.path, target.path))
	})
	for _, entry := range x.sorted[start:] {
		if entry.at.database != at.database || !strings.HasPrefix(entry.at.path, below.path) {
			break
		}
		consider(entry.table)
	}
	for cut := strings.Index(at.path, "/"); cut >= 0; cut = nextSlash(at.path, cut) {
		for _, i := range x.paths[boundPath{database: at.database, path: at.path[:cut]}] {
			consider(i)
		}
	}
	return first
}

// opposite is the bound tables whose database binding disjoint refuses
// beside owner's: the set ones for an empty binding, the empty ones for a set
// binding.
func (x *overlapIndex) opposite(owner *protocol.Reference) []int {
	if owner.Database == "" {
		return x.rooted
	}
	return x.rootless
}

// nextSlash is the position of the slash in path after the one at cut, or -1.
func nextSlash(path string, cut int) int {
	next := strings.Index(path[cut+1:], "/")
	if next < 0 {
		return -1
	}
	return cut + 1 + next
}
