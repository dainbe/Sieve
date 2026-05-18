//go:build eval

package eval

import (
	"math"
	"sort"
)

// Metrics holds precision/recall/token stats for a single eval case.
type Metrics struct {
	CaseID        string
	Query         string
	K             int
	Retrieved     []string // file IDs returned by ctx_build_context
	GroundTruth   []string
	PrecisionAtK  float64
	RecallAtK     float64
	MRR           float64
	NDCGAtK       float64
	TokenEstimate int
	WithinBudget  bool
	BuildLatencyMS   float64 // wall time of Build() in milliseconds
	RawFileTokens    int     // estimated tokens in GT files (no compression)
	TokenRatio       float64 // TokenEstimate / RawFileTokens; < 1 means compression
	// Balance metrics
	CompressionRatio   float64 // TokenEstimate / projectTotalTokens; target < 0.1
	InformationDensity float64 // tokens from GT files / TokenEstimate; higher = less noise
	EfficiencyScore    float64 // harmonic mean of PrecisionAtK and InformationDensity: 2*P*D/(P+D)
}

// Compute fills all ranking metrics for the case.
func (m *Metrics) Compute(tokenBudget int) {
	gt := toSet(m.GroundTruth)
	top := m.Retrieved
	if len(top) > m.K {
		top = top[:m.K]
	}

	tp := 0
	dcg := 0.0
	for i, id := range top {
		if gt[id] {
			tp++
			rank := i + 1 // 1-indexed
			if m.MRR == 0 {
				m.MRR = math.Round(1.0/float64(rank)*1000) / 1000
			}
			dcg += 1.0 / math.Log2(float64(rank)+1)
		}
	}

	// IDCG: ideal placement — ground truth hits at top positions
	idcg := 0.0
	for i := 0; i < len(m.GroundTruth) && i < m.K; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg > 0 {
		m.NDCGAtK = math.Round(dcg/idcg*1000) / 1000
	}

	if len(top) > 0 {
		m.PrecisionAtK = math.Round(float64(tp)/float64(len(top))*1000) / 1000
	}
	if len(gt) > 0 {
		m.RecallAtK = math.Round(float64(tp)/float64(len(gt))*1000) / 1000
	}
	m.WithinBudget = m.TokenEstimate <= tokenBudget

	// EfficiencyScore: harmonic mean of PrecisionAtK and InformationDensity.
	p, d := m.PrecisionAtK, m.InformationDensity
	if p+d > 0 {
		m.EfficiencyScore = math.Round(2*p*d/(p+d)*1000) / 1000
	}
}

func toSet(ids []string) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

// Summary aggregates multiple Metrics into averages and distribution stats.
type Summary struct {
	Count                  int
	AvgPrecision           float64
	AvgRecall              float64
	AvgMRR                 float64
	AvgNDCG                float64
	AvgTokens              float64
	BudgetPassPct          float64
	AvgLatencyMS           float64
	AvgTokenRatio          float64
	AvgCompressionRatio    float64
	AvgInformationDensity  float64
	AvgEfficiencyScore     float64
	// Distribution stats for P@5 and R@5.
	P5Min    float64
	P5Max    float64
	P5Median float64
	R5Min    float64
	R5Max    float64
	R5Median float64
}

func Summarize(ms []Metrics) Summary {
	if len(ms) == 0 {
		return Summary{}
	}
	var totalP, totalR, totalMRR, totalNDCG, totalTok, totalLat, totalRatio float64
	var totalCompr, totalDens, totalEff float64
	budgetPass := 0
	ratioCount, comprCount := 0, 0
	ps := make([]float64, 0, len(ms))
	rs := make([]float64, 0, len(ms))
	for _, m := range ms {
		totalP += m.PrecisionAtK
		totalR += m.RecallAtK
		totalMRR += m.MRR
		totalNDCG += m.NDCGAtK
		totalTok += float64(m.TokenEstimate)
		totalLat += m.BuildLatencyMS
		if m.WithinBudget {
			budgetPass++
		}
		if m.RawFileTokens > 0 {
			totalRatio += m.TokenRatio
			ratioCount++
		}
		if m.CompressionRatio > 0 {
			totalCompr += m.CompressionRatio
			totalDens += m.InformationDensity
			totalEff += m.EfficiencyScore
			comprCount++
		}
		ps = append(ps, m.PrecisionAtK)
		rs = append(rs, m.RecallAtK)
	}
	n := float64(len(ms))
	round3 := func(v float64) float64 { return math.Round(v*1000) / 1000 }
	s := Summary{
		Count:         len(ms),
		AvgPrecision:  round3(totalP / n),
		AvgRecall:     round3(totalR / n),
		AvgMRR:        round3(totalMRR / n),
		AvgNDCG:       round3(totalNDCG / n),
		AvgTokens:     round3(totalTok / n),
		BudgetPassPct: round3(float64(budgetPass) / n * 100),
		AvgLatencyMS:  round3(totalLat / n),
	}
	if ratioCount > 0 {
		s.AvgTokenRatio = round3(totalRatio / float64(ratioCount))
	}
	if comprCount > 0 {
		cn := float64(comprCount)
		s.AvgCompressionRatio   = round3(totalCompr / cn)
		s.AvgInformationDensity = round3(totalDens / cn)
		s.AvgEfficiencyScore    = round3(totalEff / cn)
	}
	// Distribution stats.
	sort.Float64s(ps)
	sort.Float64s(rs)
	s.P5Min = round3(ps[0])
	s.P5Max = round3(ps[len(ps)-1])
	s.P5Median = round3(median(ps))
	s.R5Min = round3(rs[0])
	s.R5Max = round3(rs[len(rs)-1])
	s.R5Median = round3(median(rs))
	return s
}

// median returns the median of a sorted slice of float64.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
