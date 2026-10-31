package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// compare benchmarks two servers on the same database and machine, a
// baseline (the previous release) and a candidate (the change), and fails
// when the candidate is slower than the threshold allows.
//
// Shared machines drift: a CI runner's speed changes from minute to minute.
// So the two servers are measured in alternation, scenario by scenario,
// round after round, with the order swapped each round; each round yields
// a candidate/baseline ratio taken minutes apart at most, and a scenario's
// verdict is the median of its ratios. A single slow round can't fail it,
// and drift that affects both servers alike cancels out.
func compare(args []string) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	baseURL := fs.String("baseline", "", "URL of the baseline server (the previous release)")
	candURL := fs.String("candidate", "", "URL of the candidate server (the change)")
	baseName := fs.String("baseline-name", "baseline", "name of the baseline in the report")
	candName := fs.String("candidate-name", "candidate", "name of the candidate in the report")
	conc := fs.Int("c", 8, "concurrent clients")
	dur := fs.Duration("d", 3*time.Second, "duration of one measurement")
	warm := fs.Duration("warmup", time.Second, "warm-up before each measurement")
	rounds := fs.Int("rounds", 5, "rounds; each measures every scenario on both servers")
	threshold := fs.Float64("threshold", 0.15, "fail when a scenario's median ratio is below 1 - threshold")
	token := fs.String("token", "", "JWT for scenarios that need an authenticated role")
	secret := fs.String("secret", "", "HS256 secret to sign a web_user token with, instead of -token")
	only := fs.String("only", "", "run only scenarios whose name contains this text")
	summary := fs.String("summary", "", "append the Markdown report to this file (e.g. $GITHUB_STEP_SUMMARY)")
	jsonOut := fs.String("json", "", "write the measurements as JSON to this file")
	fs.Parse(args)
	if *baseURL == "" || *candURL == "" {
		fmt.Fprintln(os.Stderr, "-baseline and -candidate are required")
		os.Exit(2)
	}
	if *token == "" && *secret != "" {
		t, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"role": "web_user"}).SignedString([]byte(*secret))
		check(err)
		*token = t
	}

	tr := &http.Transport{MaxIdleConns: *conc * 4, MaxIdleConnsPerHost: *conc * 2, IdleConnTimeout: time.Minute}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	// both servers must answer a read before minutes are spent measuring them
	for _, u := range []string{*baseURL, *candURL} {
		resp, err := client.Get(u + "/books?select=id&limit=1")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s is not ready: %v\n", u, err)
			os.Exit(2)
		}
	}

	var scs []scenario
	for _, sc := range scenarios {
		if *only != "" && !strings.Contains(sc.name, *only) {
			continue
		}
		if sc.auth && *token == "" {
			fmt.Fprintf(os.Stderr, "skipping %q: needs -token or -secret\n", sc.name)
			continue
		}
		scs = append(scs, sc)
	}
	res := make([]comparison, len(scs))
	for i, sc := range scs {
		res[i].Scenario = sc.name
	}
	start := time.Now()
	for round := 0; round < *rounds; round++ {
		fmt.Fprintf(os.Stderr, "round %d of %d\n", round+1, *rounds)
		for i, sc := range scs {
			measure := func(url string) result {
				runFor(client, url, sc, *token, *conc, *warm)
				return runFor(client, url, sc, *token, *conc, *dur)
			}
			var b, c result
			if round%2 == 0 {
				b, c = measure(*baseURL), measure(*candURL)
			} else {
				c, b = measure(*candURL), measure(*baseURL)
			}
			res[i].Baseline = append(res[i].Baseline, b.rps)
			res[i].Candidate = append(res[i].Candidate, c.rps)
			res[i].BaselineErrors += b.errs
			res[i].CandidateErrors += c.errs
			if c.firstErr != "" && res[i].FirstError == "" {
				res[i].FirstError = c.firstErr
			}
		}
	}
	failed := false
	for i := range res {
		res[i].judge(*threshold)
		failed = failed || res[i].failed()
	}

	report := renderReport(res, *baseName, *candName, *threshold, *conc, *rounds, *dur, time.Since(start))
	fmt.Print(report)
	if *summary != "" {
		f, err := os.OpenFile(*summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		check(err)
		f.WriteString(report)
		f.Close()
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(res, "", "  ")
		check(os.WriteFile(*jsonOut, b, 0o644))
	}
	if failed {
		os.Exit(1)
	}
}

// comparison holds one scenario's measurements in requests per second, one
// value per round for each server.
type comparison struct {
	Scenario        string    `json:"scenario"`
	Baseline        []float64 `json:"baseline"`
	Candidate       []float64 `json:"candidate"`
	BaselineErrors  int64     `json:"baselineErrors"`
	CandidateErrors int64     `json:"candidateErrors"`
	FirstError      string    `json:"firstError,omitempty"`
	Ratio           float64   `json:"ratio"` // median of the per-round candidate/baseline ratios
	Verdict         string    `json:"verdict"`
}

// judge sets Ratio and Verdict: "slower" below 1 - threshold, "faster"
// above 1 + threshold, "errors" when the candidate failed requests the
// baseline didn't, otherwise "same".
func (c *comparison) judge(threshold float64) {
	var ratios []float64
	for i := range c.Baseline {
		if i < len(c.Candidate) && c.Baseline[i] > 0 {
			ratios = append(ratios, c.Candidate[i]/c.Baseline[i])
		}
	}
	c.Ratio = median(ratios)
	ok := func(v []float64) bool { return median(v) > 0 }
	switch {
	case len(ratios) == 0 || !ok(c.Baseline) || !ok(c.Candidate):
		c.Verdict = "no data"
	case c.CandidateErrors > 0 && c.BaselineErrors == 0:
		c.Verdict = "errors"
	case c.Ratio < 1-threshold:
		c.Verdict = "slower"
	case c.Ratio > 1+threshold:
		c.Verdict = "faster"
	default:
		c.Verdict = "same"
	}
}

// failed reports a verdict that fails the comparison: slower, new errors,
// or no successful request to compare.
func (c *comparison) failed() bool {
	return c.Verdict == "slower" || c.Verdict == "errors" || c.Verdict == "no data"
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func renderReport(res []comparison, baseName, candName string, threshold float64, conc, rounds int, dur, took time.Duration) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "### Performance: %s against %s\n\n", candName, baseName)
	fmt.Fprintf(&sb, "%d clients, %d rounds of %s per scenario and server, alternating; %s/%s, %d CPUs; took %s. "+
		"A scenario fails when the median of its per-round ratios is below %.0f%%.\n\n",
		conc, rounds, dur, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), took.Round(time.Second), (1-threshold)*100)
	fmt.Fprintf(&sb, "| Scenario | %s req/s | %s req/s | change | range of rounds | |\n|---|---:|---:|---:|---:|---|\n", baseName, candName)
	slower := 0
	for _, c := range res {
		mark := map[string]string{"same": "", "faster": "faster", "slower": "**slower**", "errors": "**errors**", "no data": "**no data**"}[c.Verdict]
		if c.failed() {
			slower++
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for i := range c.Baseline {
			if c.Baseline[i] > 0 && i < len(c.Candidate) {
				r := c.Candidate[i] / c.Baseline[i]
				lo, hi = math.Min(lo, r), math.Max(hi, r)
			}
		}
		rng := ""
		if !math.IsInf(lo, 0) {
			rng = fmt.Sprintf("%+.0f%% to %+.0f%%", (lo-1)*100, (hi-1)*100)
		}
		change := "n/a"
		if !math.IsNaN(c.Ratio) {
			change = fmt.Sprintf("%+.1f%%", (c.Ratio-1)*100)
		}
		fmt.Fprintf(&sb, "| %s | %.0f | %.0f | %s | %s | %s |\n", c.Scenario, median(c.Baseline), median(c.Candidate), change, rng, mark)
	}
	sb.WriteString("\n")
	for _, c := range res {
		if c.FirstError != "" {
			fmt.Fprintf(&sb, "- %s: %d failed requests with %s, %d with %s; first: `%s`\n", c.Scenario, c.CandidateErrors, candName, c.BaselineErrors, baseName, c.FirstError)
		}
	}
	if slower == 0 {
		sb.WriteString("No scenario is slower than the threshold.\n")
	} else {
		fmt.Fprintf(&sb, "%d scenario(s) slower than the threshold or failing.\n", slower)
	}
	return sb.String()
}
