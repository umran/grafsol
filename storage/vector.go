package storage

import "math"

// Farthest is the greatest cosine distance there is: that of two vectors
// pointing in opposite directions. NeighborSearcher also reports it for a
// vertex it cannot measure, which is what puts such vertices last.
const Farthest float32 = 2

// CosineDistance returns how far apart two vectors point: 0 for the same
// direction, 1 for unrelated directions, 2 for opposite ones. It is the
// distance that DocumentSearcher and NeighborSearcher rank by.
//
// measured is false when there is nothing to measure: the vectors differ in
// length, or one of them is empty or all zeroes. A vector of zeroes points
// nowhere, so it is no particular distance from anything.
func CosineDistance(a, b []float32) (distance float32, measured bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	var dot, lengthA, lengthB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		lengthA += float64(a[i]) * float64(a[i])
		lengthB += float64(b[i]) * float64(b[i])
	}
	if lengthA == 0 || lengthB == 0 {
		return 0, false
	}
	return float32(1 - dot/math.Sqrt(lengthA*lengthB)), true
}
