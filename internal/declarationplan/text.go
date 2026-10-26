package declarationplan

import (
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Text renders one authored CREATE after checking every target requirement.
func Text(value *protocol.Envelope, capabilities map[string]bool) (string, error) {
	desired, err := Declaration(value)
	if err != nil {
		return "", err
	}
	if transactional, known := capabilities["transactional_ddl"]; !known || transactional {
		return "", fmt.Errorf("authored YDB creation requires established nontransactional DDL")
	}
	owner := desired.Owner
	path := tablecreate.TextPath(owner.Schema, owner.Name)
	switch desired.Kind {
	case schema.SecretProperties:
		if !capabilities[secrets.Capability] {
			return "", fmt.Errorf("secret creation requires capability secrets")
		}
		intent, err := schema.DesiredSecret(desired)
		if err != nil {
			return "", err
		}
		return secrets.CreateStatement(owner.Schema, owner.Name, intent.GetValueEnv()), nil
	case schema.CoordinationProperties:
		if !capabilities["coordination_nodes"] {
			return "", fmt.Errorf("coordination creation requires capability coordination_nodes")
		}
		intent, err := schema.CoordinationConfiguration(desired, protocol.ValueInput_DESIRED)
		if err != nil {
			return "", err
		}
		if intent.Access != nil {
			return "", accessRefusal()
		}
		settings := ydbdecl.CoordinationNode{}
		if intent.SelfCheckPeriodMillis != nil {
			settings.SelfCheckPeriodMillis = *intent.SelfCheckPeriodMillis
		}
		if intent.SessionGracePeriodMillis != nil {
			settings.SessionGracePeriodMillis = *intent.SessionGracePeriodMillis
		}
		if intent.ReadConsistencyMode != nil {
			settings.ReadConsistencyMode = *intent.ReadConsistencyMode
		}
		if intent.AttachConsistencyMode != nil {
			settings.AttachConsistencyMode = *intent.AttachConsistencyMode
		}
		if intent.RateLimiterCountersMode != nil {
			settings.RateLimiterCountersMode = *intent.RateLimiterCountersMode
		}
		return (coordinationsql.Statement{Verb: coordinationsql.Create, Path: path, Settings: settings}).Text()
	case schema.TopicProperties:
		intent, err := schema.DesiredTopic(desired)
		if err != nil {
			return "", err
		}
		if intent.Access != nil {
			return "", accessRefusal()
		}
		spec, err := topics.FromIntent(intent)
		if err != nil {
			return "", err
		}
		if err := topics.Check(owner.Schema, owner.Name, spec, capabilities); err != nil {
			return "", err
		}
		return topics.CreateStatement(path, spec), nil
	case schema.ExternalSourceProperties:
		intent, err := schema.DesiredExternalSource(desired)
		if err != nil {
			return "", err
		}
		if intent.Access != nil {
			return "", accessRefusal()
		}
		spec, err := externals.SourceFromIntent(intent, schema.InterpretationRoot(desired))
		if err != nil {
			return "", err
		}
		if err := externals.CheckDataSource(path, spec, capabilities); err != nil {
			return "", err
		}
		return externals.CreateDataSourceStatement(path, spec, externals.Create), nil
	case schema.ExternalTableProperties:
		intent, err := schema.DesiredExternalTable(desired)
		if err != nil {
			return "", err
		}
		if intent.Access != nil {
			return "", accessRefusal()
		}
		spec, err := externals.TableFromIntent(intent, schema.InterpretationRoot(desired))
		if err != nil {
			return "", err
		}
		if err := externals.CheckTable(path, spec, capabilities); err != nil {
			return "", err
		}
		return externals.CreateTableStatement(path, spec, externals.Create), nil
	case schema.ResourcePoolProperties:
		if !capabilities[workload.CapabilityResourcePools] {
			return "", fmt.Errorf("resource pools require target capability resource_pools")
		}
		spec, err := schema.WorkloadPool(desired)
		if err != nil {
			return "", err
		}
		return workload.CreatePoolStatement(owner.Name, spec), nil
	case schema.ResourcePoolClassifierProperties:
		if !capabilities[workload.CapabilityResourcePools] {
			return "", fmt.Errorf("resource pool classifiers require target capability resource_pools")
		}
		spec, err := schema.WorkloadClassifier(desired)
		if err != nil {
			return "", err
		}
		return workload.CreateClassifierStatement(owner.Name, spec), nil
	default:
		return "", fmt.Errorf("unsupported authored object kind")
	}
}

func accessRefusal() error {
	return fmt.Errorf("authored CREATE cannot express explicit access requirements; use live schema planning")
}
