// Package declaration interprets YDB source settings without database access.
package declaration

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var duration = regexp.MustCompile(`^P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)(?:\.(\d{1,6}))?S)?)?$`)

// CoordinationPeriod parses the YDB ISO8601 duration grammar into exact whole
// milliseconds. It refuses overflow and precision the native field cannot keep.
func CoordinationPeriod(text string) (uint32, error) {
	text = strings.TrimSpace(text)
	parts := duration.FindStringSubmatch(text)
	if parts == nil || strings.HasSuffix(text, "P") || strings.HasSuffix(text, "T") {
		return 0, fmt.Errorf("expected an ISO8601 period such as PT1S or PT0.5S")
	}
	var total uint64
	for i, unit := range []uint64{7 * 24 * 60 * 60 * 1000, 24 * 60 * 60 * 1000, 60 * 60 * 1000, 60 * 1000, 1000} {
		if parts[i+1] == "" {
			continue
		}
		count, err := strconv.ParseUint(parts[i+1], 10, 64)
		if err != nil || count > (math.MaxUint32-total)/unit {
			return 0, fmt.Errorf("period exceeds the native uint32 millisecond range")
		}
		total += count * unit
	}
	if fraction := parts[6]; fraction != "" {
		fraction += strings.Repeat("0", 6-len(fraction))
		micros, _ := strconv.ParseUint(fraction, 10, 32)
		if micros%1000 != 0 {
			return 0, fmt.Errorf("period must contain whole milliseconds")
		}
		if micros/1000 > math.MaxUint32-total {
			return 0, fmt.Errorf("period exceeds the native uint32 millisecond range")
		}
		total += micros / 1000
	}
	if total == 0 {
		return 0, fmt.Errorf("period must be positive")
	}
	return uint32(total), nil
}
