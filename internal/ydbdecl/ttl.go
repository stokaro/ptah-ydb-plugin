package ydbdecl

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/ttlgrammar"
)

// The units an integer TTL column may count in, as YQL spells them.
const (
	Seconds      = "SECONDS"
	Milliseconds = "MILLISECONDS"
	Microseconds = "MICROSECONDS"
	Nanoseconds  = "NANOSECONDS"
)

var units = []string{Seconds, Milliseconds, Microseconds, Nanoseconds}

// TTL is a row table's TTL that only deletes rows: the column its interval
// is measured from, the interval, and the unit an integer column counts in.
// It encodes as the host's TTL payload.
type TTL struct {
	Column   string `json:"column"`
	Interval string `json:"interval"`
	Unit     string `json:"unit,omitempty"`
}

// TTLUnit reads a declared unit in any case and returns it as YQL spells it.
// An empty unit stays empty.
func TTLUnit(declared string) (string, error) {
	trimmed := strings.TrimSpace(declared)
	if trimmed == "" {
		return "", nil
	}
	upper := strings.ToUpper(trimmed)
	for _, unit := range units {
		if upper == unit {
			return unit, nil
		}
	}
	return "", fmt.Errorf("unit %q is not one YDB takes: use %s", declared, strings.Join(units, ", "))
}

// IntervalSeconds reads an interval as YDB's Interval literal takes it and
// returns the whole number of seconds YDB keeps of it. A refusal is a
// *ttlgrammar.Error, whose reason says the same without the interval.
func IntervalSeconds(interval string) (uint64, error) {
	return ttlgrammar.IntervalSeconds(interval)
}

// formatInterval writes seconds as the shortest ISO 8601 duration of days,
// hours, minutes and seconds.
func formatInterval(seconds uint64) string {
	days := seconds / 86400
	rest := seconds % 86400
	hours, minutes, secs := rest/3600, rest%3600/60, rest%60
	var b strings.Builder
	b.WriteString("P")
	if days > 0 {
		b.WriteString(strconv.FormatUint(days, 10) + "D")
	}
	if rest == 0 {
		if days == 0 {
			return "PT0S"
		}
		return b.String()
	}
	b.WriteString("T")
	if hours > 0 {
		b.WriteString(strconv.FormatUint(hours, 10) + "H")
	}
	if minutes > 0 {
		b.WriteString(strconv.FormatUint(minutes, 10) + "M")
	}
	if secs > 0 {
		b.WriteString(strconv.FormatUint(secs, 10) + "S")
	}
	return b.String()
}

// ValidateTTL refuses a TTL YDB cannot hold, as the host refuses a desired
// TTL declaration. The unit is the one [TTLUnit] returned, so it is not read
// again here.
func ValidateTTL(policy TTL) error {
	return invalidModel(kindTTL, validateTTL(policy))
}

func validateTTL(p TTL) error {
	switch {
	case strings.TrimSpace(p.Column) == "":
		return fmt.Errorf("%w: a TTL needs the column its interval is measured from", errInvalidValue)
	case !utf8.ValidString(p.Column) || strings.ContainsRune(p.Column, '\x00'):
		return fmt.Errorf("%w: the TTL column is not valid UTF-8 without NUL", errInvalidValue)
	}
	if _, err := IntervalSeconds(p.Interval); err != nil {
		return fmt.Errorf("%w: %w", errInvalidValue, err)
	}
	return nil
}
