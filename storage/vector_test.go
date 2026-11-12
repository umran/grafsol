package storage_test

import (
	"math"
	"testing"

	"github.com/umran/grafsol/storage"
)

func TestCosineDistance(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []float32
		distance float64
		measured bool
	}{
		{"the same direction, whatever the lengths", []float32{1, 0}, []float32{5, 0}, 0, true},
		{"45 degrees apart", []float32{1, 0}, []float32{1, 1}, 1 - 1/math.Sqrt2, true},
		{"at right angles", []float32{1, 0}, []float32{0, 3}, 1, true},
		{"opposite directions", []float32{1, 0}, []float32{-2, 0}, 2, true},
		{"different numbers of dimensions", []float32{1, 0}, []float32{1, 0, 0}, 0, false},
		{"a vector of zeroes", []float32{1, 0}, []float32{0, 0}, 0, false},
		{"no vector at all", nil, nil, 0, false},
	}
	for _, c := range cases {
		distance, measured := storage.CosineDistance(c.a, c.b)
		if measured != c.measured {
			t.Errorf("%s: measured is %v, want %v", c.name, measured, c.measured)
			continue
		}
		if measured && math.Abs(float64(distance)-c.distance) > 1e-6 {
			t.Errorf("%s: distance is %v, want %v", c.name, distance, c.distance)
		}
	}
}
