package evaluate

import (
	"fmt"
	"math"
	"strconv"
)

// pctFloor is a percentage rounded down, so a value is never shown above a
// threshold it missed (D-021). Counts stay below 2^53, so the arithmetic is
// the concierge's exactly.
func pctFloor(num, den int64, places int) float64 {
	if den == 0 {
		return 0
	}
	f := math.Pow10(places)
	return math.Floor(float64(num*100*int64(f))/float64(den)) / f
}

func pctText(num, den int64, places int) string {
	return strconv.FormatFloat(pctFloor(num, den, places), 'f', places, 64) + "%"
}

// g formats like Python's "{:g}": six significant digits, no trailing zeros.
func g(x float64) string {
	return strconv.FormatFloat(x, 'g', 6, 64)
}

// round1 is Python's round(x, 1): the nearest one-decimal value to x's exact binary value.
func round1(x float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 1, 64), 64)
	return v
}

func f1(x float64) string { return fmt.Sprintf("%.1f", x) }
