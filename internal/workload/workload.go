// Package workload reads, checks, compares and writes YDB's workload
// management objects as Ptah's embedded YDB engine does: resource pools,
// which limit the queries that run in them, and resource pool classifiers,
// which send a user's or a group's queries to a pool. Both belong to the whole
// database rather than to a directory.
//
// The rules and the words are ported from the host's dialect/ydb/ydbworkload.
// Every fact here was measured there on local-ydb 25.1.4.7 to 26.2.1.14 with
// the EnableResourcePools flag on, which is off by default on each of them.
package workload

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// CapabilityResourcePools is a target that keeps resource pools and
// classifiers: one whose EnableResourcePools flag is on.
const CapabilityResourcePools = "resource_pools"

// DefaultPool is the pool YDB creates for a database, which runs every query a
// classifier does not send elsewhere. Measured on 25.1.4.7 and 26.2.1.14:
// `DROP RESOURCE POOL default` is accepted, and every later query of the
// database, `CREATE RESOURCE POOL default` included, answers `Resource pool
// default not found or you don't have access permissions`. So Ptah changes its
// settings where a declaration names it and never creates or drops it.
const DefaultPool = "default"

// PoolSpec is a resource pool's settings: what `CREATE RESOURCE POOL <name>
// WITH (...)` takes. A nil field declares nothing, which YDB keeps as -1, its
// spelling of "no limit"; zero is a limit, since `CONCURRENT_QUERY_LIMIT = 0`
// runs no query at all. The percentages, DatabaseLoadCPUThreshold and
// ResourceWeight take a fraction; the other two are whole numbers.
type PoolSpec struct {
	ConcurrentQueryLimit           *int32   `json:"concurrent_query_limit,omitempty"`
	QueueSize                      *int32   `json:"queue_size,omitempty"`
	DatabaseLoadCPUThreshold       *float64 `json:"database_load_cpu_threshold,omitempty"`
	QueryMemoryLimitPercentPerNode *float64 `json:"query_memory_limit_percent_per_node,omitempty"`
	QueryCPULimitPercentPerNode    *float64 `json:"query_cpu_limit_percent_per_node,omitempty"`
	TotalCPULimitPercentPerNode    *float64 `json:"total_cpu_limit_percent_per_node,omitempty"`
	ResourceWeight                 *float64 `json:"resource_weight,omitempty"`
}

// ClassifierSpec is a resource pool classifier: what `CREATE RESOURCE POOL
// CLASSIFIER <name> WITH (...)` takes. Of the classifiers that match a query,
// the one with the lowest rank decides.
type ClassifierSpec struct {
	// ResourcePool is the pool the classifier sends queries to. YDB does not
	// check that it exists.
	ResourcePool string `json:"resource_pool"`
	// MemberName is the user or group whose queries the classifier matches,
	// and empty for every query.
	MemberName string `json:"member_name,omitempty"`
	// Rank is the classifier's order among the classifiers of the database.
	// No two classifiers share a rank.
	Rank int64 `json:"rank"`
}

// The attributes a resource pool declaration takes: the pool's name and the
// WITH options of CREATE RESOURCE POOL, spelled as YDB spells them in lower
// case.
const (
	AttributeName                           = "name"
	AttributeConcurrentQueryLimit           = "concurrent_query_limit"
	AttributeQueueSize                      = "queue_size"
	AttributeDatabaseLoadCPUThreshold       = "database_load_cpu_threshold"
	AttributeQueryMemoryLimitPercentPerNode = "query_memory_limit_percent_per_node"
	AttributeQueryCPULimitPercentPerNode    = "query_cpu_limit_percent_per_node"
	AttributeTotalCPULimitPercentPerNode    = "total_cpu_limit_percent_per_node"
	AttributeResourceWeight                 = "resource_weight"
)

// The attributes a classifier declaration takes besides its name.
const (
	AttributeResourcePool = "resource_pool"
	AttributeMemberName   = "member_name"
	AttributeRank         = "rank"
)

// PoolAttributes returns the attributes a resource pool declaration takes, in
// the order a statement names them.
func PoolAttributes() []string {
	return []string{AttributeName, AttributeConcurrentQueryLimit, AttributeQueueSize, AttributeDatabaseLoadCPUThreshold,
		AttributeQueryMemoryLimitPercentPerNode, AttributeQueryCPULimitPercentPerNode, AttributeTotalCPULimitPercentPerNode, AttributeResourceWeight}
}

// ClassifierAttributes returns the attributes a classifier declaration takes.
func ClassifierAttributes() []string {
	return []string{AttributeName, AttributeResourcePool, AttributeMemberName, AttributeRank}
}

// DeclarationError is an attribute whose value a declaration cannot carry.
type DeclarationError struct {
	// Attribute is the attribute's name.
	Attribute string
	// Value is the value it was given.
	Value string
	// Reason says what the attribute takes.
	Reason string
}

func (e *DeclarationError) Error() string {
	return fmt.Sprintf("invalid %s %q: %s", e.Attribute, e.Value, e.Reason)
}

// ParsePool reads one resource pool out of values, keyed by attribute name,
// and ignores every key it does not name.
//
// A whole-number setting takes a whole number from 0, and a percentage a
// number from 0 to 100 with a fraction allowed, the ranges YDB keeps. YDB
// spells "no limit" -1, which YQL can write only as a string; a declaration
// leaves the attribute out instead, so a negative number is refused. A queue
// needs a limit to wait for (`queue_size unsupported without
// concurrent_query_limit or database_load_cpu_threshold`), and the pool
// `default` takes neither of those.
func ParsePool(values map[string]string) (string, PoolSpec, error) {
	name := strings.TrimSpace(values[AttributeName])
	if err := checkName("a resource pool", name); err != nil {
		return "", PoolSpec{}, err
	}
	var spec PoolSpec
	var err error
	if spec.ConcurrentQueryLimit, err = count(values, AttributeConcurrentQueryLimit); err != nil {
		return "", PoolSpec{}, err
	}
	if spec.QueueSize, err = count(values, AttributeQueueSize); err != nil {
		return "", PoolSpec{}, err
	}
	for _, percentage := range []struct {
		attribute string
		target    **float64
	}{
		{AttributeDatabaseLoadCPUThreshold, &spec.DatabaseLoadCPUThreshold},
		{AttributeQueryMemoryLimitPercentPerNode, &spec.QueryMemoryLimitPercentPerNode},
		{AttributeQueryCPULimitPercentPerNode, &spec.QueryCPULimitPercentPerNode},
		{AttributeTotalCPULimitPercentPerNode, &spec.TotalCPULimitPercentPerNode},
		{AttributeResourceWeight, &spec.ResourceWeight},
	} {
		if *percentage.target, err = percent(values, percentage.attribute); err != nil {
			return "", PoolSpec{}, err
		}
	}
	if attribute, reason := defaultPoolRefusal(name, spec); reason != "" {
		return "", PoolSpec{}, &DeclarationError{Attribute: attribute, Value: values[attribute], Reason: reason}
	}
	if reason := poolShapeRefusal(spec); reason != "" {
		return "", PoolSpec{}, &DeclarationError{Attribute: AttributeQueueSize, Value: values[AttributeQueueSize], Reason: reason}
	}
	return name, spec, nil
}

// ParseClassifier reads one classifier out of values, keyed by attribute name,
// and ignores every key it does not name.
//
// The pool is required, as YDB requires it (`Missing required property
// resource_pool`). The rank is required too, although YDB takes a classifier
// without one: it then ranks it after every classifier the database holds, so
// the rank would depend on the database rather than on the declaration.
func ParseClassifier(values map[string]string) (string, ClassifierSpec, error) {
	name := strings.TrimSpace(values[AttributeName])
	if err := checkName("a resource pool classifier", name); err != nil {
		return "", ClassifierSpec{}, err
	}
	spec := ClassifierSpec{ResourcePool: strings.TrimSpace(values[AttributeResourcePool]), MemberName: strings.TrimSpace(values[AttributeMemberName])}
	if spec.ResourcePool == "" {
		return "", ClassifierSpec{}, &DeclarationError{Attribute: AttributeResourcePool,
			Reason: "a classifier names the pool it sends queries to (`Missing required property resource_pool`)"}
	}
	raw, ok := values[AttributeRank]
	if !ok || strings.TrimSpace(raw) == "" {
		return "", ClassifierSpec{}, &DeclarationError{Attribute: AttributeRank,
			Reason: "a classifier needs a rank; YDB ranks one declared without it after every classifier the " +
				"database holds, so the rank would depend on the database rather than on the declaration"}
	}
	rank, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || rank < 0 {
		return "", ClassifierSpec{}, &DeclarationError{Attribute: AttributeRank, Value: raw, Reason: "takes a whole number from 0"}
	}
	spec.Rank = rank
	return name, spec, nil
}

// count reads a whole-number setting, nil when values does not name it.
func count(values map[string]string, attribute string) (*int32, error) {
	raw, ok := values[attribute]
	if !ok {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil || parsed < 0 {
		return nil, &DeclarationError{Attribute: attribute, Value: raw, Reason: "takes a whole number from 0; leave it out for no limit"}
	}
	return new(int32(parsed)), nil
}

// percent reads a percentage setting, nil when values does not name it.
func percent(values map[string]string, attribute string) (*float64, error) {
	raw, ok := values[attribute]
	if !ok {
		return nil, nil
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(parsed) || parsed < 0 || parsed > 100 {
		return nil, &DeclarationError{Attribute: attribute, Value: raw, Reason: "takes a percentage from 0 to 100; leave it out for no limit"}
	}
	return new(parsed), nil
}

// checkName refuses a name YDB does not take, measured on both lines: an empty
// one, one holding a slash, and one holding a space or a character outside
// ASCII. A quote and a backtick are taken, and the statements escape them.
func checkName(what, name string) error {
	if name == "" {
		return &DeclarationError{Attribute: AttributeName, Reason: what + " needs a name"}
	}
	for _, r := range name {
		if r == '/' || r <= ' ' || r > '~' {
			return &DeclarationError{Attribute: AttributeName, Value: name,
				Reason: "a name holds printable ASCII characters other than a slash and a space, as YDB requires"}
		}
	}
	return nil
}

// ValidName reports whether YDB takes name as a pool's or a classifier's.
func ValidName(name string) bool { return checkName("an object", name) == nil }

// defaultPoolRefusal names the setting the pool `default` cannot take and
// says why, or returns "" for another pool and for a default that names
// neither (`Can not change property concurrent_query_limit for default
// pool`, and the same for the threshold).
func defaultPoolRefusal(name string, spec PoolSpec) (attribute, reason string) {
	if name != DefaultPool {
		return "", ""
	}
	for _, setting := range []struct {
		attribute string
		set       bool
	}{
		{AttributeConcurrentQueryLimit, spec.ConcurrentQueryLimit != nil},
		{AttributeDatabaseLoadCPUThreshold, spec.DatabaseLoadCPUThreshold != nil},
	} {
		if setting.set {
			return setting.attribute, fmt.Sprintf("the pool %s takes no %s: YDB keeps it unlimited "+
				"(`Can not change property %s for default pool`)", DefaultPool, setting.attribute, setting.attribute)
		}
	}
	return "", ""
}

// poolShapeRefusal says why YDB refuses a pool on every line, or "".
func poolShapeRefusal(spec PoolSpec) string {
	if spec.QueueSize != nil && spec.ConcurrentQueryLimit == nil && spec.DatabaseLoadCPUThreshold == nil {
		return "a queue needs concurrent_query_limit or database_load_cpu_threshold beside it " +
			"(`queue_size unsupported without concurrent_query_limit or database_load_cpu_threshold`)"
	}
	return ""
}

// FlagHint is what a refusal through the resource_pools capability adds: every
// release line keeps the feature off by default, and the cluster's own flags
// are what turns it on.
const FlagHint = "YDB keeps resource pools behind its EnableResourcePools feature flag, off by default: " +
	"turn the flag on, and name the cluster's monitoring endpoint in the URL " +
	"(monitoring=http://host:8765) so Ptah reads the flags before it plans"

// ValidatePool checks settings independently of a name or target capability.
func ValidatePool(name string, spec PoolSpec) error {
	if err := checkName("a resource pool", name); err != nil {
		return err
	}
	if reason := poolValueRefusal(spec); reason != "" {
		return fmt.Errorf("resource pool %q: %s", name, reason)
	}
	if reason := poolShapeRefusal(spec); reason != "" {
		return fmt.Errorf("resource pool %q: %s", name, reason)
	}
	if _, reason := defaultPoolRefusal(name, spec); reason != "" {
		return fmt.Errorf("resource pool %q: %s", name, reason)
	}
	return nil
}

// ValidateClassifier checks a classifier's stored settings.
func ValidateClassifier(name string, spec ClassifierSpec) error {
	if err := checkName("a resource pool classifier", name); err != nil {
		return err
	}
	if spec.ResourcePool == "" || spec.Rank < 0 {
		return fmt.Errorf("resource pool classifier %q requires a pool and a nonnegative rank", name)
	}
	return nil
}

// poolValueRefusal says which setting of spec is out of the range YDB keeps,
// or "".
func poolValueRefusal(spec PoolSpec) string {
	for _, setting := range settingsOf(spec) {
		switch {
		case setting.integer != nil && *setting.integer < 0:
			return setting.option + " takes a whole number from 0"
		case setting.fraction != nil && (math.IsNaN(*setting.fraction) || *setting.fraction < 0 || *setting.fraction > 100):
			return setting.option + " takes a percentage from 0 to 100"
		}
	}
	return ""
}

// setting is one WITH option of a pool and its value, nil when unset.
type setting struct {
	option   string
	integer  *int32
	fraction *float64
}

// settingsOf lists spec's options in the order a statement names them.
func settingsOf(spec PoolSpec) []setting {
	return []setting{
		{option: "CONCURRENT_QUERY_LIMIT", integer: spec.ConcurrentQueryLimit},
		{option: "QUEUE_SIZE", integer: spec.QueueSize},
		{option: "DATABASE_LOAD_CPU_THRESHOLD", fraction: spec.DatabaseLoadCPUThreshold},
		{option: "QUERY_MEMORY_LIMIT_PERCENT_PER_NODE", fraction: spec.QueryMemoryLimitPercentPerNode},
		{option: "QUERY_CPU_LIMIT_PERCENT_PER_NODE", fraction: spec.QueryCPULimitPercentPerNode},
		{option: "TOTAL_CPU_LIMIT_PERCENT_PER_NODE", fraction: spec.TotalCPULimitPercentPerNode},
		{option: "RESOURCE_WEIGHT", fraction: spec.ResourceWeight},
	}
}

func (s setting) set() bool { return s.integer != nil || s.fraction != nil }

// literal writes the setting's value as YQL takes it. A WITH option of a pool
// takes an integer or a string literal and nothing else, and the string
// `"80.5"` is read as the number, measured on every line: so a whole number is
// written bare and a fraction as a string.
func (s setting) literal() string {
	if s.integer != nil {
		return strconv.FormatInt(int64(*s.integer), 10)
	}
	text := strconv.FormatFloat(*s.fraction, 'f', -1, 64)
	if strings.ContainsAny(text, ".eE") {
		return ydbtype.StringLiteral(text)
	}
	return text
}

func (s setting) equal(other setting) bool {
	switch {
	case s.set() != other.set():
		return false
	case !s.set():
		return true
	case s.integer != nil:
		return other.integer != nil && *s.integer == *other.integer
	default:
		return other.fraction != nil && *s.fraction == *other.fraction
	}
}

// PoolsEqual reports whether two pool specs hold the same settings. An unset
// setting and a zero one differ.
func PoolsEqual(desired, current PoolSpec) bool {
	left, right := settingsOf(desired), settingsOf(current)
	for i := range left {
		if !left[i].equal(right[i]) {
			return false
		}
	}
	return true
}

// unsetLimit is how a pool with no setting is created. `CREATE RESOURCE POOL`
// needs a WITH clause and an option in it, YQL writes no negative number in
// one, and the string `"-1"` is read as -1, YDB's own spelling of no limit.
const unsetLimit = `CONCURRENT_QUERY_LIMIT = "-1"`

// CreatePoolStatement is the CREATE RESOURCE POOL that creates the pool name
// with spec. A setting spec leaves unset is not named.
func CreatePoolStatement(name string, spec PoolSpec) string {
	assignments := poolAssignments(spec)
	if len(assignments) == 0 {
		assignments = append(assignments, unsetLimit)
	}
	return "CREATE RESOURCE POOL " + ydbtype.QuoteIdentifier(name) + " WITH (" + strings.Join(assignments, ", ") + ");"
}

func poolAssignments(spec PoolSpec) []string {
	var assignments []string
	for _, s := range settingsOf(spec) {
		if s.set() {
			assignments = append(assignments, s.option+" = "+s.literal())
		}
	}
	return assignments
}

// AlterPoolStatement is the ALTER RESOURCE POOL that moves the pool name from
// current to desired, or "" when they hold the same settings. It sets each
// setting desired holds that differs and resets each one only current holds,
// in one statement: YDB checks the pool a statement leaves behind as a whole,
// and refuses a statement that sets and resets one option.
func AlterPoolStatement(name string, desired, current PoolSpec) string {
	wanted, held := settingsOf(desired), settingsOf(current)
	var sets, resets []string
	for i := range wanted {
		switch {
		case wanted[i].equal(held[i]):
		case wanted[i].set():
			sets = append(sets, wanted[i].option+" = "+wanted[i].literal())
		default:
			resets = append(resets, wanted[i].option)
		}
	}
	var clauses []string
	if len(sets) > 0 {
		clauses = append(clauses, "SET ("+strings.Join(sets, ", ")+")")
	}
	if len(resets) > 0 {
		clauses = append(clauses, "RESET ("+strings.Join(resets, ", ")+")")
	}
	if len(clauses) == 0 {
		return ""
	}
	return "ALTER RESOURCE POOL " + ydbtype.QuoteIdentifier(name) + " " + strings.Join(clauses, ", ") + ";"
}

// SetPoolStatement writes only the settings spec holds, without resetting the
// others: the statement for the pool `default`, whose settings a declaration
// states and which is never created. Empty settings return "".
func SetPoolStatement(name string, spec PoolSpec) string {
	assignments := poolAssignments(spec)
	if len(assignments) == 0 {
		return ""
	}
	return "ALTER RESOURCE POOL " + ydbtype.QuoteIdentifier(name) + " SET (" + strings.Join(assignments, ", ") + ");"
}

// DropPoolStatement is the DROP RESOURCE POOL that drops the pool name. The
// queries a classifier sends to a dropped pool run in the pool `default`.
func DropPoolStatement(name string) string {
	return "DROP RESOURCE POOL " + ydbtype.QuoteIdentifier(name) + ";"
}

// CreateClassifierStatement is the CREATE RESOURCE POOL CLASSIFIER that
// creates the classifier name with spec.
func CreateClassifierStatement(name string, spec ClassifierSpec) string {
	assignments := []string{"RESOURCE_POOL = " + ydbtype.StringLiteral(spec.ResourcePool), "RANK = " + strconv.FormatInt(spec.Rank, 10)}
	if spec.MemberName != "" {
		assignments = append(assignments, "MEMBER_NAME = "+ydbtype.StringLiteral(spec.MemberName))
	}
	return "CREATE RESOURCE POOL CLASSIFIER " + ydbtype.QuoteIdentifier(name) + " WITH (" + strings.Join(assignments, ", ") + ");"
}

// AlterClassifierStatement is the ALTER RESOURCE POOL CLASSIFIER that moves
// the classifier name from current to desired, or "" when they are the same.
// It names the pool and the rank whether or not they change, and the member,
// or `RESET (MEMBER_NAME)` when desired stops naming one, so the statement
// says the whole classifier.
func AlterClassifierStatement(name string, desired, current ClassifierSpec) string {
	if desired == current {
		return ""
	}
	sets := []string{"RESOURCE_POOL = " + ydbtype.StringLiteral(desired.ResourcePool), "RANK = " + strconv.FormatInt(desired.Rank, 10)}
	if desired.MemberName != "" {
		sets = append(sets, "MEMBER_NAME = "+ydbtype.StringLiteral(desired.MemberName))
	}
	statement := "ALTER RESOURCE POOL CLASSIFIER " + ydbtype.QuoteIdentifier(name) + " SET (" + strings.Join(sets, ", ") + ")"
	if desired.MemberName == "" && current.MemberName != "" {
		statement += ", RESET (MEMBER_NAME)"
	}
	return statement + ";"
}

// DropClassifierStatement is the DROP RESOURCE POOL CLASSIFIER that drops the
// classifier name.
func DropClassifierStatement(name string) string {
	return "DROP RESOURCE POOL CLASSIFIER " + ydbtype.QuoteIdentifier(name) + ";"
}
