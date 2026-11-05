package objectplan

import (
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// directoryKind identifies a directory of the session's database as the
// subject of a step's effect: the place below a path where YDB keeps the
// objects of that path's directory.
const directoryKind = "ydb.scheme-directory"

// directorySubject is the directory of the session's database at directory
// schema and leaf name, as a step's effect names one.
func directorySubject(schema, name string) *protocol.ComparisonIdentity {
	subject := PathSubject(schema, name)
	subject.Kind = directoryKind
	return subject
}

// directoryEffects returns the effects that order the step of item against
// the steps of other planning services along its chain of directories. YDB
// needs every path above an object to be a directory. A drop leaves its path
// free for one, which the step records as the creation of the directory
// there, and a creation reads every directory above its path. The host runs a
// creation before another service's read of the same subject, so an object
// created below a path follows the drop of the object there whichever
// services plan the two. checkPathChanges orders the steps of one service,
// and the dependencies on common steps order those against the host's.
func directoryEffects(item change) []*protocol.PlanningEffect {
	subject := item.input.Subject
	switch item.action {
	case protocol.PlanningEffect_DROP:
		return []*protocol.PlanningEffect{{Subject: directorySubject(subject.GetSchema().GetNormalized(), subject.GetName().GetNormalized()),
			Action: protocol.PlanningEffect_CREATE}}
	case protocol.PlanningEffect_CREATE:
		var effects []*protocol.PlanningEffect
		parent := ""
		for component := range strings.SplitSeq(subject.GetSchema().GetNormalized(), "/") {
			if component == "" {
				continue
			}
			effects = append(effects, &protocol.PlanningEffect{Subject: directorySubject(parent, component), Action: protocol.PlanningEffect_READ})
			parent = strings.TrimPrefix(parent+"/"+component, "/")
		}
		return effects
	}
	return nil
}
