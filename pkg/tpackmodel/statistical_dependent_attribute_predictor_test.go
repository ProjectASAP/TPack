package tpackmodel

import (
	"math"
	"math/rand"
	"testing"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

func TestStatisticalPredictorBasic(t *testing.T) {
	config := DefaultConfig()
	rng := rand.New(rand.NewSource(42))
	p := NewStatisticalDependentAttributePredictor(config, 1, rng)

	// Add training samples
	for range 100 {
		p.AddSample(0, 1, 0.3, 0.5, 5000, 0.5, []int{1})
	}
	p.FinalizeFit()

	// Sample
	requests := []MetadataSampleRequest{
		{ParentFeature: NodeFeature{NodeIdx: 0}, ChildFeature: NodeFeature{NodeIdx: 1}, ParentDuration: 5000, NormalizedChildIdx: 0.5},
	}
	results := p.SampleBatch(requests, nil, nil, rng)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.GapRatio < 0 || r.GapRatio > 1 {
		t.Errorf("gap ratio %f out of [0, 1]", r.GapRatio)
	}
	if r.DurationRatio < 0 || r.DurationRatio > 1 {
		t.Errorf("duration ratio %f out of [0, 1]", r.DurationRatio)
	}
}

func TestStatisticalPredictorProtobufRoundtrip(t *testing.T) {
	config := DefaultConfig()
	rng := rand.New(rand.NewSource(42))
	p := NewStatisticalDependentAttributePredictor(config, 1, rng)

	for range 50 {
		p.AddSample(0, 1, 0.3, 0.5, 5000, 0.5, []int{1})
		p.AddSample(2, 3, 0.1, 0.8, 3000, 0.0, []int{0})
	}
	p.FinalizeFit()

	// Serialize
	models := &pb.TPackModels{}
	p.SaveStateDict(models)
	data, err := proto.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}

	// Deserialize
	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models2); err != nil {
		t.Fatal(err)
	}

	rng2 := rand.New(rand.NewSource(42))
	p2 := NewStatisticalDependentAttributePredictor(config, 1, rng2)
	p2.LoadStateDict(models2)

	// Verify regression survived roundtrip
	key := nodePairKey{0, 1}
	if _, ok := p2.Stats[key]; !ok {
		t.Fatal("expected stats for key (0, 1)")
	}

	stats := p2.Stats[key]

	// Regression beta0 should be close to the mean since all samples had same predictors
	if math.Abs(stats.GapRegression.Beta0-p.Stats[key].GapRegression.Beta0) > 1e-6 {
		t.Errorf("gap beta0 mismatch: got %f, want %f", stats.GapRegression.Beta0, p.Stats[key].GapRegression.Beta0)
	}
}

func TestStatisticalPredictorBoundsEnforcement(t *testing.T) {
	config := DefaultConfig()
	rng := rand.New(rand.NewSource(42))
	p := NewStatisticalDependentAttributePredictor(config, 0, rng)

	for range 100 {
		p.AddSample(0, 1, 0.5, 0.5, 1000, 0.5, nil)
	}
	p.FinalizeFit()

	// Request with tight bounds
	requests := []MetadataSampleRequest{
		{ParentFeature: NodeFeature{NodeIdx: 0}, ChildFeature: NodeFeature{NodeIdx: 1}, ParentDuration: 1000, NormalizedChildIdx: 0.5},
	}
	durBounds := []MinMax{{Min: 100, Max: 200}} // Duration must be in [100, 200]
	gapBounds := []MinMax{{Min: 300, Max: 400}} // Gap must be in [300, 400]

	results := p.SampleBatch(requests, durBounds, gapBounds, rng)

	r := results[0]
	dur := r.DurationRatio * 1000
	gap := r.GapRatio * 1000

	if dur < 100 || dur > 200 {
		t.Errorf("duration %f not in bounds [100, 200]", dur)
	}
	if gap < 300 || gap > 400 {
		t.Errorf("gap %f not in bounds [300, 400]", gap)
	}
}

func TestStatisticalPredictorRegressionSensitivity(t *testing.T) {
	// Regression uses childIdx only (no parent duration conditioning).
	// Train with samples where gapRatio depends on childIdx:
	// Early children (low idx) → higher gapRatio, later children → lower gapRatio
	config := DefaultConfig()
	rng := rand.New(rand.NewSource(42))
	p := NewStatisticalDependentAttributePredictor(config, 0, rng)

	for i := range 200 {
		childIdx := float64(i) / 199.0
		// gapRatio decreases with childIdx: 0.6 at idx=0, 0.1 at idx=1
		gapRatio := 0.6 - 0.5*childIdx
		p.AddSample(0, 1, gapRatio, 0.3, 5000, childIdx, nil)
	}
	p.FinalizeFit()

	stats := p.Stats[nodePairKey{0, 1}]

	// Beta2 should be negative (higher childIdx → lower gapRatio)
	if stats.GapRegression.Beta1 >= 0 {
		t.Errorf("expected negative gap beta2, got %f", stats.GapRegression.Beta1)
	}

	// Sample at early vs late childIdx and check that gap ratios differ
	earlyReq := []MetadataSampleRequest{{
		ParentFeature: NodeFeature{NodeIdx: 0}, ChildFeature: NodeFeature{NodeIdx: 1},
		ParentDuration: 5000, NormalizedChildIdx: 0.1,
	}}
	lateReq := []MetadataSampleRequest{{
		ParentFeature: NodeFeature{NodeIdx: 0}, ChildFeature: NodeFeature{NodeIdx: 1},
		ParentDuration: 5000, NormalizedChildIdx: 0.9,
	}}

	// Average over many samples
	nSamples := 500
	sumEarly, sumLate := 0.0, 0.0
	for range nSamples {
		er := p.SampleBatch(earlyReq, nil, nil, rng)
		lr := p.SampleBatch(lateReq, nil, nil, rng)
		sumEarly += er[0].GapRatio
		sumLate += lr[0].GapRatio
	}
	avgEarly := sumEarly / float64(nSamples)
	avgLate := sumLate / float64(nSamples)

	if avgEarly <= avgLate {
		t.Errorf("expected early-child gap (%.3f) > late-child gap (%.3f)", avgEarly, avgLate)
	}
}

func TestStatisticalPredictorPercentileBasic(t *testing.T) {
	config := DefaultConfig()
	config.OffsetModel = "percentile"
	rng := rand.New(rand.NewSource(42))
	p := NewStatisticalDependentAttributePredictor(config, 1, rng)

	// Add training samples with absolute values (as in percentile mode)
	for range 200 {
		absGap := 500.0 + rng.Float64()*1000.0
		absDur := 1000.0 + rng.Float64()*1500.0
		parentDur := 5000.0 + rng.Float64()*5000.0
		p.AddSample(0, 1, absGap, absDur, parentDur, 0.5, []int{0})
	}
	p.FinalizeFit()

	// Verify stats were created with percentile fields
	stats := p.Stats[nodePairKey{0, 1}]
	if stats == nil {
		t.Fatal("expected stats for pair (0, 1)")
	}
	if len(stats.GapPercentiles) != 21 {
		t.Errorf("expected 21 gap percentiles, got %d", len(stats.GapPercentiles))
	}
	if len(stats.DurPercentiles) != 21 {
		t.Errorf("expected 21 dur percentiles, got %d", len(stats.DurPercentiles))
	}

	// Sample and verify output is in valid range
	requests := []MetadataSampleRequest{
		{ParentFeature: NodeFeature{NodeIdx: 0}, ChildFeature: NodeFeature{NodeIdx: 1}, ParentDuration: 5000, NormalizedChildIdx: 0.5},
	}
	for range 100 {
		results := p.SampleBatch(requests, nil, nil, rng)
		r := results[0]
		if r.GapRatio < 0 || r.GapRatio > 1 {
			t.Errorf("gap ratio %f out of [0, 1]", r.GapRatio)
		}
		if r.DurationRatio < 0 || r.DurationRatio > 1 {
			t.Errorf("duration ratio %f out of [0, 1]", r.DurationRatio)
		}
		if r.GapRatio+r.DurationRatio > 1.0+1e-9 {
			t.Errorf("gap+dur ratio %f > 1", r.GapRatio+r.DurationRatio)
		}
	}
}

func TestStatisticalPredictorPercentileRoundtrip(t *testing.T) {
	config := DefaultConfig()
	config.OffsetModel = "percentile"
	rng := rand.New(rand.NewSource(42))
	p := NewStatisticalDependentAttributePredictor(config, 0, rng)

	for range 100 {
		p.AddSample(0, 1, 200.0, 400.0, 5000, 0.5, nil)
	}
	p.FinalizeFit()

	// Serialize
	models := &pb.TPackModels{}
	p.SaveStateDict(models)
	data, err := proto.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}

	// Deserialize
	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models2); err != nil {
		t.Fatal(err)
	}

	rng2 := rand.New(rand.NewSource(42))
	p2 := NewStatisticalDependentAttributePredictor(config, 0, rng2)
	p2.LoadStateDict(models2)

	key := nodePairKey{0, 1}
	s1 := p.Stats[key]
	s2 := p2.Stats[key]
	if s2 == nil {
		t.Fatal("expected stats after roundtrip")
	}

	// Verify percentiles survived roundtrip
	if len(s2.GapPercentiles) != len(s1.GapPercentiles) {
		t.Errorf("gap percentile count: got %d, want %d", len(s2.GapPercentiles), len(s1.GapPercentiles))
	}
	for i := range s1.GapPercentiles {
		if math.Abs(s1.GapPercentiles[i]-s2.GapPercentiles[i]) > 1e-10 {
			t.Errorf("gap percentile[%d]: got %f, want %f", i, s2.GapPercentiles[i], s1.GapPercentiles[i])
		}
	}
	if len(s1.DurPercentiles) != len(s2.DurPercentiles) {
		t.Errorf("dur percentile count: got %d, want %d", len(s2.DurPercentiles), len(s1.DurPercentiles))
	}
}

func TestStatisticalPredictorFewSamplesFallback(t *testing.T) {
	config := DefaultConfig()
	rng := rand.New(rand.NewSource(42))
	p := NewStatisticalDependentAttributePredictor(config, 0, rng)

	// Only 3 samples — below regression threshold (5)
	p.AddSample(0, 1, 0.3, 0.5, 5000, 0.5, nil)
	p.AddSample(0, 1, 0.4, 0.6, 3000, 0.3, nil)
	p.AddSample(0, 1, 0.2, 0.4, 8000, 0.8, nil)
	p.FinalizeFit()

	// Should still sample without panicking (regression fallback gives unconditional mean)
	requests := []MetadataSampleRequest{{
		ParentFeature: NodeFeature{NodeIdx: 0}, ChildFeature: NodeFeature{NodeIdx: 1},
		ParentDuration: 5000, NormalizedChildIdx: 0.5,
	}}
	results := p.SampleBatch(requests, nil, nil, rng)
	if results[0].GapRatio < 0 || results[0].GapRatio > 1 {
		t.Errorf("gap=%v out of range", results[0].GapRatio)
	}
}
