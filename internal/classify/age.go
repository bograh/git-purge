package classify

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseAge parses ages like "90d", "6mo", "2w", "1y", "36h" (or any Go duration).
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	units := []struct {
		suffix string
		d      time.Duration
	}{{"mo", 30 * 24 * time.Hour}, {"y", 365 * 24 * time.Hour}, {"w", 7 * 24 * time.Hour}, {"d", 24 * time.Hour}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(n, 64)
			if err != nil || v < 0 {
				return 0, fmt.Errorf("invalid age %q (try 90d, 6mo, 2w, 1y)", s)
			}
			return time.Duration(v * float64(u.d)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q (try 90d, 6mo, 2w, 1y)", s)
	}
	return d, nil
}

// HumanAge renders a duration the way ParseAge accepts it.
func HumanAge(d time.Duration) string {
	day := 24 * time.Hour
	switch {
	case d >= 365*day && d%(365*day) == 0:
		return fmt.Sprintf("%dy", d/(365*day))
	case d >= 30*day && d%(30*day) == 0:
		return fmt.Sprintf("%dmo", d/(30*day))
	case d >= day:
		return fmt.Sprintf("%dd", d/day)
	}
	return d.String()
}
