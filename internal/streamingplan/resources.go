package streamingplan

import (
	"fmt"
	"path"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func queryResources(request *protocol.PlanFeaturesRequest, subject *protocol.ComparisonIdentity, spec streaming.Spec, early bool, id *protocol.StepIdentity) ([]*protocol.PlanningEffect, []*protocol.PlanningDependency, error) {
	names, err := streaming.NamedPaths(spec.Text)
	if err != nil {
		return nil, nil, err
	}
	var effects []*protocol.PlanningEffect
	var dependencies []*protocol.PlanningDependency
	addRead := func(subject *protocol.ComparisonIdentity) {
		for _, effect := range effects {
			if provider.CompareIdentities(effect.Subject, subject) == 0 {
				return
			}
		}
		effects = append(effects, &protocol.PlanningEffect{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ})
	}
	addDependency := func(edge *protocol.PlanningDependency) {
		for _, known := range dependencies {
			if proto.Equal(edge, known) {
				return
			}
		}
		dependencies = append(dependencies, edge)
	}
	for _, name := range names {
		resource, err := queryPath(request.DatabasePath, subject, name)
		if err != nil {
			return nil, nil, err
		}
		// The captured request need not contain every native family. A scheme
		// slot states the physical dependency without inventing an object kind.
		addRead(resource)
		for _, table := range request.Tables {
			if provider.CompareIdentities(resource, objectplan.Slot(table.Subject)) != 0 {
				continue
			}
			if early && table.Current != nil || !early && table.Desired != nil {
				addRead(table.Subject)
			}
		}
		for _, common := range request.CommonSteps {
			writes, touches := false, false
			for _, effect := range common.GetMetadata().GetEffects() {
				writes = writes || effect.Action != protocol.PlanningEffect_READ
				touches = touches || provider.CompareIdentities(resource, objectplan.Slot(effect.Subject)) == 0
			}
			if !writes || !touches {
				continue
			}
			edge := &protocol.PlanningDependency{Before: proto.CloneOf(common.Id), After: proto.CloneOf(id)}
			if early {
				edge.Before, edge.After = edge.After, edge.Before
			}
			addDependency(edge)
		}
	}
	if pool := streaming.Pool(spec); pool != "default" {
		effects = append(effects, &protocol.PlanningEffect{Subject: &protocol.ComparisonIdentity{
			Kind: schema.ResourcePoolProperties, Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: &protocol.ComparisonPart{}, Parent: &protocol.ComparisonPart{},
			Name: &protocol.ComparisonPart{Source: pool, Normalized: pool},
		}, Action: protocol.PlanningEffect_READ})
	}
	return effects, dependencies, nil
}

func queryPath(database string, owner *protocol.ComparisonIdentity, name string) (*protocol.ComparisonIdentity, error) {
	if database == "" {
		database = owner.GetDatabase().GetNormalized()
	}
	if path.IsAbs(name) {
		if database == "" || !path.IsAbs(database) || path.Clean(database) != database || !strings.HasPrefix(name, strings.TrimSuffix(database, "/")+"/") {
			return nil, fmt.Errorf("streaming query path is outside the captured database or dev realm")
		}
		name = strings.TrimPrefix(name, strings.TrimSuffix(database, "/")+"/")
	}
	directory, leaf := path.Split(name)
	directory = strings.TrimSuffix(directory, "/")
	if err := streaming.ValidateIdentity(directory, leaf); err != nil {
		return nil, err
	}
	return objectplan.Slot(&protocol.ComparisonIdentity{
		Catalog: &protocol.ComparisonPart{}, Parent: &protocol.ComparisonPart{},
		Schema: &protocol.ComparisonPart{Source: directory, Normalized: directory}, Name: &protocol.ComparisonPart{Source: leaf, Normalized: leaf},
	}), nil
}
