package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestJudge(t *testing.T) {
	for _, c := range []struct {
		name       string
		base, cand []float64
		cerrs      int64
		want       string
		ratio      float64
	}{
		{"same within noise", []float64{1000, 1000, 1000}, []float64{960, 1030, 990}, 0, "same", 0.99},
		{"clearly slower", []float64{1000, 1100, 900}, []float64{800, 850, 700}, 0, "slower", 0.7778},
		// one bad round in five doesn't fail a scenario
		{"one slow round", []float64{1000, 1000, 1000, 1000, 1000}, []float64{1000, 500, 990, 1010, 1000}, 0, "same", 1.0},
		// drift that slows both servers in a round cancels out
		{"drift on both", []float64{1000, 500, 1000}, []float64{1000, 500, 1000}, 0, "same", 1.0},
		{"faster", []float64{1000, 1000, 1000}, []float64{1300, 1250, 1400}, 0, "faster", 1.3},
		{"new errors", []float64{1000}, []float64{1000}, 3, "errors", 1.0},
		{"nothing measured", []float64{0, 0}, []float64{0, 0}, 0, "no data", math.NaN()},
		{"just inside", []float64{1000, 1000}, []float64{860, 860}, 0, "same", 0.86},
		{"just outside", []float64{1000, 1000}, []float64{840, 840}, 0, "slower", 0.84},
	} {
		cmp := comparison{Baseline: c.base, Candidate: c.cand, CandidateErrors: c.cerrs}
		cmp.judge(0.15)
		if cmp.Verdict != c.want || !(math.Abs(cmp.Ratio-c.ratio) <= 0.001 || math.IsNaN(c.ratio) && math.IsNaN(cmp.Ratio)) {
			t.Errorf("%s: %s %.4f, want %s %.4f", c.name, cmp.Verdict, cmp.Ratio, c.want, c.ratio)
		}
	}
}

func TestReport(t *testing.T) {
	res := []comparison{
		{Scenario: "row by primary key", Baseline: []float64{5000, 5100}, Candidate: []float64{5050, 5000}},
		{Scenario: "page of 100 rows", Baseline: []float64{2300, 2300}, Candidate: []float64{1500, 1600}},
	}
	for i := range res {
		res[i].judge(0.15)
	}
	r := renderReport(res, "v0.7.0", "main", 0.15, 8, 2, 3*time.Second, time.Minute)
	for _, want := range []string{"| v0.7.0 req/s | main req/s |", "| page of 100 rows | 2300 | 1550 | -32.6% | -35% to -30% | **slower** |", "1 scenario(s) slower"} {
		if !strings.Contains(r, want) {
			t.Errorf("report lacks %q:\n%s", want, r)
		}
	}
}
