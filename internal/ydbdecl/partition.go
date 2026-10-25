package ydbdecl

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The attributes that declare the settings a table and a global index share,
// spelled as YDB spells the settings, in lower case.
const (
	AttributeBySize          = "auto_partitioning_by_size"
	AttributePartitionSizeMB = "auto_partitioning_partition_size_mb"
	AttributeByLoad          = "auto_partitioning_by_load"
	AttributeMinPartitions   = "auto_partitioning_min_partitions_count"
	AttributeMaxPartitions   = "auto_partitioning_max_partitions_count"
	AttributeReadReplicas    = "read_replicas_settings"
)

// The attributes only a table declaration reads, beside the shared ones.
const (
	AttributeKeyBloomFilter    = "key_bloom_filter"
	AttributeUniformPartitions = "uniform_partitions"
	AttributePartitionAtKeys   = "partition_at_keys"
)

// PartitioningAttributes lists the shared attributes in the order a
// declaration's errors are reported.
func PartitioningAttributes() []string {
	return []string{
		AttributeBySize, AttributePartitionSizeMB, AttributeByLoad,
		AttributeMinPartitions, AttributeMaxPartitions, AttributeReadReplicas,
	}
}

// TableAttributes lists every attribute a table declaration reads, in the
// order its errors are reported.
func TableAttributes() []string {
	return append(PartitioningAttributes(), AttributeKeyBloomFilter, AttributeUniformPartitions, AttributePartitionAtKeys)
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

// TablePartitioning is what a row table's declaration names of how it splits
// into partitions, its read replicas, its key bloom filter and the partitions
// it starts with. A nil switch, a zero count and empty text are settings it
// leaves out.
type TablePartitioning struct {
	BySize            *bool      `json:"by_size,omitempty"`
	PartitionSizeMB   uint64     `json:"partition_size_mb,omitempty"`
	ByLoad            *bool      `json:"by_load,omitempty"`
	MinPartitions     uint64     `json:"min_partitions,omitempty"`
	MaxPartitions     uint64     `json:"max_partitions,omitempty"`
	ReadReplicas      string     `json:"read_replicas,omitempty"`
	KeyBloomFilter    *bool      `json:"key_bloom_filter,omitempty"`
	UniformPartitions uint64     `json:"uniform_partitions,omitempty"`
	PartitionAtKeys   [][]string `json:"partition_at_keys,omitempty"`
}

// declared is what a declaration names of the settings a table and an index
// share.
type declared struct {
	bySize          *bool
	partitionSizeMB uint64
	byLoad          *bool
	minPartitions   uint64
	maxPartitions   uint64
	readReplicas    string
}

// parseDeclared reads the shared attributes out of values, keyed by attribute
// name, and ignores every other key. It reports whether any of them is
// present.
func parseDeclared(values map[string]string) (declared, bool, error) {
	var result declared
	present := false
	for _, attribute := range PartitioningAttributes() {
		value, ok := values[attribute]
		if !ok {
			continue
		}
		present = true
		if err := result.set(attribute, strings.TrimSpace(value)); err != nil {
			return declared{}, false, err
		}
	}
	return result, present, nil
}

func (d *declared) set(attribute, value string) error {
	switch attribute {
	case AttributeBySize:
		enabled, err := parseSwitch(attribute, value)
		d.bySize = enabled
		return err
	case AttributeByLoad:
		enabled, err := parseSwitch(attribute, value)
		d.byLoad = enabled
		return err
	case AttributePartitionSizeMB:
		return parseCount(attribute, value, &d.partitionSizeMB)
	case AttributeMinPartitions:
		return parseCount(attribute, value, &d.minPartitions)
	case AttributeMaxPartitions:
		return parseCount(attribute, value, &d.maxPartitions)
	default:
		replicas, err := parseReplicas(value)
		if err != nil {
			return &DeclarationError{Attribute: attribute, Value: value,
				Reason: "write PER_AZ:<n> for n replicas in every availability zone, or ANY_AZ:<n> for n in all of them together"}
		}
		d.readReplicas = replicas
		return nil
	}
}

// parseSwitch reads ENABLED or DISABLED, in either case.
func parseSwitch(attribute, value string) (*bool, error) {
	switch strings.ToUpper(value) {
	case "ENABLED":
		return new(true), nil
	case "DISABLED":
		return new(false), nil
	default:
		return nil, &DeclarationError{Attribute: attribute, Value: value, Reason: "write ENABLED or DISABLED"}
	}
}

// parseCount reads a count of at least 1 into target.
func parseCount(attribute, value string, target *uint64) error {
	count, err := strconv.ParseUint(value, 10, 64)
	if err != nil || count == 0 {
		return &DeclarationError{Attribute: attribute, Value: value,
			Reason: "write a whole number of at least 1; YDB refuses a count of zero"}
	}
	*target = count
	return nil
}

// parseReplicas reads READ_REPLICAS_SETTINGS as YDB takes it, `PER_AZ:<n>` or
// `ANY_AZ:<n>` in either case, and writes it back in capitals.
func parseReplicas(value string) (string, error) {
	mode, count, found := strings.Cut(strings.TrimSpace(value), ":")
	if !found {
		return "", errReplicas
	}
	perAZ := false
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "PER_AZ":
		perAZ = true
	case "ANY_AZ":
	default:
		return "", errReplicas
	}
	n, err := strconv.ParseUint(strings.TrimSpace(count), 10, 64)
	if err != nil {
		return "", errReplicas
	}
	written := "ANY_AZ"
	if perAZ {
		written = "PER_AZ"
	}
	return fmt.Sprintf("%s:%d", written, n), nil
}

var errReplicas = errors.New("read replicas are not one YDB takes")

// ParseTablePartitioning reads a table declaration's settings out of values,
// keyed by attribute name, and ignores every other key. It returns nil where
// none is present.
func ParseTablePartitioning(values map[string]string) (*TablePartitioning, error) {
	shared, present, err := parseDeclared(values)
	if err != nil {
		return nil, err
	}
	spec := &TablePartitioning{BySize: shared.bySize, PartitionSizeMB: shared.partitionSizeMB, ByLoad: shared.byLoad,
		MinPartitions: shared.minPartitions, MaxPartitions: shared.maxPartitions, ReadReplicas: shared.readReplicas}
	if value, ok := values[AttributeKeyBloomFilter]; ok {
		present = true
		if spec.KeyBloomFilter, err = parseSwitch(AttributeKeyBloomFilter, strings.TrimSpace(value)); err != nil {
			return nil, err
		}
	}
	if value, ok := values[AttributeUniformPartitions]; ok {
		present = true
		if err := parseCount(AttributeUniformPartitions, strings.TrimSpace(value), &spec.UniformPartitions); err != nil {
			return nil, err
		}
	}
	if value, ok := values[AttributePartitionAtKeys]; ok {
		present = true
		points, err := ParseSplitPoints(value)
		if err != nil {
			return nil, &DeclarationError{Attribute: AttributePartitionAtKeys, Value: value, Reason: err.Error()}
		}
		spec.PartitionAtKeys = points
	}
	if !present {
		return nil, nil
	}
	return spec, nil
}

// ParseSplitPoints reads split points as a declaration spells them: values
// separated by commas, a value with more than one key column in parentheses,
// a string in single or double quotes with a backslash escaping the next
// byte, and anything else up to a separator as written.
func ParseSplitPoints(text string) ([][]string, error) {
	p := splitPointParser{text: text}
	p.skipSpace()
	if p.done() {
		return nil, errors.New("name at least one split point, such as 10, 20 or (10, 'a'), (20)")
	}
	var points [][]string
	for {
		p.skipSpace()
		point, err := p.point()
		if err != nil {
			return nil, err
		}
		points = append(points, point)
		p.skipSpace()
		if p.done() {
			return points, nil
		}
		if !p.take(',') {
			return nil, fmt.Errorf("expected a comma after split point %d at offset %d", len(points), p.at)
		}
	}
}

type splitPointParser struct {
	text string
	at   int
}

func (p *splitPointParser) done() bool { return p.at >= len(p.text) }

func (p *splitPointParser) skipSpace() {
	for !p.done() && strings.ContainsRune(" \t\r\n", rune(p.text[p.at])) {
		p.at++
	}
}

func (p *splitPointParser) take(c byte) bool {
	if !p.done() && p.text[p.at] == c {
		p.at++
		return true
	}
	return false
}

func (p *splitPointParser) point() ([]string, error) {
	if !p.take('(') {
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		return []string{value}, nil
	}
	var values []string
	for {
		p.skipSpace()
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		p.skipSpace()
		if p.take(')') {
			return values, nil
		}
		if !p.take(',') {
			return nil, fmt.Errorf("expected a comma or a closing parenthesis at offset %d", p.at)
		}
	}
}

func (p *splitPointParser) value() (string, error) {
	if p.done() {
		return "", errors.New("a split point ends without a value")
	}
	if quote := p.text[p.at]; quote == '\'' || quote == '"' {
		p.at++
		var b strings.Builder
		for !p.done() {
			c := p.text[p.at]
			p.at++
			switch {
			case c == quote:
				return b.String(), nil
			case c == '\\' && !p.done():
				b.WriteByte(p.text[p.at])
				p.at++
			default:
				b.WriteByte(c)
			}
		}
		return "", fmt.Errorf("a string value has no closing %c", quote)
	}
	start := p.at
	for !p.done() && !strings.ContainsRune(" \t\r\n,()'\"", rune(p.text[p.at])) {
		p.at++
	}
	if p.at == start {
		return "", fmt.Errorf("expected a value at offset %d", start)
	}
	return p.text[start:p.at], nil
}

// FormatSplitPoints writes split points as [ParseSplitPoints] reads them.
func FormatSplitPoints(points [][]string) string {
	written := make([]string, len(points))
	for i, point := range points {
		values := make([]string, len(point))
		for j, value := range point {
			values[j] = formatValue(value)
		}
		written[i] = strings.Join(values, ", ")
		if len(point) != 1 {
			written[i] = "(" + written[i] + ")"
		}
	}
	return strings.Join(written, ", ")
}

// bareValue is a value [FormatSplitPoints] writes without quotes.
var bareValue = regexp.MustCompile(`^[0-9]+$`)

func formatValue(value string) string {
	if bareValue.MatchString(value) {
		return value
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}
