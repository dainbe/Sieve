//go:build eval

package eval

import (
	"math"
	"testing"
)

func TestMetrics_MRR(t *testing.T) {
	t.Run("hit at rank 1", func(t *testing.T) {
		m := Metrics{
			K:           5,
			Retrieved:   []string{"a.go", "b.go", "c.go"},
			GroundTruth: []string{"a.go"},
		}
		m.Compute(4000)
		if m.MRR != 1.0 {
			t.Errorf("want MRR=1.000, got %.3f", m.MRR)
		}
	})

	t.Run("hit at rank 3", func(t *testing.T) {
		m := Metrics{
			K:           5,
			Retrieved:   []string{"a.go", "b.go", "c.go"},
			GroundTruth: []string{"c.go"},
		}
		m.Compute(4000)
		want := math.Round(1.0/3.0*1000) / 1000
		if m.MRR != want {
			t.Errorf("want MRR=%.3f, got %.3f", want, m.MRR)
		}
	})

	t.Run("no hit", func(t *testing.T) {
		m := Metrics{
			K:           5,
			Retrieved:   []string{"a.go", "b.go"},
			GroundTruth: []string{"z.go"},
		}
		m.Compute(4000)
		if m.MRR != 0 {
			t.Errorf("want MRR=0, got %.3f", m.MRR)
		}
	})
}

func TestMetrics_NDCG(t *testing.T) {
	t.Run("perfect rank 1", func(t *testing.T) {
		m := Metrics{
			K:           5,
			Retrieved:   []string{"a.go", "b.go", "c.go"},
			GroundTruth: []string{"a.go"},
		}
		m.Compute(4000)
		if m.NDCGAtK != 1.0 {
			t.Errorf("want nDCG=1.000, got %.3f", m.NDCGAtK)
		}
	})

	t.Run("hit at rank 2 single truth", func(t *testing.T) {
		m := Metrics{
			K:           5,
			Retrieved:   []string{"x.go", "a.go", "b.go"},
			GroundTruth: []string{"a.go"},
		}
		m.Compute(4000)
		// DCG = 1/log2(3), IDCG = 1/log2(2) = 1 → nDCG = (1/log2(3))/1
		want := math.Round((1.0/math.Log2(3))/1.0*1000) / 1000
		if m.NDCGAtK != want {
			t.Errorf("want nDCG=%.3f, got %.3f", want, m.NDCGAtK)
		}
	})

	t.Run("no hit returns zero", func(t *testing.T) {
		m := Metrics{
			K:           5,
			Retrieved:   []string{"a.go", "b.go"},
			GroundTruth: []string{"z.go"},
		}
		m.Compute(4000)
		if m.NDCGAtK != 0 {
			t.Errorf("want nDCG=0, got %.3f", m.NDCGAtK)
		}
	})

	t.Run("two truths both retrieved", func(t *testing.T) {
		m := Metrics{
			K:           5,
			Retrieved:   []string{"a.go", "b.go", "c.go"},
			GroundTruth: []string{"a.go", "b.go"},
		}
		m.Compute(4000)
		// DCG = 1/log2(2) + 1/log2(3), IDCG = 1/log2(2) + 1/log2(3) → nDCG=1
		if m.NDCGAtK != 1.0 {
			t.Errorf("want nDCG=1.000, got %.3f", m.NDCGAtK)
		}
	})
}
