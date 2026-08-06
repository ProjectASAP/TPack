package main

import (
	"math"
	"math/rand"
	"runtime"
	"sync"
)

// sifterNumWorkers is the degree of vocab-loop parallelism used inside
// forward/backward. Chosen at program start to avoid repeated syscalls.
var sifterNumWorkers = max(1, runtime.NumCPU())

// sifterMinWorkPerWorker is the minimum number of multiply-accumulate operations
// a goroutine must be given before it is worth spawning. Spawning a goroutine and
// joining it through a WaitGroup barrier costs on the order of a microsecond; a
// multiply-accumulate costs on the order of a nanosecond. Below roughly 10^4
// operations per worker the fan-out is pure loss.
//
// This matters because the vocabulary is small here: otel-demo has 212 span types
// and RE2 has 152, so vocabSize*contextSize stays under 10^4 and the entire vocab
// loop is a few microseconds of work. Splitting that across runtime.NumCPU()
// workers made the pass slower the more cores the machine had, which is the
// opposite of the intent. Sizing by work keeps the parallel path for the case it
// was written for -- Uber's 15,615 span types, where vocabSize*contextSize is
// ~6*10^5 -- and collapses to serial everywhere else.
const sifterMinWorkPerWorker = 16384

// vocabSplit returns chunk boundaries bounds[0..n] so worker w owns
// [bounds[w], bounds[w+1]). Lengths are as even as possible. workPerEntry is the
// number of operations the split loop performs per vocabulary entry.
func vocabSplit(vocabSize, workPerEntry int) []int {
	n := vocabSize * workPerEntry / sifterMinWorkPerWorker
	n = max(min(n, min(sifterNumWorkers, vocabSize)), 1)
	bounds := make([]int, n+1)
	base := vocabSize / n
	rem := vocabSize % n
	off := 0
	for i := range n {
		bounds[i] = off
		off += base
		if i < rem {
			off++
		}
	}
	bounds[n] = vocabSize
	return bounds
}

// sifterModel implements the paragraph vector model from Sifter (Las-Casas et al., SoCC 2019).
// It learns common trace execution patterns and computes sampling probabilities
// that bias toward unusual (high prediction error) traces.
type sifterModel struct {
	// Model parameters, stored flat rather than as slices-of-slices. The inner
	// loops walk one row at a time, so a [][]float64 costs a pointer dereference
	// and a bounds check per row and scatters the rows across the heap. Flat
	// backing arrays keep the whole matrix contiguous and prefetch-friendly.
	//   embeddings: vocabSize x P,           row i at [i*P : (i+1)*P]
	//   weights:    vocabSize x (N-1)*P,     row j at [j*contextSize : (j+1)*contextSize]
	embeddings []float64
	weights    []float64

	vocabSize int
	P         int     // embedding dimension (default 10)
	N         int     // path length (default 5)
	lr        float64 // learning rate (default 0.01)

	// Sampling state
	recentLosses []float64 // sliding window of k most recent trace losses
	k            int       // window size (default 50)
	alpha        float64   // target sampling rate (1/rate)

	// Scratch buffers (reused across calls to avoid allocation)
	concat  []float64
	logits  []float64
	probs   []float64
	dConcat []float64

	// Per-worker dConcat buffers for parallel backward reduction.
	workerDConcat [][]float64
}

// newSifterModel creates a Sifter model with Xavier-initialized weights.
func newSifterModel(vocabSize int, alpha float64, rng *rand.Rand) *sifterModel {
	const P = 10
	const N = 5
	contextSize := (N - 1) * P // 4 * 10 = 40

	m := &sifterModel{
		vocabSize: vocabSize,
		P:         P,
		N:         N,
		lr:        0.01,
		k:         50,
		alpha:     alpha,
		concat:    make([]float64, contextSize),
		logits:    make([]float64, vocabSize),
		probs:     make([]float64, vocabSize),
		dConcat:   make([]float64, contextSize),
	}
	m.workerDConcat = make([][]float64, sifterNumWorkers)
	for w := range m.workerDConcat {
		m.workerDConcat[w] = make([]float64, contextSize)
	}

	// Xavier initialization. Iterating the flat arrays in row-major order draws
	// from rng in exactly the same sequence the previous row-by-row loops did,
	// so initialization stays bit-identical.
	embScale := math.Sqrt(2.0 / float64(vocabSize+P))
	m.embeddings = make([]float64, vocabSize*P)
	for i := range m.embeddings {
		m.embeddings[i] = rng.NormFloat64() * embScale
	}

	wScale := math.Sqrt(2.0 / float64(contextSize+vocabSize))
	m.weights = make([]float64, vocabSize*contextSize)
	for i := range m.weights {
		m.weights[i] = rng.NormFloat64() * wScale
	}

	return m
}

// runVocabPhase applies fn to each worker's vocab chunk. With a single chunk it
// calls fn inline: the whole point of sizing the split by work is that small
// vocabularies should not pay for goroutine creation or a WaitGroup barrier at
// all, and spawning one goroutine to do all the work would keep exactly the cost
// we are trying to remove.
func runVocabPhase(bounds []int, fn func(w, js, je int)) {
	nWorkers := len(bounds) - 1
	if nWorkers <= 1 {
		fn(0, bounds[0], bounds[len(bounds)-1])
		return
	}
	var wg sync.WaitGroup
	wg.Add(nWorkers)
	for w := range nWorkers {
		go func(w int) {
			defer wg.Done()
			fn(w, bounds[w], bounds[w+1])
		}(w)
	}
	wg.Wait()
}

// forward computes the forward pass: embed context labels, concatenate, linear, softmax.
// Returns probs and populates m.concat as a side effect (used by backward).
// The vocab-outer loops are split across workers only when the vocabulary is
// large enough to be worth it (see sifterMinWorkPerWorker).
func (m *sifterModel) forward(context [4]int32) {
	contextSize := (m.N - 1) * m.P

	// Look up and concatenate embeddings
	for c := range 4 {
		off := int(context[c]) * m.P
		copy(m.concat[c*m.P:(c+1)*m.P], m.embeddings[off:off+m.P])
	}

	// Sized by the linear layer, the dominant phase; the softmax phases reuse the
	// same bounds so the three passes agree on chunk boundaries.
	bounds := vocabSplit(m.vocabSize, contextSize)
	nWorkers := len(bounds) - 1
	concat := m.concat[:contextSize]

	// Linear layer: logits[j] = dot(weights[j], concat). Per-worker localMax.
	localMax := make([]float64, nWorkers)
	runVocabPhase(bounds, func(w, js, je int) {
		maxL := math.Inf(-1)
		for j := js; j < je; j++ {
			base := j * contextSize
			ww := m.weights[base : base+contextSize]
			dot := 0.0
			for i, cv := range concat {
				dot += ww[i] * cv
			}
			m.logits[j] = dot
			if dot > maxL {
				maxL = dot
			}
		}
		localMax[w] = maxL
	})
	maxLogit := math.Inf(-1)
	for _, mx := range localMax {
		if mx > maxLogit {
			maxLogit = mx
		}
	}

	// Softmax: compute exp(logit - maxLogit), then normalize. Parallel per-worker sums.
	localSum := make([]float64, nWorkers)
	runVocabPhase(bounds, func(w, js, je int) {
		s := 0.0
		for j := js; j < je; j++ {
			e := math.Exp(m.logits[j] - maxLogit)
			m.probs[j] = e
			s += e
		}
		localSum[w] = s
	})
	sumExp := 0.0
	for _, s := range localSum {
		sumExp += s
	}
	invSum := 1.0 / sumExp
	runVocabPhase(bounds, func(_, js, je int) {
		for j := js; j < je; j++ {
			m.probs[j] *= invSum
		}
	})
}

// processPath runs forward + backward on a single path, returning the cross-entropy loss.
// Backward fuses dConcat accumulation with the weight update (dConcat reads the OLD
// weights that the update then overwrites — safe because both are indexed by the
// same (j, i) pair). Parallel across the vocab axis.
func (m *sifterModel) processPath(context [4]int32, target int32) float64 {
	m.forward(context)

	// Cross-entropy loss
	prob := m.probs[target]
	if prob < 1e-30 {
		prob = 1e-30
	}
	loss := -math.Log(prob)

	contextSize := (m.N - 1) * m.P
	lr := m.lr
	bounds := vocabSplit(m.vocabSize, contextSize)
	nWorkers := len(bounds) - 1
	concat := m.concat[:contextSize]

	for w := range nWorkers {
		buf := m.workerDConcat[w]
		for i := range contextSize {
			buf[i] = 0
		}
	}

	// Fused backward: per-worker dConcat buffers + in-place weight update.
	runVocabPhase(bounds, func(w, js, je int) {
		buf := m.workerDConcat[w]
		for j := js; j < je; j++ {
			dL := m.probs[j]
			if int32(j) == target {
				dL -= 1.0
			}
			lrDL := lr * dL
			base := j * contextSize
			ww := m.weights[base : base+contextSize]
			for i, cv := range concat {
				buf[i] += dL * ww[i] // dConcat += old weight × dL
				ww[i] -= lrDL * cv   // weight update (concat is read-only)
			}
		}
	})

	// Reduce per-worker dConcat buffers
	for i := range contextSize {
		m.dConcat[i] = 0
	}
	for w := range nWorkers {
		buf := m.workerDConcat[w]
		for i := range contextSize {
			m.dConcat[i] += buf[i]
		}
	}

	// Update embeddings from dConcat (only 4 rows, negligible — stay serial)
	for c := range 4 {
		off := int(context[c]) * m.P
		emb := m.embeddings[off : off+m.P]
		for p := range emb {
			emb[p] -= lr * m.dConcat[c*m.P+p]
		}
	}

	return loss
}

// samplingProbability computes the sampling probability for a trace given its mean loss.
// Implements the sliding window scheme from Sifter paper section 5.3.
func (m *sifterModel) samplingProbability(traceLoss float64) float64 {
	// During warmup (< k traces seen), sample everything
	if len(m.recentLosses) < m.k {
		m.recentLosses = append(m.recentLosses, traceLoss)
		return 1.0
	}

	// Find min of recent losses
	minLoss := m.recentLosses[0]
	for _, l := range m.recentLosses[1:] {
		if l < minLoss {
			minLoss = l
		}
	}

	// Compute weights: w_i = loss_i - minLoss
	sumW := 0.0
	for _, l := range m.recentLosses {
		sumW += l - minLoss
	}
	wNew := traceLoss - minLoss

	// Sampling probability
	var p float64
	denom := sumW + wNew
	if denom < 1e-30 {
		// All losses identical -> sample at base rate
		p = m.alpha
	} else {
		p = (wNew / denom) * float64(m.k+1) * m.alpha
	}

	// Clamp to [0, 1]
	if p > 1.0 {
		p = 1.0
	}
	if p < 0.0 {
		p = 0.0
	}

	// Update sliding window (drop oldest, append new)
	copy(m.recentLosses, m.recentLosses[1:])
	m.recentLosses[m.k-1] = traceLoss

	return p
}
