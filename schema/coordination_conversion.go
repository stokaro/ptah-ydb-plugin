package schema

import (
	"encoding/json"
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// ConvertCoordination preserves timing, modes, and access constraints offline.
// It cannot establish observation coverage or discard source-holder provenance.
func ConvertCoordination(value *protocol.Envelope, from, to protocol.ValueInput_Representation) (*protocol.Envelope, error) {
	if value.GetKind() != CoordinationProperties {
		return nil, fmt.Errorf("expected a YDB coordination model")
	}
	if from == protocol.ValueInput_OBSERVED && to == protocol.ValueInput_DESIRED {
		intent, err := CoordinationConfiguration(value, from)
		if err != nil {
			return nil, err
		}
		return CaptureCoordinationIntent(value.Owner, intent)
	}
	if from == protocol.ValueInput_DESIRED && to == protocol.ValueInput_OBSERVED {
		intent, err := CoordinationConfiguration(value, from)
		if err != nil {
			return nil, err
		}
		if intent.StructName != "" {
			return nil, fmt.Errorf("YDB source holder provenance has no observed representation")
		}
		config := &Ydb_Coordination.Config{}
		if intent.SelfCheckPeriodMillis != nil {
			config.SelfCheckPeriodMillis = *intent.SelfCheckPeriodMillis
		}
		if intent.SessionGracePeriodMillis != nil {
			config.SessionGracePeriodMillis = *intent.SessionGracePeriodMillis
		}
		mode := func(v *string) Ydb_Coordination.ConsistencyMode {
			if v == nil {
				return Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_UNSET
			}
			if *v == "strict" {
				return Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT
			}
			return Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED
		}
		config.ReadConsistencyMode, config.AttachConsistencyMode = mode(intent.ReadConsistencyMode), mode(intent.AttachConsistencyMode)
		if intent.RateLimiterCountersMode != nil {
			config.RateLimiterCountersMode = Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_AGGREGATED
			if *intent.RateLimiterCountersMode == "detailed" {
				config.RateLimiterCountersMode = Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED
			}
		}
		return convertedEnvelope(value, model(CoordinationProperties), &Ydb_Coordination.DescribeNodeResult{Self: accessObservation(intent.Access), Config: config}, Normalize)
	}
	return nil, fmt.Errorf("YDB coordination conversion requires distinct schema representations")
}

// ValidateCoordinationAccess checks requirements against established access.
// Coordination operations preserve access; creation cannot promise inheritance.
func ValidateCoordinationAccess(before, after *protocol.Envelope) error {
	if after == nil {
		return nil
	}
	desired, err := CoordinationConfiguration(after, protocol.ValueInput_DESIRED)
	if err != nil {
		return err
	}
	required := desired.Access
	if required == nil {
		return nil
	}
	if before == nil {
		return fmt.Errorf("coordination creation cannot establish requested access requirements")
	}
	observed, err := CoordinationConfiguration(before, protocol.ValueInput_OBSERVED)
	if err != nil {
		return err
	}
	actual := observed.Access
	if actual == nil || (required.Owner != nil && required.GetOwner() != actual.GetOwner()) || !permissionSetsEqual(required.DirectGrants, actual.DirectGrants) || !permissionSetsEqual(required.RequiredEffectivePermissions, actual.RequiredEffectivePermissions) {
		return fmt.Errorf("coordination access requirements differ from established access")
	}
	return nil
}

func permissionSetsEqual(a, b []*Ydb_Scheme.Permissions) bool {
	normalize := func(values []*Ydb_Scheme.Permissions) []string {
		var result []string
		for _, value := range values {
			names := slices.Clone(value.PermissionNames)
			slices.Sort(names)
			// Canonical JSON avoids ambiguity between subject and permission delimiters.
			encoded, err := json.Marshal(struct {
				Subject string
				Names   []string
			}{value.Subject, names})
			if err != nil {
				panic(err)
			}
			result = append(result, string(encoded))
		}
		slices.Sort(result)
		return result
	}
	return slices.Equal(normalize(a), normalize(b))
}
