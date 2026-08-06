package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) < tol
}

func TestSymmetricMAPE(t *testing.T) {
	tests := []struct {
		orig, res, want float64
	}{
		{100, 100, 0},
		{100, 0, 100},
		{0, 0, 0},
		{0, 50, 100},
		{100, 50, 100.0 * 50.0 / 150.0}, // abs((100-50)/(100+50)) * 100 = 33.33...
		{50, 100, 100.0 * 50.0 / 150.0},  // symmetric
	}
	for _, tt := range tests {
		got := symmetricMAPE(tt.orig, tt.res)
		if !approxEqual(got, tt.want, 0.001) {
			t.Errorf("symmetricMAPE(%g, %g) = %g, want %g", tt.orig, tt.res, got, tt.want)
		}
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		a, b []float64
		want float64
	}{
		{[]float64{1, 0, 0}, []float64{1, 0, 0}, 1.0},
		{[]float64{1, 0, 0}, []float64{0, 1, 0}, 0.0},
		{[]float64{1, 2, 3}, []float64{1, 2, 3}, 1.0},
		{[]float64{1, 2, 3}, []float64{2, 4, 6}, 1.0},
		{[]float64{}, []float64{1}, 0.0},
		{[]float64{5}, []float64{5}, 1.0},  // single element, equal
		{[]float64{5}, []float64{10}, 0.0}, // single element, not equal
	}
	for _, tt := range tests {
		got := cosineSimilarity(tt.a, tt.b)
		if !approxEqual(got, tt.want, 0.0001) {
			t.Errorf("cosineSimilarity(%v, %v) = %g, want %g", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestWassersteinDistance(t *testing.T) {
	tests := []struct {
		a, b []float64
		want float64
	}{
		// Identical distributions
		{[]float64{1, 2, 3}, []float64{1, 2, 3}, 0},
		// Shifted by 1
		{[]float64{0, 0, 0}, []float64{1, 1, 1}, 1.0},
		// Simple case: [0] vs [1]
		{[]float64{0}, []float64{1}, 1.0},
		// scipy.stats.wasserstein_distance([1,2,3,4], [2,3,4,5]) = 1.0
		{[]float64{1, 2, 3, 4}, []float64{2, 3, 4, 5}, 1.0},
		// scipy.stats.wasserstein_distance([1,1,1], [2,2,2]) = 1.0
		{[]float64{1, 1, 1}, []float64{2, 2, 2}, 1.0},
	}
	for _, tt := range tests {
		got := wassersteinDistance(tt.a, tt.b)
		if !approxEqual(got, tt.want, 0.0001) {
			t.Errorf("wassersteinDistance(%v, %v) = %g, want %g", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestExpandCompressorPrefixes(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"head_1_1", "head_5_2", "head_100_3", "tpack_simple_42_1", "tpack_vae_42_1", "other"} {
		os.Mkdir(filepath.Join(dir, d), 0o755)
	}

	tests := []struct {
		prefixes []string
		want     []string
	}{
		{[]string{"head"}, []string{"head_100_3", "head_1_1", "head_5_2"}},
		{[]string{"tpack"}, []string{"tpack_simple_42_1", "tpack_vae_42_1"}},
		{[]string{"head", "tpack"}, []string{"head_100_3", "head_1_1", "head_5_2", "tpack_simple_42_1", "tpack_vae_42_1"}},
		{[]string{"other"}, []string{"other"}},
		{[]string{"nonexistent"}, nil},
	}

	for _, tt := range tests {
		got := expandCompressorPrefixes(tt.prefixes, dir)
		if len(got) != len(tt.want) {
			t.Errorf("expandCompressorPrefixes(%v) = %v, want %v", tt.prefixes, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("expandCompressorPrefixes(%v)[%d] = %q, want %q", tt.prefixes, i, got[i], tt.want[i])
			}
		}
	}
}

func TestGraphEditDistance(t *testing.T) {
	// Identical graphs
	g1 := graphData{
		nodes: []string{"A", "B"},
		edges: map[string]float64{"A->B": 10},
	}
	d := graphEditDistance(g1, g1)
	if d != 0 {
		t.Errorf("identical graphs: distance = %g, want 0", d)
	}

	// Extra node
	g2 := graphData{
		nodes: []string{"A", "B", "C"},
		edges: map[string]float64{"A->B": 10},
	}
	d = graphEditDistance(g1, g2)
	if d != 1 {
		t.Errorf("extra node: distance = %g, want 1", d)
	}

	// Different edge weight
	g3 := graphData{
		nodes: []string{"A", "B"},
		edges: map[string]float64{"A->B": 15},
	}
	d = graphEditDistance(g1, g3)
	if d != 5 {
		t.Errorf("weight diff: distance = %g, want 5", d)
	}
}

func TestExtractRCAAnswer(t *testing.T) {
	tests := []struct {
		service, want string
	}{
		{"checkoutservice_cpu", "checkoutservice"},
		{"checkoutservice_delay", "checkoutservice"},
		{"checkoutservice_disk", "checkoutservice"},
		{"checkoutservice_loss", "checkoutservice"},
		{"checkoutservice_mem", "checkoutservice"},
		{"checkoutservice_socket", "checkoutservice"},
		{"ts-auth-service_cpu", "ts-auth-service"},
		{"202510", "202510"},         // no fault suffix
		{"simple", "simple"},         // no fault suffix
		{"my_service", "my_service"}, // unknown suffix, not a fault
	}
	for _, tt := range tests {
		got := extractRCAAnswer(tt.service)
		if got != tt.want {
			t.Errorf("extractRCAAnswer(%q) = %q, want %q", tt.service, got, tt.want)
		}
	}
}

func TestAcAtK(t *testing.T) {
	ranks := []string{"svcA", "svcB", "svcC", "svcD", "svcE"}

	if !acAtK("svcA", ranks, 1) {
		t.Error("svcA should be in top 1")
	}
	if acAtK("svcB", ranks, 1) {
		t.Error("svcB should not be in top 1")
	}
	if !acAtK("svcB", ranks, 2) {
		t.Error("svcB should be in top 2")
	}
	if !acAtK("svcE", ranks, 5) {
		t.Error("svcE should be in top 5")
	}
	if acAtK("svcX", ranks, 5) {
		t.Error("svcX should not be in any rank")
	}
	// k > len(ranks)
	if acAtK("svcX", ranks, 10) {
		t.Error("svcX should not be found even with k > len")
	}
}
