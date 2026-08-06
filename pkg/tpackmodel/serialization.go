package tpackmodel

import (
	"fmt"
	"math/rand"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

// TPackModelState holds the complete state of all TPack models.
type TPackModelState struct {
	Config                 TPackConfig
	NodeEncoder            *NodeEncoder
	StartTableModel              *StartTableModel
	TopologyModel          *TopologyModel
	RootDurationModel      *RootDurationModel
	SpanDurationBounds     *SpanDurationBoundsModel
	SpanGapBounds          *SpanGapBoundsModel
	DependentAttributePredictor      DependentAttributePredictor

	// Feature columns that form node identity (e.g. ["service.name", "span.kind", ...])
	PrimaryAttributes []string

	// Dynamic metadata columns and their vocabularies
	DependentAttributes []string              // e.g. ["http.status_code"]
	DependentAttributeVocabs  map[string][]string   // column → sorted unique values

	// Timing metadata from training batch (for pacing trace emission)
	MinStartTimeUs int64
	MaxStartTimeUs int64
	TraceCount     int32
}

// NewTPackModelState creates an empty model state with default config.
// DependentAttributePredictor is intentionally left nil; callers set it once they know
// the metadata column count: NewStreamingTrainer at training time, or
// loadMetadataPredictor when restoring from proto.
func NewTPackModelState(config TPackConfig) *TPackModelState {
	return &TPackModelState{
		Config:             config,
		NodeEncoder:        NewNodeEncoder(),
		StartTableModel:          NewStartTableModel(config),
		TopologyModel:      NewTopologyModel(config),
		RootDurationModel:  NewRootDurationModel(config),
		SpanDurationBounds: NewSpanDurationBoundsModel(config),
		SpanGapBounds:      NewSpanGapBoundsModel(config),
		DependentAttributeVocabs:     make(map[string][]string),
	}
}

// Marshal serializes the full model state to protobuf bytes.
func (s *TPackModelState) Marshal() ([]byte, error) {
	models := &pb.TPackModels{
		Config:         s.Config.ToProto(),
		MinStartTimeUs: s.MinStartTimeUs,
		MaxStartTimeUs: s.MaxStartTimeUs,
		TraceCount:     s.TraceCount,
	}

	// Save node vocabulary (Go-readable format)
	if s.NodeEncoder.IsFitted() {
		models.NodeVocabulary = s.NodeEncoder.VocabularyStrings()
	}

	// Save feature columns
	models.PrimaryAttributes = s.PrimaryAttributes

	// Save metadata columns and vocabs
	models.DependentAttributes = s.DependentAttributes
	for _, col := range s.DependentAttributes {
		if vals, ok := s.DependentAttributeVocabs[col]; ok {
			models.DependentAttributeVocabs = append(models.DependentAttributeVocabs, &pb.DependentAttributeVocab{
				ColumnName: col,
				Values:     vals,
			})
		}
	}

	// Save each model
	s.StartTableModel.SaveStateDict(models)
	s.TopologyModel.SaveStateDict(models)
	s.RootDurationModel.SaveStateDict(models)
	s.SpanDurationBounds.SaveStateDict(models)
	s.SpanGapBounds.SaveStateDict(models)

	if s.DependentAttributePredictor != nil {
		s.DependentAttributePredictor.SaveStateDict(models)
	}

	return proto.Marshal(models)
}

// MarshalDelta serializes the model state with delta vocabulary encoding.
// Only vocabulary entries at indices [vocabOffset:] are included in node_vocabulary.
// CumulativeVocabSize is set to the full vocabulary size for reconstruction.
func (s *TPackModelState) MarshalDelta(vocabOffset int) ([]byte, error) {
	models := &pb.TPackModels{
		Config:         s.Config.ToProto(),
		MinStartTimeUs: s.MinStartTimeUs,
		MaxStartTimeUs: s.MaxStartTimeUs,
		TraceCount:     s.TraceCount,
	}

	// Save only delta vocabulary entries
	if s.NodeEncoder.IsFitted() {
		allVocab := s.NodeEncoder.VocabularyStrings()
		if vocabOffset < len(allVocab) {
			models.NodeVocabulary = allVocab[vocabOffset:]
		}
		models.CumulativeVocabSize = int32(len(allVocab))
	}

	// Save feature columns
	models.PrimaryAttributes = s.PrimaryAttributes

	// Save metadata columns and vocabs
	models.DependentAttributes = s.DependentAttributes
	for _, col := range s.DependentAttributes {
		if vals, ok := s.DependentAttributeVocabs[col]; ok {
			models.DependentAttributeVocabs = append(models.DependentAttributeVocabs, &pb.DependentAttributeVocab{
				ColumnName: col,
				Values:     vals,
			})
		}
	}

	// Save each model
	s.StartTableModel.SaveStateDict(models)
	s.TopologyModel.SaveStateDict(models)
	s.RootDurationModel.SaveStateDict(models)
	s.SpanDurationBounds.SaveStateDict(models)
	s.SpanGapBounds.SaveStateDict(models)

	if s.DependentAttributePredictor != nil {
		s.DependentAttributePredictor.SaveStateDict(models)
	}

	return proto.Marshal(models)
}

// Unmarshal restores the full model state from protobuf bytes.
func (s *TPackModelState) Unmarshal(data []byte) error {
	models := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models); err != nil {
		return fmt.Errorf("unmarshal protobuf: %w", err)
	}

	return s.LoadFromProto(models)
}

// LoadFromProto restores model state from an already-parsed protobuf message.
func (s *TPackModelState) LoadFromProto(models *pb.TPackModels) error {
	// Load config
	if models.Config != nil {
		s.Config = ConfigFromProto(models.Config)
	}

	// Load timing metadata
	s.MinStartTimeUs = models.MinStartTimeUs
	s.MaxStartTimeUs = models.MaxStartTimeUs
	s.TraceCount = models.TraceCount

	// Load node encoder from vocabulary
	if len(models.NodeVocabulary) > 0 {
		s.NodeEncoder = NewNodeEncoder()
		s.NodeEncoder.FitFromVocabulary(models.NodeVocabulary)
	}

	// Load models
	s.StartTableModel = NewStartTableModel(s.Config)
	s.StartTableModel.LoadStateDict(models)

	s.TopologyModel = NewTopologyModel(s.Config)
	s.TopologyModel.LoadStateDict(models)

	s.RootDurationModel = NewRootDurationModel(s.Config)
	s.RootDurationModel.LoadStateDict(models)

	s.SpanDurationBounds = NewSpanDurationBoundsModel(s.Config)
	s.SpanDurationBounds.LoadStateDict(models)

	s.SpanGapBounds = NewSpanGapBoundsModel(s.Config)
	s.SpanGapBounds.LoadStateDict(models)

	// Load feature columns
	s.PrimaryAttributes = models.PrimaryAttributes

	// Load metadata columns and vocabs
	s.DependentAttributes = models.DependentAttributes
	s.DependentAttributeVocabs = make(map[string][]string)
	for _, mv := range models.DependentAttributeVocabs {
		s.DependentAttributeVocabs[mv.ColumnName] = mv.Values
	}

	// Load metadata predictor
	if err := s.loadMetadataPredictor(models); err != nil {
		return fmt.Errorf("load metadata predictor: %w", err)
	}

	return nil
}

// LoadFromProtoWithBaseVocabulary restores model state from a delta-encoded protobuf.
// If CumulativeVocabSize > 0, baseVocab is prepended to the delta node_vocabulary.
// If CumulativeVocabSize == 0, baseVocab is ignored (legacy full-vocab path).
func (s *TPackModelState) LoadFromProtoWithBaseVocabulary(models *pb.TPackModels, baseVocab []string) error {
	if models.CumulativeVocabSize > 0 && len(baseVocab) > 0 {
		fullVocab := make([]string, len(baseVocab), int(models.CumulativeVocabSize))
		copy(fullVocab, baseVocab)
		fullVocab = append(fullVocab, models.NodeVocabulary...)
		models.NodeVocabulary = fullVocab
	}
	return s.LoadFromProto(models)
}

// loadMetadataPredictor creates and loads the statistical metadata predictor.
// DependentAttributes must be loaded into s before calling this: the predictor's
// numMetaCols is fixed at construction and is not in the proto schema.
func (s *TPackModelState) loadMetadataPredictor(models *pb.TPackModels) error {
	rng := rand.New(rand.NewSource(int64(s.Config.RandomSeed)))
	statPredictor := NewStatisticalDependentAttributePredictor(s.Config, len(s.DependentAttributes), rng)
	statPredictor.LoadStateDict(models)
	s.DependentAttributePredictor = statPredictor
	return nil
}
