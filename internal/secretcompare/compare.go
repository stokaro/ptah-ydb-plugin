// Package secretcompare compares secret presence and explicit rotation requests.
package secretcompare

import (
	"context"
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the secret comparison owner.
const ServiceID = "ydb-secrets"

// Compare decides presence changes without reading values. A rotation request
// must name a declared secret; missing or unknown current state follows the
// same coverage rules as creation and removal.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	database, err := objectcompare.SessionDatabase(request, schema.SecretProperties)
	if err != nil {
		return nil, err
	}
	bound := func(id *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
		result := proto.CloneOf(id)
		if result.GetDatabase().GetNormalized() == database {
			result.Database = &protocol.ComparisonPart{}
		}
		return result
	}
	var rotations []*protocol.ComparisonIdentity
	var resolved []*protocol.ResolvedRequest
	for i, change := range request.Requests {
		if change.GetAction() != "rotate" {
			return nil, fmt.Errorf("unsupported secret change request")
		}
		subject := change.GetSubject()
		if change.GetWritten() != "" {
			subject, err = written(change.Written)
			if err != nil {
				return nil, err
			}
		}
		if _, err := schema.NativeBoundOwner(schema.SecretProperties, subject); err != nil {
			return nil, err
		}
		subject = bound(subject)
		var declared *protocol.ComparisonIdentity
		for _, record := range request.GetDesired().GetRecords() {
			if record.GetKind() == schema.SecretProperties && provider.CompareIdentities(bound(record.Subject), subject) == 0 {
				declared = record.Subject
				break
			}
		}
		if declared == nil {
			return nil, fmt.Errorf("rotation names a secret the desired schema does not declare")
		}
		rotations = append(rotations, subject)
		if change.Written != "" {
			resolved = append(resolved, &protocol.ResolvedRequest{Request: uint32(i), Subject: proto.CloneOf(declared)})
		}
	}
	family := objectcompare.Family{
		ServiceID: ServiceID, Kind: schema.SecretProperties, Capability: secrets.Capability, Noun: "secret",
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
