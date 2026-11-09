// Package secretcompare compares secret presence and explicit rotation requests.
package secretcompare

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ServiceID names the secret comparison owner.
const ServiceID = "ydb-secrets"

// Compare decides presence changes without reading values. A rotation request
// must name a declared secret; missing or unknown current state follows the
// same coverage rules as creation and removal.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	database, err := objectcompare.SessionDatabase(request)
	if err != nil {
		return nil, err
	}
	rotations, resolved, err := resolveRotations(request, database)
	if err != nil {
		return nil, err
	}
	family := objectcompare.Family{
		ServiceID: ServiceID, Kind: schema.SecretProperties, ChangeKind: schema.SecretChange, Capability: secrets.Capability, ChangeCapability: true, Noun: "secret", DottedLimits: true,
		// YDB drops a secret a data source reads by its path and keeps the
		// source, which then names a secret that is gone.
		RequiredRemoval: "is dropped while a declared object reads it by its path",
		Unread: func(capabilities map[string]bool) string {
			if capabilities[secrets.Capability] {
				return ""
			}
			return "The target's secrets capability is unavailable"
		},
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(schema.SecretProperties) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.SecretProperties, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize,
		Reserved:  func(*protocol.ComparisonIdentity) string { return "" },
		Unsettled: func(_, _ *protocol.Envelope) error { return nil },
		Equal: func(_, desired *protocol.Envelope) (bool, error) {
			for _, requested := range rotations {
				owner, err := schema.NativeBoundOwner(schema.SecretProperties, requested)
				if err != nil {
					return false, err
				}
				if proto.Equal(owner, desired.Owner) {
					return false, nil
				}
			}
			return true, nil
		},
		Change: schema.CaptureSecretChange,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			value, err := schema.ConvertNative(current, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			if err != nil {
				return nil, err
			}
			value.Owner = proto.CloneOf(owner)
			return value, nil
		},
	}
	result, err := objectcompare.Compare(ctx, request, family)
	if err != nil {
		return nil, err
	}
	result.ResolvedRequests = resolved
	return result, nil
}

// resolveRotations reads the rotation requests: the subject each rotates,
// bound to the session database, and a resolved request for each one written
// as a path, naming the declared secret. A request that is not a rotation, or
// one naming a secret the desired schema does not declare, refuses the
// comparison.
func resolveRotations(request *protocol.CompareModelsRequest, database string) ([]*protocol.ComparisonIdentity, []*protocol.ResolvedRequest, error) {
	var rotations []*protocol.ComparisonIdentity
	var resolved []*protocol.ResolvedRequest
	for i, change := range request.Requests {
		if change.GetAction() != "rotate" {
			return nil, nil, fmt.Errorf("unsupported secret change request")
		}
		subject := change.GetSubject()
		if change.GetWritten() != "" {
			var err error
			subject, err = written(change.Written)
			if err != nil {
				return nil, nil, err
			}
		}
		if _, err := schema.NativeBoundOwner(schema.SecretProperties, subject); err != nil {
			return nil, nil, err
		}
		subject = bound(database, subject)
		declared := declaredSecret(request, database, subject)
		if declared == nil {
			return nil, nil, fmt.Errorf("rotation names a secret the desired schema does not declare")
		}
		rotations = append(rotations, subject)
		if change.Written != "" {
			resolved = append(resolved, &protocol.ResolvedRequest{Request: uint32(i), Subject: proto.CloneOf(declared)})
		}
	}
	return rotations, resolved, nil
}

// bound copies id, clearing its database part when that part names the
// session database.
func bound(database string, id *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
	result := proto.CloneOf(id)
	if result.GetDatabase().GetNormalized() == database {
		result.Database = &protocol.ComparisonPart{}
	}
	return result
}

// declaredSecret returns the subject of the desired secret record that names
// subject once bound to the session database, or nil when none does.
func declaredSecret(request *protocol.CompareModelsRequest, database string, subject *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
	for _, record := range request.GetDesired().GetRecords() {
		if record.GetKind() == schema.SecretProperties && provider.CompareIdentities(bound(database, record.Subject), subject) == 0 {
			return record.Subject
		}
	}
	return nil
}

func written(value string) (*protocol.ComparisonIdentity, error) {
	value = strings.TrimSpace(value)
	directory, name := "", value
	if i := strings.LastIndexByte(value, '/'); i >= 0 {
		directory, name = value[:i], value[i+1:]
	}
	if err := secrets.ValidateIdentity(directory, name); err != nil {
		return nil, err
	}
	part := func(value string) *protocol.ComparisonPart {
		return &protocol.ComparisonPart{Source: value, Normalized: value}
	}
	return &protocol.ComparisonIdentity{Kind: schema.SecretProperties, Database: part(""), Catalog: part(""), Schema: part(directory), Parent: part(""), Name: part(name)}, nil
}
