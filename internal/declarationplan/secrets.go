package declarationplan

import (
	"fmt"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalpath"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

func sourceSecretEffects(object *protocol.ComparisonRecord) ([]*protocol.PlanningEffect, error) {
	if object.GetKind() != schema.ExternalSourceProperties {
		return nil, nil
	}
	value := object.GetValue().GetValue()
	intent, err := schema.DesiredExternalSource(value)
	if err != nil {
		return nil, err
	}
	spec, err := externals.SourceFromIntent(intent, schema.InterpretationRoot(value))
	if err != nil {
		return nil, err
	}
	paths, err := externalpath.SecretPaths(spec.Options, schema.InterpretationRoot(value))
	if err != nil {
		return nil, err
	}
	var effects []*protocol.PlanningEffect
	for _, path := range paths {
		if path.Schema == object.GetSubject().GetSchema().GetSource() && path.Name == object.GetSubject().GetName().GetSource() {
			return nil, fmt.Errorf("a data source cannot use its own path as a secret")
		}
		ref := proto.CloneOf(object.GetSubject())
		ref.Kind = schema.SecretProperties
		ref.Schema = &protocol.ComparisonPart{Source: path.Schema, Normalized: path.Schema}
		ref.Name = &protocol.ComparisonPart{Source: path.Name, Normalized: path.Name}
		effects = append(effects, &protocol.PlanningEffect{Subject: ref, Action: protocol.PlanningEffect_READ},
			&protocol.PlanningEffect{Subject: objectplan.Slot(ref), Action: protocol.PlanningEffect_READ})
	}
	return effects, nil
}
