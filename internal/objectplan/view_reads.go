package objectplan

import (
	"path"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
)

// createsViewReading reports whether common creates a view whose query names
// the scheme path subject of the database at root. YDB compiles a view's
// query when it creates the view and refuses one that reads a path nothing
// holds yet (SCHEME_ERROR, issue 2003, on 25.1.4.7 and 26.2.1.14), so a step
// that creates such a path, as an async replication creates its replica
// tables, has to come first. The host names no read of a view's query among
// the step's effects. A query whose paths cannot be named statically, or that
// names a path outside root, names none here.
func createsViewReading(root string, common *protocol.CommonPlanningStep, subject *protocol.ComparisonIdentity) bool {
	view := common.GetCommonObject().GetCreateView().GetView()
	if view == nil {
		return false
	}
	names, err := streaming.NamedPaths(view.GetBody())
	if err != nil {
		return false
	}
	for _, name := range names {
		if read, ok := localPath(root, name); ok && provider.CompareIdentities(read, subject) == 0 {
			return true
		}
	}
	return false
}

// localPath is the scheme path subject of name, a path a query names, in the
// database at root: relative to root, or absolute below it.
func localPath(root, name string) (*protocol.ComparisonIdentity, bool) {
	if path.IsAbs(name) {
		prefix := strings.TrimSuffix(root, "/") + "/"
		if root == "" || !strings.HasPrefix(name, prefix) {
			return nil, false
		}
		name = strings.TrimPrefix(name, prefix)
	}
	directory, leaf := path.Split(path.Clean(name))
	if leaf == "" || leaf == "." || strings.HasPrefix(directory, "../") {
		return nil, false
	}
	return PathSubject(strings.TrimSuffix(directory, "/"), leaf), true
}

// ReadByCommonStep reports whether a common step of request reads the scheme
// path of the session's database at directory and leaf name: one whose
// effects read the path, or one that creates a view whose query names it.
// sidePaths orders a step that creates the path before each such step, so
// the step's execution can tell whether the rest of the plan needs the path
// to stand once the step's statement returns.
func ReadByCommonStep(request *protocol.PlanFeaturesRequest, directory, name string) bool {
	subject := PathSubject(directory, name)
	for _, common := range request.GetCommonSteps() {
		if createsViewReading(request.GetDatabasePath(), common, subject) {
			return true
		}
		for _, effect := range common.GetMetadata().GetEffects() {
			if effect.GetAction() == protocol.PlanningEffect_READ && effect.GetSubject().GetParent().GetNormalized() == "" && isSchemeSubject(effect.GetSubject()) &&
				provider.CompareIdentities(slot(effect.Subject), subject) == 0 {
				return true
			}
		}
	}
	return false
}
