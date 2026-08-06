package tpackmodel

import (
	"fmt"
	"sort"
)

// NodeEncoder maps SpanFeatures to integer indices and back.
type NodeEncoder struct {
	featureToIdx map[SpanFeature]int32
	idxToFeature []SpanFeature
	isFitted     bool
}

// NewNodeEncoder creates a new empty NodeEncoder.
func NewNodeEncoder() *NodeEncoder {
	return &NodeEncoder{
		featureToIdx: make(map[SpanFeature]int32),
	}
}

// Fit learns the vocabulary from a list of SpanFeatures.
// Features are sorted by their string representation for deterministic ordering.
func (ne *NodeEncoder) Fit(features []SpanFeature) {
	// Deduplicate
	seen := make(map[SpanFeature]struct{}, len(features))
	unique := make([]SpanFeature, 0, len(features))
	for _, f := range features {
		if _, ok := seen[f]; !ok {
			seen[f] = struct{}{}
			unique = append(unique, f)
		}
	}

	// Sort by string representation for deterministic ordering
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].String() < unique[j].String()
	})

	ne.idxToFeature = unique
	ne.featureToIdx = make(map[SpanFeature]int32, len(unique))
	for i, f := range unique {
		ne.featureToIdx[f] = int32(i)
	}
	ne.isFitted = true
}

// FitFromVocabulary restores the encoder from a pre-ordered vocabulary of strings.
// Used when loading from protobuf node_vocabulary field.
func (ne *NodeEncoder) FitFromVocabulary(vocab []string) {
	ne.idxToFeature = make([]SpanFeature, len(vocab))
	ne.featureToIdx = make(map[SpanFeature]int32, len(vocab))
	for i, s := range vocab {
		f := ParseSpanFeature(s)
		ne.idxToFeature[i] = f
		ne.featureToIdx[f] = int32(i)
	}
	ne.isFitted = true
}

// Transform converts a SpanFeature to its index. Unknown features map to 0.
func (ne *NodeEncoder) Transform(f SpanFeature) int32 {
	if !ne.isFitted {
		return 0
	}
	if idx, ok := ne.featureToIdx[f]; ok {
		return idx
	}
	return 0 // Unknown -> 0, matching Python behavior
}

// InverseTransform converts an index back to its SpanFeature.
func (ne *NodeEncoder) InverseTransform(idx int32) SpanFeature {
	if !ne.isFitted || idx < 0 || int(idx) >= len(ne.idxToFeature) {
		if len(ne.idxToFeature) > 0 {
			return ne.idxToFeature[0]
		}
		return ""
	}
	return ne.idxToFeature[idx]
}

// VocabSize returns the number of unique features.
func (ne *NodeEncoder) VocabSize() int32 {
	return int32(len(ne.idxToFeature))
}

// Vocabulary returns the ordered feature slice.
func (ne *NodeEncoder) Vocabulary() []SpanFeature {
	return ne.idxToFeature
}

// VocabularyStrings returns the vocabulary as serialized strings for proto storage.
func (ne *NodeEncoder) VocabularyStrings() []string {
	result := make([]string, len(ne.idxToFeature))
	for i, f := range ne.idxToFeature {
		result[i] = f.String()
	}
	return result
}

// Extend adds new features without disturbing existing indices.
// New features are appended after existing ones. Existing indices remain stable.
func (ne *NodeEncoder) Extend(features []SpanFeature) {
	for _, f := range features {
		if _, exists := ne.featureToIdx[f]; !exists {
			ne.featureToIdx[f] = int32(len(ne.idxToFeature))
			ne.idxToFeature = append(ne.idxToFeature, f)
		}
	}
	ne.isFitted = true
}

// MergeFrom absorbs the other encoder's vocabulary into this one.
// Returns a remap table: remap[otherIdx] = thisIdx.
// Existing indices in this encoder are stable.
func (ne *NodeEncoder) MergeFrom(other *NodeEncoder) []int32 {
	remap := make([]int32, len(other.idxToFeature))
	for i, f := range other.idxToFeature {
		if idx, ok := ne.featureToIdx[f]; ok {
			remap[i] = idx
		} else {
			remap[i] = int32(len(ne.idxToFeature))
			ne.featureToIdx[f] = remap[i]
			ne.idxToFeature = append(ne.idxToFeature, f)
		}
	}
	ne.isFitted = true
	return remap
}

// IsFitted returns whether the encoder has been fitted.
func (ne *NodeEncoder) IsFitted() bool {
	return ne.isFitted
}

func (ne *NodeEncoder) String() string {
	if ne.isFitted {
		return fmt.Sprintf("NodeEncoder(vocab_size=%d)", ne.VocabSize())
	}
	return "NodeEncoder(not_fitted)"
}
