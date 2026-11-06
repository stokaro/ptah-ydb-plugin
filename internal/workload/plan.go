package workload

import (
	"cmp"
	"fmt"
	"slices"
)

// PoolChange is the change of one resource pool. Before is the pool as the
// database holds it, nil for a creation; After is the declared pool, nil for
// a removal.
type PoolChange struct {
	Name          string
	Before, After *PoolSpec
}

// ClassifierChange is the change of one classifier. Before is the classifier
// as the database holds it, nil for a creation; After is the declared one, nil
// for a removal.
type ClassifierChange struct {
	Name          string
	Before, After *ClassifierSpec
}

// phase is where a statement runs among the statements of one workload
// change, the host's order: a removed classifier frees its rank and stops
// routing to a pool before the pool goes, and a classifier is moved or
// created after the pool it routes to exists.
type phase int

const (
	classifierRemoval phase = iota
	poolRemoval
	poolSettings
	classifierSettings
	classifierCreation
)

// ChangeError is a change Statements refuses, naming the object.
type ChangeError struct {
	// Classifier is true for a classifier and false for a pool.
	Classifier bool
	// Name is the object's name.
	Name string
	// Reason says why the change is refused.
	Reason string
}

func (e *ChangeError) Error() string {
	noun := "resource pool"
	if e.Classifier {
		noun = "resource pool classifier"
	}
	return fmt.Sprintf("%s %q: %s", noun, e.Name, e.Reason)
}

func poolError(name, reason string) error { return &ChangeError{Name: name, Reason: reason} }

func classifierError(name, reason string) error {
	return &ChangeError{Classifier: true, Name: name, Reason: reason}
}

type operation struct {
	phase phase
	name  string
	Operation
}

// Operation is one statement of the workload changes Statements carries out:
// the change of the one pool or classifier the statement makes, as that
// object's change alone, and the statement. Exactly one of Pool and
// Classifier is set.
type Operation struct {
	Pool       *PoolChange
	Classifier *ClassifierChange
	Statement  string
}

// Statements returns the statements that carry out the changes of the pools
// and the classifiers of one database, in the order they run: classifier
// removals, pool removals, pool creations and changes, classifier changes,
// classifier creations, each in name order.
//
// A classifier that moves to a rank another changed classifier holds is
// dropped and created again, since YDB keeps one classifier per rank and
// checks it at every statement (`Classifier with rank 20 already exists`):
// the drop frees the old rank before the other classifier moves into it. A
// removed classifier frees its rank before any classifier moves.
//
// A change that holds no difference, a change of the pool `default` that
// creates or drops it, and two classifiers on one rank on either side are
// refused with a *ChangeError naming the object.
func Statements(pools []PoolChange, classifiers []ClassifierChange) ([]string, error) {
	operations, err := Operations(pools, classifiers)
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(operations))
	for i, item := range operations {
		texts[i] = item.Statement
	}
	return texts, nil
}

// Operations is [Statements] with the change each statement makes, refused
// alike. The change of a classifier dropped and created again is two
// operations, its removal and its creation, so that the operations of a
// change can run apart: Statements of one operation's change alone is that
// operation's statement.
func Operations(pools []PoolChange, classifiers []ClassifierChange) ([]Operation, error) {
	var operations []operation
	pool := func(phase phase, change PoolChange, text string) {
		operations = append(operations, operation{phase, change.Name, Operation{Pool: &change, Statement: text}})
	}
	classifier := func(phase phase, change ClassifierChange, text string) {
		operations = append(operations, operation{phase, change.Name, Operation{Classifier: &change, Statement: text}})
	}
	for _, change := range pools {
		switch {
		case change.Before == nil && change.After == nil:
			return nil, poolError(change.Name, "the change has no operand")
		case change.Name == DefaultPool && (change.Before == nil || change.After == nil):
			return nil, poolError(change.Name, "the default pool cannot be created or dropped")
		case change.Before == nil:
			pool(poolSettings, change, CreatePoolStatement(change.Name, *change.After))
		case change.After == nil:
			pool(poolRemoval, change, DropPoolStatement(change.Name))
		default:
			text := AlterPoolStatement(change.Name, *change.After, *change.Before)
			if text == "" {
				return nil, poolError(change.Name, "the change holds no difference")
			}
			pool(poolSettings, change, text)
		}
	}
	held, err := heldRanks(classifiers)
	if err != nil {
		return nil, err
	}
	for _, change := range classifiers {
		switch {
		case change.Before == nil && change.After == nil:
			return nil, classifierError(change.Name, "the change has no operand")
		case change.Before == nil:
			classifier(classifierCreation, change, CreateClassifierStatement(change.Name, *change.After))
		case change.After == nil:
			classifier(classifierRemoval, change, DropClassifierStatement(change.Name))
		case *change.Before == *change.After:
			return nil, classifierError(change.Name, "the change holds no difference")
		case held[change.After.Rank] != "" && held[change.After.Rank] != change.Name:
			classifier(classifierRemoval, ClassifierChange{Name: change.Name, Before: change.Before}, DropClassifierStatement(change.Name))
			classifier(classifierCreation, ClassifierChange{Name: change.Name, After: change.After}, CreateClassifierStatement(change.Name, *change.After))
		default:
			classifier(classifierSettings, change, AlterClassifierStatement(change.Name, *change.After, *change.Before))
		}
	}
	slices.SortStableFunc(operations, func(a, b operation) int {
		return cmp.Or(cmp.Compare(a.phase, b.phase), cmp.Compare(a.name, b.name))
	})
	result := make([]Operation, len(operations))
	for i, item := range operations {
		result[i] = item.Operation
	}
	return result, nil
}

// heldRanks maps the rank each changed classifier holds before and keeps
// after the change to its name, and refuses two classifiers on one rank on
// either side of the changes.
func heldRanks(classifiers []ClassifierChange) (map[int64]string, error) {
	held := make(map[int64]string)
	before, after := make(map[int64]string), make(map[int64]string)
	for _, change := range classifiers {
		if change.Before != nil {
			if other, taken := before[change.Before.Rank]; taken {
				return nil, classifierError(change.Name, fmt.Sprintf("the database holds classifiers %q and %q on rank %d", other, change.Name, change.Before.Rank))
			}
			before[change.Before.Rank] = change.Name
		}
		if change.After != nil {
			if other, taken := after[change.After.Rank]; taken {
				return nil, classifierError(change.Name, fmt.Sprintf("classifiers %q and %q cannot share rank %d", other, change.Name, change.After.Rank))
			}
			after[change.After.Rank] = change.Name
		}
		if change.Before != nil && change.After != nil {
			held[change.Before.Rank] = change.Name
		}
	}
	return held, nil
}
