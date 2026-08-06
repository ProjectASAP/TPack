package tpackmodel

import (
	"math"
	"math/rand"
	"sort"
)

// GaussianMixture1D is a 1-dimensional Gaussian Mixture Model.
// It supports fitting via EM and sampling.
type GaussianMixture1D struct {
	Weights    []float64 // Component weights (sum to 1)
	Means      []float64 // Component means
	Variances  []float64 // Component variances
	Components int       // Number of components
}

// NewGaussianMixture1D creates a GMM with the specified number of components.
func NewGaussianMixture1D(nComponents int) *GaussianMixture1D {
	return &GaussianMixture1D{
		Weights:    make([]float64, nComponents),
		Means:      make([]float64, nComponents),
		Variances:  make([]float64, nComponents),
		Components: nComponents,
	}
}

// Fit trains the GMM on 1D data using Expectation-Maximization.
func (g *GaussianMixture1D) Fit(data []float64) {
	n := len(data)
	if n == 0 {
		return
	}
	k := g.Components
	if k <= 0 {
		return
	}
	if k > n {
		k = n
		g.Components = k
		g.Weights = make([]float64, k)
		g.Means = make([]float64, k)
		g.Variances = make([]float64, k)
	}

	// Initialize using evenly-spaced quantiles
	sorted := make([]float64, n)
	copy(sorted, data)
	sort.Float64s(sorted)

	for i := 0; i < k; i++ {
		idx := (i*n + n/2) / k
		if idx >= n {
			idx = n - 1
		}
		g.Means[i] = sorted[idx]
		g.Weights[i] = 1.0 / float64(k)
	}

	// Initialize variances from data
	totalVar := variance(data)
	if totalVar < 1e-10 {
		totalVar = 1e-10
	}
	for i := 0; i < k; i++ {
		g.Variances[i] = totalVar
	}

	// EM iterations
	responsibilities := make([][]float64, n)
	for i := range responsibilities {
		responsibilities[i] = make([]float64, k)
	}

	const maxIter = 100
	const tol = 1e-6
	prevLogLik := math.Inf(-1)

	for range maxIter {
		// E-step: compute responsibilities
		logLik := 0.0
		for i := range n {
			maxLog := math.Inf(-1)
			for j := 0; j < k; j++ {
				logP := math.Log(g.Weights[j]) + logNormalPDF(data[i], g.Means[j], g.Variances[j])
				responsibilities[i][j] = logP
				if logP > maxLog {
					maxLog = logP
				}
			}
			// Log-sum-exp for numerical stability
			sumExp := 0.0
			for j := 0; j < k; j++ {
				responsibilities[i][j] = math.Exp(responsibilities[i][j] - maxLog)
				sumExp += responsibilities[i][j]
			}
			logLik += maxLog + math.Log(sumExp)
			for j := 0; j < k; j++ {
				responsibilities[i][j] /= sumExp
			}
		}

		// Check convergence
		if math.Abs(logLik-prevLogLik) < tol {
			break
		}
		prevLogLik = logLik

		// M-step: update parameters
		for j := 0; j < k; j++ {
			nk := 0.0
			meanSum := 0.0
			for i := range n {
				nk += responsibilities[i][j]
				meanSum += responsibilities[i][j] * data[i]
			}

			if nk < 1e-10 {
				continue
			}

			g.Weights[j] = nk / float64(n)
			g.Means[j] = meanSum / nk

			varSum := 0.0
			for i := range n {
				diff := data[i] - g.Means[j]
				varSum += responsibilities[i][j] * diff * diff
			}
			g.Variances[j] = varSum / nk

			// Floor variance to prevent degenerate distributions
			if g.Variances[j] < 1e-6 {
				g.Variances[j] = 1e-6
			}
		}
	}
}

// Sample draws a single sample from the GMM.
func (g *GaussianMixture1D) Sample(rng *rand.Rand) float64 {
	// Pick component by weight
	r := rng.Float64()
	cumWeight := 0.0
	comp := 0
	for i := 0; i < g.Components; i++ {
		cumWeight += g.Weights[i]
		if r <= cumWeight {
			comp = i
			break
		}
	}

	// Sample from selected Gaussian
	return rng.NormFloat64()*math.Sqrt(g.Variances[comp]) + g.Means[comp]
}

// logNormalPDF computes the log of the normal PDF at x with given mean and variance.
func logNormalPDF(x, mean, variance float64) float64 {
	if variance <= 0 {
		variance = 1e-10
	}
	diff := x - mean
	return -0.5*math.Log(2*math.Pi*variance) - 0.5*diff*diff/variance
}

// variance computes the sample variance of data.
func variance(data []float64) float64 {
	n := len(data)
	if n < 2 {
		return 0
	}
	mean := 0.0
	for _, v := range data {
		mean += v
	}
	mean /= float64(n)

	v := 0.0
	for _, val := range data {
		diff := val - mean
		v += diff * diff
	}
	return v / float64(n)
}
