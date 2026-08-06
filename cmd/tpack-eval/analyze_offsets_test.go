package main

import (
	"math"
	mathrand "math/rand"
	"testing"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
)

// TestOffsetOLSConsistency verifies that the OLS regression in analyze_offsets
// (offsetRegAccum) and the pipeline (StatisticalDependentAttributePredictor) produce
// equivalent predictions given the same input data.
func TestOffsetOLSConsistency(t *testing.T) {
	config := tpackmodel.DefaultConfig()
	config.OffsetValue = "absolute"
	config.OffsetModel = "regression"
	rng := mathrand.New(mathrand.NewSource(42))
	pred := tpackmodel.NewStatisticalDependentAttributePredictor(config, 0, rng)

	var analyzeAccumGap, analyzeAccumDur offsetRegAccum

	type sample struct {
		parentDur float64
		childIdx  float64
		gap       float64
		dur       float64
	}

	var samples []sample
	parentDurs := []float64{1000, 2000, 5000, 10000, 50000}
	for _, pd := range parentDurs {
		for ci := 0.0; ci <= 1.0; ci += 0.25 {
			gap := 0.1*pd + 100*ci
			dur := 0.3 * pd
			samples = append(samples, sample{pd, ci, gap, dur})

			logGap := math.Log(gap + 1)
			logDur := math.Log(dur + 1)

			analyzeAccumGap.add(ci, logGap)
			analyzeAccumDur.add(ci, logDur)

			pred.AddSample(0, 1, logGap, logDur, pd, ci, nil)
		}
	}
	pred.FinalizeFit()

	gapCoeffs := analyzeAccumGap.solve()
	durCoeffs := analyzeAccumDur.solve()

	// Compare predictions at various query points
	tol := 1e-3
	queryDurs := []float64{500, 3000, 20000, 80000}
	queryIdxs := []float64{0.0, 0.5, 1.0}

	for _, qd := range queryDurs {
		for _, qi := range queryIdxs {
			// analyze-offsets prediction
			analyzeGap := reconstructGap("logGap", gapCoeffs, qd, qi)
			analyzeDur := reconstructDur("logDur", durCoeffs, qd, qi)

			// pipeline prediction (deterministic in log mode)
			results := pred.SampleBatch(
				[]tpackmodel.MetadataSampleRequest{{
					ParentFeature:      tpackmodel.NodeFeature{NodeIdx: 0},
					ChildFeature:       tpackmodel.NodeFeature{NodeIdx: 1},
					ParentDuration:     qd,
					NormalizedChildIdx: qi,
				}},
				nil, nil, pred.Rng,
			)

			// Pipeline returns log-space values; convert to absolute
			pipelineGap := math.Exp(clamp(results[0].GapRatio, 0, 30)) - 1
			pipelineGap = clamp(pipelineGap, 0, qd)
			pipelineDur := math.Exp(clamp(results[0].DurationRatio, 0, 30)) - 1
			pipelineDur = clamp(pipelineDur, 0, qd)

			if math.Abs(analyzeGap-pipelineGap) > tol {
				t.Errorf("parentDur=%.0f childIdx=%.1f: gap mismatch: analyze=%.3f, pipeline=%.3f",
					qd, qi, analyzeGap, pipelineGap)
			}
			if math.Abs(analyzeDur-pipelineDur) > tol {
				t.Errorf("parentDur=%.0f childIdx=%.1f: dur mismatch: analyze=%.3f, pipeline=%.3f",
					qd, qi, analyzeDur, pipelineDur)
			}
		}
	}
}

// TestLogReconstructionMatchesPipeline verifies that analyze-offsets'
// reconstructGap/reconstructDur produce the same absolute values as the
// pipeline's exp(clamp(pred, 0, 30)) - 1 + clamp to parentDur.
func TestLogReconstructionMatchesPipeline(t *testing.T) {
	coeffs := offsetRegCoeffs{Beta0: 2.0, Beta1: -0.1}

	cases := []struct {
		name      string
		parentDur float64
		childIdx  float64
	}{
		{"normal", 10000, 0.5},
		{"small_parent", 100, 0.0},
		{"large_parent", 1000000, 1.0},
		{"zero_idx", 5000, 0.0},
	}

	for _, tc := range cases {
		analyzeGap := reconstructGap("logGap", coeffs, tc.parentDur, tc.childIdx)
		analyzeDur := reconstructDur("logDur", coeffs, tc.parentDur, tc.childIdx)

		// Replicate pipeline: same formula
		pred := coeffs.Beta0 + coeffs.Beta1*tc.childIdx
		pipelineVal := math.Exp(clamp(pred, 0, 30)) - 1
		pipelineGap := clamp(pipelineVal, 0, tc.parentDur)
		pipelineDur := clamp(pipelineVal, 0, tc.parentDur)

		if math.Abs(analyzeGap-pipelineGap) > 1e-6 {
			t.Errorf("%s: gap mismatch: analyze=%.6f, pipeline=%.6f", tc.name, analyzeGap, pipelineGap)
		}
		if math.Abs(analyzeDur-pipelineDur) > 1e-6 {
			t.Errorf("%s: dur mismatch: analyze=%.6f, pipeline=%.6f", tc.name, analyzeDur, pipelineDur)
		}
	}
}

// TestLogReconstructionNegativePrediction verifies that negative OLS
// predictions in log space are clamped to 0, producing gap/dur = 0.
func TestLogReconstructionNegativePrediction(t *testing.T) {
	coeffs := offsetRegCoeffs{Beta0: -5.0, Beta1: 0.0}
	parentDur := 100.0
	// pred = -5.0 → clamp to 0 → exp(0)-1 = 0

	gap := reconstructGap("logGap", coeffs, parentDur, 0)
	if gap != 0 {
		t.Errorf("expected gap=0 for negative log prediction, got %.6f", gap)
	}

	dur := reconstructDur("logDur", coeffs, parentDur, 0)
	if dur != 0 {
		t.Errorf("expected dur=0 for negative log prediction, got %.6f", dur)
	}
}

// TestLogReconstructionOverflow verifies that very large OLS predictions
// are clamped to parentDur.
func TestLogReconstructionOverflow(t *testing.T) {
	coeffs := offsetRegCoeffs{Beta0: 50.0, Beta1: 0.0}
	parentDur := 10000.0

	gap := reconstructGap("logGap", coeffs, parentDur, 0)
	if gap != parentDur {
		t.Errorf("expected gap=parentDur for overflow, got %.6f", gap)
	}
}

// TestReconstructWithBoundsRatio verifies that reconstructWithBounds applies
// bounds and proportional scaling for ratio mode.
func TestReconstructWithBoundsRatio(t *testing.T) {
	parentDur := 10000.0

	// Coefficients where ratio predictions are 0.8 each (would exceed parentDur)
	gapCoeffs := offsetRegCoeffs{Beta0: 0.8, Beta1: 0.0}
	durCoeffs := offsetRegCoeffs{Beta0: 0.8, Beta1: 0.0}

	gap, dur := reconstructWithBounds(
		gapCoeffs, durCoeffs,
		parentDur, 0,
		1, parentDur, 0, parentDur, // wide bounds
		false,
	)

	// After proportional scaling: each should be 0.5 * parentDur
	if math.Abs(gap+dur-parentDur) > 1e-6 {
		t.Errorf("gap+dur=%.1f, expected %.1f", gap+dur, parentDur)
	}
	if math.Abs(gap-5000) > 1e-6 || math.Abs(dur-5000) > 1e-6 {
		t.Errorf("expected 5000/5000, got %.1f/%.1f", gap, dur)
	}
}

// TestReconstructWithBoundsLog verifies that reconstructWithBounds applies
// bounds for log mode.
func TestReconstructWithBoundsLog(t *testing.T) {
	parentDur := 10000.0

	// Coefficients producing exp(8.987)-1 ≈ 7999
	gapCoeffs := offsetRegCoeffs{Beta0: 8.987, Beta1: 0.0}
	durCoeffs := offsetRegCoeffs{Beta0: 8.987, Beta1: 0.0}

	// Tight bounds: dur must be in [100, 500]
	gap, dur := reconstructWithBounds(
		gapCoeffs, durCoeffs,
		parentDur, 0,
		100, 500, 0, parentDur,
		true,
	)

	if dur != 500 {
		t.Errorf("expected dur clamped to 500, got %.1f", dur)
	}
	// Gap should be clamped to parentDur (wide gap bounds)
	if gap > parentDur {
		t.Errorf("expected gap <= parentDur, got %.1f", gap)
	}
}

// TestReconstructWithBoundsMinDurationFloor verifies the minimum duration floor.
func TestReconstructWithBoundsMinDurationFloor(t *testing.T) {
	// Coefficients producing sub-microsecond values
	coeffs := offsetRegCoeffs{Beta0: 0.01, Beta1: 0.0}

	_, dur := reconstructWithBounds(
		coeffs, coeffs,
		10000, 0,
		0, math.Inf(1), 0, math.Inf(1),
		true,
	)

	if dur < 1.0 {
		t.Errorf("expected dur >= 1.0, got %.6f", dur)
	}
}

// MPQ tests are now in pkg/tpackmodel/mpq_test.go
