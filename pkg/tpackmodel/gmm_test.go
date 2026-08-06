package tpackmodel

import (
	"math"
	"math/rand"
	"testing"
)

func TestGMMFitSingleComponent(t *testing.T) {
	// Generate data from a known Gaussian
	rng := rand.New(rand.NewSource(42))
	data := make([]float64, 1000)
	for i := range data {
		data[i] = rng.NormFloat64()*2.0 + 5.0 // mean=5, std=2
	}

	gmm := NewGaussianMixture1D(1)
	gmm.Fit(data)

	if math.Abs(gmm.Means[0]-5.0) > 0.2 {
		t.Errorf("expected mean ~5.0, got %f", gmm.Means[0])
	}
	if math.Abs(gmm.Variances[0]-4.0) > 0.5 {
		t.Errorf("expected variance ~4.0, got %f", gmm.Variances[0])
	}
	if math.Abs(gmm.Weights[0]-1.0) > 1e-6 {
		t.Errorf("expected weight 1.0, got %f", gmm.Weights[0])
	}
}

func TestGMMFitMultiComponent(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	data := make([]float64, 2000)

	// Bimodal distribution: half from N(0,1), half from N(10,1)
	for i := range 1000 {
		data[i] = rng.NormFloat64()
	}
	for i := 1000; i < 2000; i++ {
		data[i] = rng.NormFloat64() + 10.0
	}

	gmm := NewGaussianMixture1D(2)
	gmm.Fit(data)

	// Check that we recovered two separated means
	if gmm.Means[0] > gmm.Means[1] {
		gmm.Means[0], gmm.Means[1] = gmm.Means[1], gmm.Means[0]
		gmm.Weights[0], gmm.Weights[1] = gmm.Weights[1], gmm.Weights[0]
	}

	if math.Abs(gmm.Means[0]-0.0) > 0.5 {
		t.Errorf("expected first mean ~0.0, got %f", gmm.Means[0])
	}
	if math.Abs(gmm.Means[1]-10.0) > 0.5 {
		t.Errorf("expected second mean ~10.0, got %f", gmm.Means[1])
	}
	if math.Abs(gmm.Weights[0]-0.5) > 0.1 {
		t.Errorf("expected weights ~0.5, got %f and %f", gmm.Weights[0], gmm.Weights[1])
	}
}

func TestGMMSample(t *testing.T) {
	gmm := NewGaussianMixture1D(1)
	gmm.Weights[0] = 1.0
	gmm.Means[0] = 5.0
	gmm.Variances[0] = 1.0

	rng := rand.New(rand.NewSource(42))
	sum := 0.0
	n := 10000
	for range n {
		sum += gmm.Sample(rng)
	}
	mean := sum / float64(n)

	if math.Abs(mean-5.0) > 0.1 {
		t.Errorf("expected sample mean ~5.0, got %f", mean)
	}
}

func TestGMMFitSmallData(t *testing.T) {
	// Should not panic with very small datasets
	gmm := NewGaussianMixture1D(3)
	gmm.Fit([]float64{1.0, 2.0})

	rng := rand.New(rand.NewSource(42))
	_ = gmm.Sample(rng) // Should not panic
}

func TestGMMFitEmptyData(t *testing.T) {
	gmm := NewGaussianMixture1D(1)
	gmm.Fit(nil) // Should not panic
}
