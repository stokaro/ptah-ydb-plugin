package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// CoordinationIntent preserves authored configuration. Missing settings request
// server defaults; normalization retains omission and does not resolve defaults.
// Period pointers distinguish an omitted setting from an invalid explicit zero.
type CoordinationIntent struct {
	// SelfCheckPeriodMillis is the declared self-check interval.
	SelfCheckPeriodMillis *uint32 `json:"self_check_period_millis,omitempty"`
	// SessionGracePeriodMillis is the declared grace interval on leader changes.
	SessionGracePeriodMillis *uint32 `json:"session_grace_period_millis,omitempty"`
	// ReadConsistencyMode selects strict or relaxed reads.
	ReadConsistencyMode *string `json:"read_consistency_mode,omitempty"`
	// AttachConsistencyMode selects strict or relaxed session attachment.
	AttachConsistencyMode *string `json:"attach_consistency_mode,omitempty"`
	// RateLimiterCountersMode selects aggregated or detailed counters.
	RateLimiterCountersMode *string `json:"rate_limiter_counters_mode,omitempty"`
	// StructName retains source provenance without a server counterpart.
	StructName string `json:"struct_name,omitempty"`
}

// CoordinationDesiredModel returns the complete desired configuration contract.
// Native access metadata is observed separately and is not an authored setting.
func CoordinationDesiredModel() *protocol.ValueModel { return proto.CloneOf(coordinationDesiredModel) }

var coordinationDesiredModel = buildCoordinationDesiredModel()

func buildCoordinationDesiredModel() *protocol.ValueModel {
	const definition = `{"kind":"` + CoordinationProperties + `","representation":"desired","schema_version":1,"encoding":"json",
"payload":{"type":"object","additionalProperties":false,"properties":{
"self_check_period_millis":{"type":"integer","minimum":500,"maximum":10000},
"session_grace_period_millis":{"type":"integer","minimum":1500,"maximum":30000},
"read_consistency_mode":{"enum":["strict","relaxed"]},"attach_consistency_mode":{"enum":["strict","relaxed"]},
"rate_limiter_counters_mode":{"enum":["aggregated","detailed"]},"struct_name":{"type":"string"}}},
"default_intent":"omitted settings request server defaults; normalization preserves omission",
"effective_defaults":{"self_check_period_millis":1000,"session_grace_period_millis":10000,"read_consistency_mode":"relaxed","attach_consistency_mode":"strict","rate_limiter_counters_mode":"aggregated"},
"cross_field_rule":"effective grace >= effective self-check + 1000 milliseconds; refuse server clamping",
"canonicalization":"strict JSON; no null, unknown or duplicate fields; uint32 decimal integers; no common identity in payload",
"source":{"format":"yaml","document_key":"coordination_nodes","selector":"coordination_nodes","shape":"mapping of entry keys to node settings; name defaults to entry key; schema is relative to database",
"text_fields":["name","schema","struct_name","self_check_period","session_grace_period","read_consistency_mode","attach_consistency_mode","rate_limiter_counters_mode"],
"periods":"positive ISO8601 week/day/hour/minute/second durations; at most six fractional second digits; exact whole milliseconds",
"modes":"trimmed and case-insensitive; empty or null values refused","unknown_fields":"refused"},
"identity":"exact YDB relative schema and leaf name; no catalog, parent path, overload or system path; root ptah_locks is reserved for runtime use",
"comparison":"canonical desired equality is representation-local; no desired-observed equivalence implied"}`
	canonical, err := modeljson.Canonical([]byte(definition))
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: CoordinationProperties, Representation: protocol.ValueInput_DESIRED, SchemaVersion: 1,
		Encoding: "json", DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]),
		CanonicalFormat: CoordinationProperties + "/desired-json-v1", SourceFormats: []protocol.SourceDocument_Format{protocol.SourceDocument_YAML}}
}

// CoordinationYAMLKey declares the document grammar owned by this model.
func CoordinationYAMLKey() *protocol.YAMLSourceKey {
	return &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: "coordination_nodes", Selector: "coordination_nodes", Models: []string{CoordinationProperties}}
}

// ValidateCoordinationIntent refuses settings YDB would silently clamp. Bounds
// and defaults are the native tablet's rules on qualified YDB 25.1 and 26.2.
func ValidateCoordinationIntent(value CoordinationIntent) error {
	check, grace := uint32(1000), uint32(10000)
	if value.SelfCheckPeriodMillis != nil {
		check = *value.SelfCheckPeriodMillis
	}
	if value.SessionGracePeriodMillis != nil {
		grace = *value.SessionGracePeriodMillis
	}
	if check < 500 || check > 10000 || grace < check+1000 || grace > 30000 {
		return fmt.Errorf("YDB coordination periods would be clamped by the server")
	}
	for _, mode := range []*string{value.ReadConsistencyMode, value.AttachConsistencyMode} {
		if mode != nil && *mode != "strict" && *mode != "relaxed" {
			return fmt.Errorf("invalid YDB coordination consistency mode")
		}
	}
	if mode := value.RateLimiterCountersMode; mode != nil && *mode != "aggregated" && *mode != "detailed" {
		return fmt.Errorf("invalid YDB coordination counter mode")
	}
	if !utf8.ValidString(value.StructName) {
		return fmt.Errorf("invalid YDB source holder name")
	}
	return nil
}

// ValidateCoordinationOwner checks exact native path components, including runtime nodes.
// Desired declarations apply their additional runtime-node policy separately.
func ValidateCoordinationOwner(owner *protocol.Reference) error {
	if owner.GetEngine() != "ydb" || owner.GetKind() != CoordinationProperties || owner.GetCatalog() != "" || len(owner.GetPath()) != 0 || len(owner.GetOverload()) != 0 {
		return fmt.Errorf("invalid desired YDB coordination identity")
	}
	if database := owner.GetDatabase(); database != "" && (!utf8.ValidString(database) || !path.IsAbs(database) || path.Clean(database) != database || strings.ContainsAny(database, "\\\x00")) {
		return fmt.Errorf("invalid desired YDB database path")
	}
	parts := []string{owner.GetName()}
	if namespace := owner.GetSchema(); namespace != "" {
		parts = append(parts, strings.Split(namespace, "/")...)
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || !utf8.ValidString(part) || strings.HasPrefix(part, ".") || strings.ContainsAny(part, "/\\\x00") {
			return fmt.Errorf("invalid or system-owned YDB coordination path")
		}
	}
	return nil
}

func desiredCoordinationOwner(owner *protocol.Reference) error {
	if err := ValidateCoordinationOwner(owner); err != nil {
		return err
	}
	if owner.GetSchema() == "" && owner.GetName() == "ptah_locks" {
		return fmt.Errorf("database-root ptah_locks is reserved for provider runtime use")
	}
	return nil
}

// NormalizeDesired validates a desired model independently of observed payloads.
// It neither opens a connection nor turns missing settings into stored defaults.
func NormalizeDesired(value *protocol.Envelope) (*protocol.Envelope, error) {
	if value.GetKind() == ColumnProperties {
		return normalizeColumnIntent(value)
	}
	model := CoordinationDesiredModel()
	if value.GetProviderId() != ProviderID || value.GetKind() != model.Kind || value.GetSchemaVersion() != model.SchemaVersion ||
		value.GetEncoding() != model.Encoding || value.GetDefinitionSha256() != model.DefinitionSha256 {
		return nil, fmt.Errorf("desired YDB value does not match its negotiated definition")
	}
	if err := desiredCoordinationOwner(value.GetOwner()); err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(value.GetPayload())
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("desired YDB coordination requires an object")
	}
	for key, raw := range fields {
		// encoding/json matches struct tags case-insensitively. The negotiated
		// grammar does not: aliases could overwrite another authored field.
		switch key {
		case "self_check_period_millis", "session_grace_period_millis", "read_consistency_mode", "attach_consistency_mode", "rate_limiter_counters_mode", "struct_name":
		default:
			return nil, fmt.Errorf("unknown desired YDB coordination field")
		}
		if bytes.Equal(raw, []byte("null")) {
			return nil, fmt.Errorf("null YDB coordination setting")
		}
	}
	var intent CoordinationIntent
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return nil, fmt.Errorf("invalid desired YDB coordination fields")
	}
	if err := ValidateCoordinationIntent(intent); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	canonical, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = canonical
	return result, nil
}

// CaptureCoordinationIntent captures a validated declaration under its model.
func CaptureCoordinationIntent(owner *protocol.Reference, intent CoordinationIntent) (*protocol.Envelope, error) {
	if err := ValidateCoordinationIntent(intent); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	model := CoordinationDesiredModel()
	return NormalizeDesired(&protocol.Envelope{ProviderId: ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Owner: proto.CloneOf(owner), Payload: payload})
}
