package main

import (
	"math"
	"math/rand"
	"testing"
)

func TestKruskalWallisH(t *testing.T) {
	// Two groups with known values:
	// Group A: [1, 2, 3] → ranks [1, 2, 3], mean rank = 2
	// Group B: [4, 5, 6] → ranks [4, 5, 6], mean rank = 5
	// n = 6, grand mean rank = 3.5
	// H = (12 / (6*7)) × (3*(2-3.5)² + 3*(5-3.5)²) = (12/42)*(3*2.25 + 3*2.25)
	//   = 0.2857 * 13.5 = 3.857
	groups := [][]float64{
		{1, 2, 3},
		{4, 5, 6},
	}
	H, n := kruskalWallisH(groups)
	if n != 6 {
		t.Fatalf("expected n=6, got %d", n)
	}
	expected := 3.857
	if math.Abs(H-expected) > 0.01 {
		t.Errorf("expected H≈%.3f, got %.3f", expected, H)
	}
}

func TestKruskalWallisHTies(t *testing.T) {
	// Groups with ties
	groups := [][]float64{
		{1, 1, 2},
		{3, 3, 4},
	}
	H, n := kruskalWallisH(groups)
	if n != 6 {
		t.Fatalf("expected n=6, got %d", n)
	}
	// With ties, H should be corrected upward slightly
	if H <= 0 {
		t.Error("expected H > 0 for separated groups with ties")
	}
	t.Logf("H with ties = %.4f", H)
}

func TestEtaSquaredHighEffect(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	// Group A: durations ~100 (small ratios)
	groupA := make([]float64, 200)
	for i := range groupA {
		groupA[i] = 0.05 + rng.Float64()*0.05 // 0.05–0.10
	}

	// Group B: durations ~1000 (large ratios)
	groupB := make([]float64, 200)
	for i := range groupB {
		groupB[i] = 0.8 + rng.Float64()*0.1 // 0.80–0.90
	}

	groups := [][]float64{groupA, groupB}
	H, n := kruskalWallisH(groups)
	eta2 := H / float64(n-1)

	t.Logf("High effect: H=%.2f, n=%d, η²=%.4f", H, n, eta2)
	if eta2 < 0.5 {
		t.Errorf("expected η² > 0.5 for strongly separated groups, got %.4f", eta2)
	}
}

func TestEtaSquaredNoEffect(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	// All groups from same distribution
	groups := make([][]float64, 3)
	for gi := range groups {
		groups[gi] = make([]float64, 100)
		for i := range groups[gi] {
			groups[gi][i] = rng.Float64()
		}
	}

	H, n := kruskalWallisH(groups)
	eta2 := H / float64(n-1)

	t.Logf("No effect: H=%.2f, n=%d, η²=%.4f", H, n, eta2)
	if eta2 > 0.05 {
		t.Errorf("expected η² ≈ 0 for same-distribution groups, got %.4f", eta2)
	}
}

func TestEtaSquaredSingleGroup(t *testing.T) {
	// Single group with ≥5 samples — should return 0 from etaSquared
	groups := [][]float64{
		{0.1, 0.2, 0.3, 0.4, 0.5, 0.6},
	}
	eta2 := etaSquared(groups)
	if eta2 != 0 {
		t.Errorf("expected η²=0 for single group, got %.4f", eta2)
	}
}

func TestEtaSquaredSmallGroups(t *testing.T) {
	// Two groups but one has < 5 samples — should return 0
	groups := [][]float64{
		{0.1, 0.2, 0.3, 0.4, 0.5},
		{0.9, 0.8, 0.7}, // only 3 samples
	}
	eta2 := etaSquared(groups)
	if eta2 != 0 {
		t.Errorf("expected η²=0 when a group has < 5 samples, got %.4f", eta2)
	}
}
