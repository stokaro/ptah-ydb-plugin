package ydbdecl

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The units an integer TTL column may count in, as YQL spells them.
const (
	Seconds      = "SECONDS"
	Milliseconds = "MILLISECONDS"
	Microseconds = "MICROSECONDS"
	Nanoseconds  = "NANOSECONDS"
)

var units = []string{Seconds, Milliseconds, Microseconds, Nanoseconds}

// maxSeconds is the longest interval YDB's Interval type holds.
const maxSeconds = 4291747199

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
// returns the whole number of seconds YDB keeps of it.
func IntervalSeconds(interval string) (uint64, error) {
	parsed, err := parseDuration(interval)
	if err != nil {
		return 0, fmt.Errorf("interval %q is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): %w",
			interval, err)
	}
	if parsed.fraction {
		return 0, fmt.Errorf("interval %q has a fraction of a second, and YDB keeps whole seconds only "+
			"(PT1.5S reads back as PT1S)", interval)
	}
	if parsed.seconds > maxSeconds {
		return 0, fmt.Errorf("interval %q is longer than YDB's Interval type holds (%s)",
			interval, formatInterval(maxSeconds))
	}
	return parsed.seconds, nil
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

type duration struct {
	seconds  uint64
	fraction bool
}

// parseDuration reads `P[nW][nD][T[nH][nM][n[.f]S]]`, with at least one part
// and the T present only when a time part follows it.
func parseDuration(text string) (duration, error) {
	rest, ok := strings.CutPrefix(text, "P")
	if !ok {
		return duration{}, fmt.Errorf("it does not begin with P")
	}
	datePart, timePart, hasTime := strings.Cut(rest, "T")
	if hasTime && timePart == "" {
		return duration{}, fmt.Errorf("T is not followed by hours, minutes or seconds")
	}
	var parsed duration
	parts := 0
	read := func(part string, designators []designator) error {
		next := 0
		for part != "" {
			end := strings.IndexFunc(part, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
			if end <= 0 {
				return fmt.Errorf("%q is not a number followed by a designator", part)
			}
			number, letter := part[:end], part[end]
			index := designatorIndex(designators, letter, next)
			if index < 0 {
				return fmt.Errorf("designator %q is out of place or not one YDB takes (weeks, days, hours, minutes, seconds)",
					string(letter))
			}
			whole, decimals, err := readNumber(number)
			if err != nil {
				return err
			}
			if decimals != "" && !designators[index].fractional {
				return fmt.Errorf("%q is not a number YDB takes there", number)
			}
			parsed.seconds += whole * designators[index].seconds
			parsed.fraction = parsed.fraction || strings.Trim(decimals, "0") != ""
			parts++
			next = index + 1
			part = part[end+1:]
		}
		return nil
	}
	if err := read(datePart, dateDesignators); err != nil {
		return duration{}, err
	}
	if err := read(timePart, timeDesignators); err != nil {
		return duration{}, err
	}
	if parts == 0 {
		return duration{}, fmt.Errorf("it names no weeks, days, hours, minutes or seconds")
	}
	return parsed, nil
}

type designator struct {
	letter     byte
	seconds    uint64
	fractional bool
}

var (
	dateDesignators = []designator{{letter: 'W', seconds: 7 * 86400}, {letter: 'D', seconds: 86400}}
	timeDesignators = []designator{
		{letter: 'H', seconds: 3600}, {letter: 'M', seconds: 60}, {letter: 'S', seconds: 1, fractional: true},
	}
)

func designatorIndex(designators []designator, letter byte, from int) int {
	for i := from; i < len(designators); i++ {
		if designators[i].letter == letter {
			return i
		}
	}
	return -1
}

func readNumber(number string) (whole uint64, decimals string, _ error) {
	integer, decimals, hasDecimals := strings.Cut(number, ".")
	if hasDecimals && (decimals == "" || strings.Contains(decimals, ".")) {
		return 0, "", fmt.Errorf("%q is not a number YDB takes there", number)
	}
	if integer == "" {
		return 0, "", fmt.Errorf("%q has no whole part", number)
	}
	value, err := strconv.ParseUint(integer, 10, 64)
	if err != nil || value > maxSeconds {
		return 0, "", fmt.Errorf("%q is too large", number)
	}
	return value, decimals, nil
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
