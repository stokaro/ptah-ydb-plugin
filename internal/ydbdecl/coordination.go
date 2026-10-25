package ydbdecl

import (
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// The settings a coordination node declares.
const (
	SettingSelfCheckPeriod         = "self_check_period"
	SettingSessionGracePeriod      = "session_grace_period"
	SettingReadConsistencyMode     = "read_consistency_mode"
	SettingAttachConsistencyMode   = "attach_consistency_mode"
	SettingRateLimiterCountersMode = "rate_limiter_counters_mode"
)

// CoordinationSettings lists the settings in the order a declaration is read.
func CoordinationSettings() []string {
	return []string{
		SettingSelfCheckPeriod, SettingSessionGracePeriod, SettingReadConsistencyMode,
		SettingAttachConsistencyMode, SettingRateLimiterCountersMode,
	}
}

// LockNode is the coordination node at the database root that holds the
// host's own locks.
const LockNode = "ptah_locks"

const (
	consistencyStrict  = "strict"
	consistencyRelaxed = "relaxed"
	countersAggregated = "aggregated"
	countersDetailed   = "detailed"

	minSelfCheckPeriodMillis    = 500
	maxSelfCheckPeriodMillis    = 10_000
	minSessionGraceMarginMillis = 1_000
	maxSessionGracePeriodMillis = 30_000
)

// CoordinationNode is a coordination node's settings, as the host's spec
// encodes them.
type CoordinationNode struct {
	SelfCheckPeriodMillis    uint32 `json:"self_check_period_millis,omitempty"`
	SessionGracePeriodMillis uint32 `json:"session_grace_period_millis,omitempty"`
	ReadConsistencyMode      string `json:"read_consistency_mode,omitempty"`
	AttachConsistencyMode    string `json:"attach_consistency_mode,omitempty"`
	RateLimiterCountersMode  string `json:"rate_limiter_counters_mode,omitempty"`
}

type settingError struct{ message string }

func (e *settingError) Error() string { return e.message }

func newSettingError(format string, args ...any) error {
	return &settingError{message: fmt.Sprintf(format, args...)}
}

// ParseCoordinationNode reads a coordination node's settings.
func ParseCoordinationNode(values map[string]string) (CoordinationNode, error) {
	var spec CoordinationNode
	for _, setting := range CoordinationSettings() {
		value, present := values[setting]
		if !present {
			continue
		}
		if err := setCoordination(&spec, setting, value); err != nil {
			return CoordinationNode{}, err
		}
	}
	if err := ValidateCoordinationNode(spec); err != nil {
		return CoordinationNode{}, err
	}
	return spec, nil
}

func setCoordination(spec *CoordinationNode, setting, value string) error {
	if strings.TrimSpace(value) == "" {
		return newSettingError("%s is empty: leave the setting out to keep the node's default", setting)
	}
	switch setting {
	case SettingSelfCheckPeriod, SettingSessionGracePeriod:
		millis, err := parsePeriod(value)
		if err != nil {
			return newSettingError("%s %q: %v", setting, value, err)
		}
		if setting == SettingSelfCheckPeriod {
			spec.SelfCheckPeriodMillis = millis
		} else {
			spec.SessionGracePeriodMillis = millis
		}
	case SettingReadConsistencyMode:
		spec.ReadConsistencyMode = strings.ToLower(strings.TrimSpace(value))
	case SettingAttachConsistencyMode:
		spec.AttachConsistencyMode = strings.ToLower(strings.TrimSpace(value))
	default:
		spec.RateLimiterCountersMode = strings.ToLower(strings.TrimSpace(value))
	}
	return nil
}

func parsePeriod(text string) (uint32, error) {
	micros, ok := ydbtype.ParseInterval(text)
	switch {
	case !ok:
		return 0, fmt.Errorf("a period is an ISO 8601 duration such as PT1S or PT0.5S")
	case micros <= 0:
		return 0, fmt.Errorf("a period is longer than zero")
	case micros%1_000 != 0:
		return 0, fmt.Errorf("the node keeps a period in whole milliseconds")
	case micros/1_000 > math.MaxUint32:
		return 0, fmt.Errorf("the node keeps a period of at most %d milliseconds", uint32(math.MaxUint32))
	}
	return uint32(micros / 1_000), nil
}

func periodText(millis uint32) string {
	return ydbtype.IntervalText(int64(millis) * 1_000)
}

// ValidateCoordinationNode refuses settings the node would not keep as
// declared: a mode YDB does not take, and a period YDB moves into its range.
func ValidateCoordinationNode(spec CoordinationNode) error {
	for _, mode := range []struct{ setting, value string }{
		{SettingReadConsistencyMode, spec.ReadConsistencyMode},
		{SettingAttachConsistencyMode, spec.AttachConsistencyMode},
	} {
		if mode.value != "" && mode.value != consistencyStrict && mode.value != consistencyRelaxed {
			return newSettingError("%s %q: the mode is %q or %q", mode.setting, mode.value, consistencyStrict, consistencyRelaxed)
		}
	}
	if counters := spec.RateLimiterCountersMode; counters != "" && counters != countersAggregated && counters != countersDetailed {
		return newSettingError("%s %q: the mode is %q or %q", SettingRateLimiterCountersMode, counters, countersAggregated, countersDetailed)
	}
	check := spec.SelfCheckPeriodMillis
	if check == 0 {
		check = 1_000
	}
	grace := spec.SessionGracePeriodMillis
	if grace == 0 {
		grace = 10_000
	}
	if check < minSelfCheckPeriodMillis || check > maxSelfCheckPeriodMillis {
		return newSettingError("%s %s: YDB runs a node's self-check every %s to %s and "+
			"moves a period outside that range into it", SettingSelfCheckPeriod, periodText(check),
			periodText(minSelfCheckPeriodMillis), periodText(maxSelfCheckPeriodMillis))
	}
	least := check + minSessionGraceMarginMillis
	if grace < least || grace > maxSessionGracePeriodMillis {
		return newSettingError("%s %s: YDB runs a node with a grace period from the "+
			"self-check period plus %s (%s here) to %s and moves a period outside that range into it",
			SettingSessionGracePeriod, periodText(grace), periodText(minSessionGraceMarginMillis), periodText(least),
			periodText(maxSessionGracePeriodMillis))
	}
	return nil
}

// CoordinationIdentity reports whether a directory and a name are a node's
// path: a name without a slash, and a clean relative directory.
func CoordinationIdentity(schema, name string) error {
	if name == "" || strings.Contains(name, "/") || (schema != "" && (path.IsAbs(schema) || path.Clean(schema) != schema)) {
		return fmt.Errorf("%w: coordination node requires a schema-scoped YDB identity", errInvalidValue)
	}
	return nil
}

// RefuseCoordinationName refuses a node name that starts a segment with a
// dot, which the server owns, and the node the host keeps its locks in.
func RefuseCoordinationName(schema, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a coordination node needs a name")
	}
	written := strings.Trim(schema, "/") + "/" + name
	for segment := range strings.SplitSeq(strings.TrimPrefix(written, "/"), "/") {
		if strings.HasPrefix(segment, ".") {
			return fmt.Errorf("coordination node %s has the path segment %q; a name that starts with a dot "+
				"belongs to the server, and Ptah never writes one", strings.TrimPrefix(written, "/"), segment)
		}
	}
	if path.Clean(strings.TrimPrefix(written, "/")) == LockNode {
		return fmt.Errorf("coordination node %s at the database root holds Ptah's own locks, "+
			"and Ptah never creates, changes or drops it for a schema; name the node differently", LockNode)
	}
	return nil
}
