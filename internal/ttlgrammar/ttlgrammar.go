// Package ttlgrammar reads the two values a row table's TTL declaration spells
// as text: the interval after which YDB deletes a row, and the unit an integer
// column counts since the Unix epoch.
//
// The grammar is the one YDB's Interval literal takes, measured on 25.1.4.7 and
// 26.2.1.14. An interval is an ISO 8601 duration of weeks, days, hours, minutes
// and seconds. Months and years have no fixed length and are refused, and so
// are lower case, a sign and surrounding space. The server keeps whole seconds
// and drops a fraction in silence, so a fraction of a second is refused here
// instead of declaring a policy that never reads back. The longest interval is
// one second short of 49673 days.
//
// The package reads text only. Which column a TTL reads, and whether that
// column's type fits the unit, is decided where the table's columns are known.
//
// A refusal is an *Error. Its text quotes the declaration, and its reason says
// the same without it, for a host that prints a provider's reasons: a reason
// never quotes a value the declaration wrote.
package ttlgrammar

import (
	"fmt"
	"strconv"
	"strings"
)

// The units an integer TTL column may count in, as YQL spells them.
const (
	Seconds      = "SECONDS"
	Milliseconds = "MILLISECONDS"
	Microseconds = "MICROSECONDS"
	Nanoseconds  = "NANOSECONDS"
)

// units are the accepted units, in the order a refusal lists them.
var units = []string{Seconds, Milliseconds, Microseconds, Nanoseconds}

// Error is an interval or a unit YDB does not take.
type Error struct {
	text, reason string
}

// Error returns the refusal with the declared value it quotes.
func (e *Error) Error() string { return e.text }

// Reason returns the refusal without the declared value, as one sentence a
// host may print, such as `the interval is not an ISO 8601 duration YDB takes
// (such as P30D or PT1H30M): it does not begin with P`.
func (e *Error) Reason() string { return e.reason }

// Within returns the refusal with prefix in front of both its text and its
// reason, such as the tier of a TTL the refused interval belongs to.
func (e *Error) Within(prefix string) *Error {
	return &Error{text: prefix + e.text, reason: prefix + e.reason}
}

// refusal is an Error whose text quotes value where its reason names it.
func refusal(noun, value, rest string) *Error {
	return &Error{text: fmt.Sprintf("%s %q %s", noun, value, rest), reason: "the " + noun + " " + rest}
}

// MaxSeconds is the longest interval YDB's Interval type holds: P49672DT23H59M59S
// is accepted and P49673D answers `Invalid value "P49673D" for type Interval`.
const MaxSeconds = 4291747199

// Unit reads a declared unit in any case and returns it as YQL spells it. An
// empty unit stays empty: it belongs to a date column's policy.
func Unit(declared string) (string, error) {
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
	return "", refusal("unit", declared, "is not one YDB takes: use "+strings.Join(units, ", "))
}

// IntervalSeconds reads an interval as YDB's Interval literal takes it and
// returns the whole number of seconds YDB keeps of it. A spelling YDB refuses is
// refused here, and so is a fraction of a second.
func IntervalSeconds(interval string) (uint64, error) {
	parsed, err := parseDuration(interval)
	if err != nil {
		return 0, &Error{text: fmt.Sprintf("interval %q is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): %s", interval, err.text),
			reason: "the interval is not an ISO 8601 duration YDB takes (such as P30D or PT1H30M): " + err.reason}
	}
	if parsed.fraction {
		return 0, refusal("interval", interval, "has a fraction of a second, and YDB keeps whole seconds only (PT1.5S reads back as PT1S)")
	}
	if parsed.seconds > MaxSeconds {
		return 0, refusal("interval", interval, "is longer than YDB's Interval type holds (P49672DT23H59M59S)")
	}
	return parsed.seconds, nil
}

// duration is an ISO 8601 duration as YDB's Interval literal reads it.
type duration struct {
	seconds  uint64
	fraction bool
}

// parseDuration reads `P[nW][nD][T[nH][nM][n[.f]S]]` with at least one part and
// the T present only when a time part follows it. A refusal's text quotes the
// part it refuses, and its reason describes the part.
func parseDuration(text string) (duration, *Error) {
	plain := func(problem string) *Error { return &Error{text: problem, reason: problem} }
	rest, ok := strings.CutPrefix(text, "P")
	if !ok {
		return duration{}, plain("it does not begin with P")
	}
	datePart, timePart, hasTime := strings.Cut(rest, "T")
	if hasTime && timePart == "" {
		return duration{}, plain("the T is not followed by hours, minutes or seconds")
	}
	var parsed duration
	parts := 0
	read := func(part string, designators []designator) *Error {
		next := 0
		for part != "" {
			end := strings.IndexFunc(part, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
			if end <= 0 {
				return &Error{text: fmt.Sprintf("%q is not a number followed by a designator", part), reason: "a part is not a number followed by a designator"}
			}
			number, letter := part[:end], part[end]
			index := designatorIndex(designators, letter, next)
			if index < 0 {
				const takes = "is out of place or not one YDB takes (weeks, days, hours, minutes, seconds)"
				return &Error{text: fmt.Sprintf("designator %q %s", string(letter), takes), reason: "a designator " + takes}
			}
			whole, decimals, err := readNumber(number)
			if err != nil {
				return err
			}
			if decimals != "" && !designators[index].fractional {
				return &Error{text: fmt.Sprintf("%q is not a number YDB takes there", number), reason: "a number has a fraction where YDB takes a whole number"}
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
		return duration{}, plain("it names no weeks, days, hours, minutes or seconds")
	}
	return parsed, nil
}

// designator is one unit letter of a duration.
type designator struct {
	letter     byte
	seconds    uint64
	fractional bool
}

var (
	dateDesignators = []designator{{letter: 'W', seconds: 7 * 86400}, {letter: 'D', seconds: 86400}}
	timeDesignators = []designator{{letter: 'H', seconds: 3600}, {letter: 'M', seconds: 60}, {letter: 'S', seconds: 1, fractional: true}}
)

// designatorIndex finds letter among designators at or after from, so each
// appears once and in order.
func designatorIndex(designators []designator, letter byte, from int) int {
	for i := from; i < len(designators); i++ {
		if designators[i].letter == letter {
			return i
		}
	}
	return -1
}

// readNumber reads a duration part's number: its whole part, and the digits
// after a decimal point, which only seconds may carry.
func readNumber(number string) (whole uint64, decimals string, _ *Error) {
	integer, decimals, hasDecimals := strings.Cut(number, ".")
	if hasDecimals && (decimals == "" || strings.Contains(decimals, ".")) {
		return 0, "", &Error{text: fmt.Sprintf("%q is not a number YDB takes there", number), reason: "a number is not one YDB takes there"}
	}
	if integer == "" {
		return 0, "", &Error{text: fmt.Sprintf("%q has no whole part", number), reason: "a number has no whole part"}
	}
	value, err := strconv.ParseUint(integer, 10, 64)
	if err != nil || value > MaxSeconds {
		return 0, "", &Error{text: fmt.Sprintf("%q is too large", number), reason: "a number is too large"}
	}
	return value, decimals, nil
}
