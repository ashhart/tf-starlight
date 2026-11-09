package routing

import (
	"math"
	"slices"
)

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	return ordered[max(0, int(math.Ceil(quantile*float64(len(ordered))))-1)]
}
