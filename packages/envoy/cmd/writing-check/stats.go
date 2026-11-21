package main

import (
	"math"

	"gonum.org/v1/gonum/stat/distuv"
)

// The confidences the measurement's bounds are stated at: one-sided 95% for a lone bound, and
// 97.5% for each half of a composed bound (the labeller's bound plus the scanner's bound against
// the labels, each at 1 − α/2 for α = 5%).
const (
	confidence95  = 0.95
	confidence975 = 0.975
)

// upperBound is the exact one-sided upper confidence bound on a rate after x events in n trials,
// at confidence conf (Clopper-Pearson: the Beta(x+1, n−x) quantile at conf). It is 1 for no trial
// and for x = n.
func upperBound(x, n int, conf float64) float64 {
	if n == 0 || x >= n {
		return 1
	}
	return distuv.Beta{Alpha: float64(x + 1), Beta: float64(n - x)}.Quantile(conf)
}

// lowerBound is the exact one-sided lower confidence bound on a rate after x events in n trials,
// at confidence conf (Clopper-Pearson: the Beta(x, n−x+1) quantile at 1 − conf). It is 0 for no
// trial and for x = 0.
func lowerBound(x, n int, conf float64) float64 {
	if n == 0 || x <= 0 {
		return 0
	}
	if x >= n {
		return math.Pow(1-conf, 1/float64(n))
	}
	return distuv.Beta{Alpha: float64(x), Beta: float64(n - x + 1)}.Quantile(1 - conf)
}

// requiredDraws is the trials a class needs to bound its error at e with confidence conf when no
// error is observed: the least n with (1 − e)^n ≤ 1 − conf, that is n ≥ ln(α) / ln(1 − e).
func requiredDraws(e, conf float64) int {
	if e <= 0 || e >= 1 {
		return 0
	}
	return int(math.Ceil(math.Log(1-conf)/math.Log(1-e) - 1e-9))
}

// rate is a share of hits in n trials, as recall and precision are reported: the point estimate
// (absent for no trial) and the exact one-sided 95% lower bound.
type rate struct {
	Hits    int      `json:"hits"`
	N       int      `json:"n"`
	Point   *float64 `json:"point"`
	Lower95 float64  `json:"lower_95"`
}

func newRate(hits, n int) rate {
	r := rate{Hits: hits, N: n, Lower95: lowerBound(hits, n, confidence95)}
	if n > 0 {
		p := float64(hits) / float64(n)
		r.Point = &p
	}
	return r
}

// errorRate is a share of errors in n trials, as 1 − precision is bounded: the point estimate
// (absent for no trial) and the exact one-sided upper bounds at 95% and at 97.5%.
type errorRate struct {
	Errors   int      `json:"errors"`
	N        int      `json:"n"`
	Point    *float64 `json:"point"`
	Upper95  float64  `json:"upper_95"`
	Upper975 float64  `json:"upper_975"`
}

func newErrorRate(errors, n int) errorRate {
	r := errorRate{Errors: errors, N: n, Upper95: upperBound(errors, n, confidence95), Upper975: upperBound(errors, n, confidence975)}
	if n > 0 {
		p := float64(errors) / float64(n)
		r.Point = &p
	}
	return r
}

// share is part over whole, 0 for an empty whole.
func share(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// pointText is a rate's point estimate as a percentage, or "-" with no trial.
func pointText(p *float64) string {
	if p == nil {
		return "-"
	}
	return pct(*p)
}
