package tpackmodel

import (
	"fmt"
	"math/rand"
	"strings"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
)

// edgeKey uniquely identifies a parent->child edge in the topology model.
// The parent's child_idx is -1 (used as parent reference), and the separate
// ChildPosition field tracks the actual position of the child.
type edgeKey struct {
	ParentFeature NodeFeature // Parent feature with ChildIdx=-1
	ChildPosition int32       // Position of child among parent's children
	ChildFeature  NodeFeature // Child feature with actual child_idx
}

// childCandidate pairs a child feature with its sampling probability.
type childCandidate struct {
	Feature     NodeFeature
	Probability float64
}

// TraceTemplate represents a full tree topology as a BFS-ordered node sequence.
type TraceTemplate struct {
	Nodes         []NodeFeature // BFS order, index 0 = root
	ParentIndices []int32       // parent index in Nodes, -1 for root
	Count         int64
}

// Hash returns a canonical string key for deduplication.
func (t *TraceTemplate) Hash() string {
	var b strings.Builder
	for i, nf := range t.Nodes {
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%d,%d,%d", nf.NodeIdx, nf.ChildCount, t.ParentIndices[i])
	}
	return b.String()
}

// TopologyModel learns and generates tree structures using edge counts
// stratified by trace_type.
type TopologyModel struct {
	Config TPackConfig

	// EdgeCounts[TraceType][edgeKey] = raw count
	EdgeCounts map[TraceType]map[edgeKey]float64

	// childCandidatesCache[TraceType][(parentFeature, childPosition)] = []childCandidate
	ChildCandidatesCache map[TraceType]map[parentKey][]childCandidate

	// Template mode: full tree templates (used when Config.TopologyMode == "template")
	Templates map[TraceType]map[string]*TraceTemplate // hash → template

	// MaxNodes is the maximum number of nodes observed in any tree.
	MaxNodes int32
}

// parentKey identifies a parent and child position for the candidates cache.
type parentKey struct {
	ParentFeature NodeFeature // ChildIdx = -1
	ChildPosition int32
}

// NewTopologyModel creates a new TopologyModel.
func NewTopologyModel(config TPackConfig) *TopologyModel {
	return &TopologyModel{
		Config: config,
		EdgeCounts: map[TraceType]map[edgeKey]float64{
			TraceTypeNormal: {},
			TraceTypeError:  {},
		},
		ChildCandidatesCache: map[TraceType]map[parentKey][]childCandidate{
			TraceTypeNormal: {},
			TraceTypeError:  {},
		},
		Templates: map[TraceType]map[string]*TraceTemplate{
			TraceTypeNormal: {},
			TraceTypeError:  {},
		},
	}
}

// AddEdge records an edge observation. Used during training.
func (tm *TopologyModel) AddEdge(
	traceType TraceType,
	parentFeature NodeFeature, // ChildIdx = -1
	childPosition int32,
	childFeature NodeFeature,
	count int,
) {
	if tm.EdgeCounts[traceType] == nil {
		tm.EdgeCounts[traceType] = make(map[edgeKey]float64)
	}
	key := edgeKey{
		ParentFeature: parentFeature,
		ChildPosition: childPosition,
		ChildFeature:  childFeature,
	}
	tm.EdgeCounts[traceType][key] += float64(count)
}

// BuildChildCandidatesCache groups edge counts by (parent, position) and
// normalizes to conditional probabilities for sampling.
func (tm *TopologyModel) BuildChildCandidatesCache() {
	tm.ChildCandidatesCache = map[TraceType]map[parentKey][]childCandidate{
		TraceTypeNormal: {},
		TraceTypeError:  {},
	}

	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		edges := tm.EdgeCounts[traceType]

		// Group counts by (parentFeature, childPosition)
		grouped := make(map[parentKey][]struct {
			childFeature NodeFeature
			count        float64
		})

		for key, count := range edges {
			pk := parentKey{
				ParentFeature: key.ParentFeature,
				ChildPosition: key.ChildPosition,
			}
			grouped[pk] = append(grouped[pk], struct {
				childFeature NodeFeature
				count        float64
			}{key.ChildFeature, count})
		}

		// Normalize counts to probabilities per group
		cache := make(map[parentKey][]childCandidate)
		for pk, children := range grouped {
			total := 0.0
			for _, c := range children {
				total += c.count
			}

			candidates := make([]childCandidate, len(children))
			for i, c := range children {
				candidates[i] = childCandidate{
					Feature:     c.childFeature,
					Probability: c.count / total,
				}
			}

			cache[pk] = candidates
		}

		tm.ChildCandidatesCache[traceType] = cache
	}
}

// GenerateTreeStructure generates a tree starting from the given root feature.
// Returns nil if no model data exists.
func (tm *TopologyModel) GenerateTreeStructure(
	rootFeature NodeFeature,
	traceType TraceType,
	rng *rand.Rand,
) *TreeNode {
	if tm.MaxNodes <= 0 {
		return nil
	}

	root := &TreeNode{Feature: rootFeature, Depth: 0}
	nodesCreated := 1

	// BFS queue: (node, expectedChildren)
	type queueItem struct {
		node             *TreeNode
		expectedChildren int32
	}
	queue := []queueItem{{root, rootFeature.ChildCount}}

	for len(queue) > 0 && nodesCreated < int(tm.MaxNodes) {
		item := queue[0]
		queue = queue[1:]

		if item.node.Depth >= int(tm.Config.MaxDepth) {
			continue
		}

		maxChildrenHere := min(item.expectedChildren, tm.Config.MaxChildren)

		for childIdx := range maxChildrenHere {
			if nodesCreated >= int(tm.MaxNodes) {
				break
			}

			childFeature := tm.sampleChildFeature(
				item.node.Feature,
				childIdx,
				traceType,
				rng,
			)
			if childFeature == nil {
				continue
			}

			child := &TreeNode{Feature: *childFeature, Depth: item.node.Depth + 1}
			item.node.AddChild(child)

			if childFeature.ChildCount > 0 {
				queue = append(queue, queueItem{child, childFeature.ChildCount})
			}

			nodesCreated++
		}
	}

	return root
}

// sampleChildFeature samples a child feature given parent context.
func (tm *TopologyModel) sampleChildFeature(
	parentFeature NodeFeature,
	childIdx int32,
	traceType TraceType,
	rng *rand.Rand,
) *NodeFeature {
	parentRef := NodeFeature{
		NodeIdx:    parentFeature.NodeIdx,
		ChildIdx:   -1,
		ChildCount: parentFeature.ChildCount,
	}
	pos := childIdx
	pk := parentKey{
		ParentFeature: parentRef,
		ChildPosition: pos,
	}

	candidates, ok := tm.ChildCandidatesCache[traceType][pk]
	if !ok || len(candidates) == 0 {
		return nil
	}

	// Weighted random selection
	r := rng.Float64()
	cumProb := 0.0
	for _, c := range candidates {
		cumProb += c.Probability
		if r <= cumProb {
			f := c.Feature
			return &f
		}
	}

	// Fallback (floating point edge case)
	f := candidates[len(candidates)-1].Feature
	return &f
}

// AddTemplate records a full trace template. Used during training in template mode.
func (tm *TopologyModel) AddTemplate(traceType TraceType, nodes []NodeFeature, parentIndices []int32) {
	tmpl := &TraceTemplate{Nodes: nodes, ParentIndices: parentIndices, Count: 1}
	hash := tmpl.Hash()

	if tm.Templates[traceType] == nil {
		tm.Templates[traceType] = make(map[string]*TraceTemplate)
	}
	if existing, ok := tm.Templates[traceType][hash]; ok {
		existing.Count++
	} else {
		tm.Templates[traceType][hash] = tmpl
	}
}

// GenerateTreeFromTemplate builds a TreeNode tree from a template.
func (tm *TopologyModel) GenerateTreeFromTemplate(tmpl *TraceTemplate) *TreeNode {
	treeNodes := make([]*TreeNode, len(tmpl.Nodes))
	for i, nf := range tmpl.Nodes {
		treeNodes[i] = &TreeNode{Feature: nf}
	}
	for i, parentIdx := range tmpl.ParentIndices {
		if parentIdx >= 0 && int(parentIdx) < len(treeNodes) {
			treeNodes[parentIdx].AddChild(treeNodes[i])
		}
	}
	if len(treeNodes) == 0 {
		return nil
	}
	return treeNodes[0]
}

// GetAllTemplateSamples returns RootSamples for all templates, each repeated Count times.
func (tm *TopologyModel) GetAllTemplateSamples() []RootSample {
	var samples []RootSample
	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		for _, tmpl := range tm.Templates[traceType] {
			for i := int64(0); i < tmpl.Count; i++ {
				samples = append(samples, RootSample{
					Feature:   tmpl.Nodes[0],
					TraceType: traceType,
					Template:  tmpl,
				})
			}
		}
	}
	return samples
}

// RemapNodeIdx translates all NodeIdx references using the given mapping.
func (tm *TopologyModel) RemapNodeIdx(mapping []int32) {
	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		remapped := make(map[edgeKey]float64, len(tm.EdgeCounts[traceType]))
		for k, v := range tm.EdgeCounts[traceType] {
			k.ParentFeature = RemapNodeFeature(k.ParentFeature, mapping)
			k.ChildFeature = RemapNodeFeature(k.ChildFeature, mapping)
			remapped[k] += v
		}
		tm.EdgeCounts[traceType] = remapped

		// Remap templates
		remappedTemplates := make(map[string]*TraceTemplate, len(tm.Templates[traceType]))
		for _, tmpl := range tm.Templates[traceType] {
			newNodes := make([]NodeFeature, len(tmpl.Nodes))
			for i, nf := range tmpl.Nodes {
				newNodes[i] = NodeFeature{
					NodeIdx:    mapping[nf.NodeIdx],
					ChildIdx:   nf.ChildIdx,
					ChildCount: nf.ChildCount,
				}
			}
			newTmpl := &TraceTemplate{Nodes: newNodes, ParentIndices: tmpl.ParentIndices, Count: tmpl.Count}
			remappedTemplates[newTmpl.Hash()] = newTmpl
		}
		tm.Templates[traceType] = remappedTemplates
	}
}

// MergeFrom combines another TopologyModel's edge counts and templates into this one.
func (tm *TopologyModel) MergeFrom(other *TopologyModel) {
	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		for k, v := range other.EdgeCounts[traceType] {
			if tm.EdgeCounts[traceType] == nil {
				tm.EdgeCounts[traceType] = make(map[edgeKey]float64)
			}
			tm.EdgeCounts[traceType][k] += v
		}

		// Merge templates
		for hash, otherTmpl := range other.Templates[traceType] {
			if tm.Templates[traceType] == nil {
				tm.Templates[traceType] = make(map[string]*TraceTemplate)
			}
			if existing, ok := tm.Templates[traceType][hash]; ok {
				existing.Count += otherTmpl.Count
			} else {
				cp := *otherTmpl
				tm.Templates[traceType][hash] = &cp
			}
		}
	}
	if other.MaxNodes > tm.MaxNodes {
		tm.MaxNodes = other.MaxNodes
	}
}

func (tm *TopologyModel) SaveStateDict(models *pb.TPackModels) {
	models.MaxNodesCount = tm.MaxNodes

	for _, traceType := range []TraceType{TraceTypeNormal, TraceTypeError} {
		protoTT := traceType.ToProto()

		for key, count := range tm.EdgeCounts[traceType] {
			topo := &pb.TopologyModel{
				ParentFeature: &pb.NodeFeature{
					NodeIdx:    key.ParentFeature.NodeIdx,
					ChildIdx:   key.ChildPosition, // Save position, not -1
					ChildCount: key.ParentFeature.ChildCount,
				},
				ChildFeature: key.ChildFeature.ToProto(),
				Count:        uint64(count),
				TraceType:    protoTT,
			}
			models.TopologyModels = append(models.TopologyModels, topo)
		}

		// Save templates
		for _, tmpl := range tm.Templates[traceType] {
			protoNodes := make([]*pb.NodeFeature, len(tmpl.Nodes))
			for i, nf := range tmpl.Nodes {
				protoNodes[i] = nf.ToProto()
			}
			models.TraceTemplates = append(models.TraceTemplates, &pb.TraceTemplateProto{
				Nodes:         protoNodes,
				ParentIndices: tmpl.ParentIndices,
				Count:         uint64(tmpl.Count),
				TraceType:     protoTT,
			})
		}
	}
}

// LoadStateDict restores the topology model from a protobuf message.
func (tm *TopologyModel) LoadStateDict(models *pb.TPackModels) {
	tm.EdgeCounts = map[TraceType]map[edgeKey]float64{
		TraceTypeNormal: {},
		TraceTypeError:  {},
	}
	tm.Templates = map[TraceType]map[string]*TraceTemplate{
		TraceTypeNormal: {},
		TraceTypeError:  {},
	}
	tm.MaxNodes = models.MaxNodesCount

	for _, topo := range models.TopologyModels {
		traceType := TraceTypeFromProto(topo.TraceType)

		// Reconstruct the edgeKey: parent_feature has ChildIdx=-1,
		// and the saved ChildIdx in parent_feature is actually the child position.
		childPosition := topo.ParentFeature.ChildIdx
		parentFeature := NodeFeature{
			NodeIdx:    topo.ParentFeature.NodeIdx,
			ChildIdx:   -1,
			ChildCount: topo.ParentFeature.ChildCount,
		}
		childFeature := NodeFeatureFromProto(topo.ChildFeature)

		key := edgeKey{
			ParentFeature: parentFeature,
			ChildPosition: childPosition,
			ChildFeature:  childFeature,
		}

		if tm.EdgeCounts[traceType] == nil {
			tm.EdgeCounts[traceType] = make(map[edgeKey]float64)
		}
		tm.EdgeCounts[traceType][key] = float64(topo.Count)
	}

	// Load templates
	for _, pt := range models.TraceTemplates {
		traceType := TraceTypeFromProto(pt.TraceType)
		nodes := make([]NodeFeature, len(pt.Nodes))
		for i, pn := range pt.Nodes {
			nodes[i] = NodeFeatureFromProto(pn)
		}
		tmpl := &TraceTemplate{Nodes: nodes, ParentIndices: pt.ParentIndices, Count: int64(pt.Count)}
		if tm.Templates[traceType] == nil {
			tm.Templates[traceType] = make(map[string]*TraceTemplate)
		}
		tm.Templates[traceType][tmpl.Hash()] = tmpl
	}

	tm.BuildChildCandidatesCache()
}
