package tpackmodel

import (
	"math/rand"
	"testing"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"google.golang.org/protobuf/proto"
)

func TestTopologyModelGenerateTree(t *testing.T) {
	config := DefaultConfig()
	tm := NewTopologyModel(config)
	tm.MaxNodes = 10

	parentFeature := NodeFeature{NodeIdx: 0, ChildIdx: -1, ChildCount: 2}
	child0 := NodeFeature{NodeIdx: 1, ChildIdx: 0, ChildCount: 0}
	child1 := NodeFeature{NodeIdx: 2, ChildIdx: 1, ChildCount: 0}

	// Add edges: parent → child0, parent → child1
	tm.AddEdge(TraceTypeNormal, parentFeature, 0, child0, 10)
	tm.AddEdge(TraceTypeNormal, parentFeature, 1, child1, 10)
	tm.BuildChildCandidatesCache()

	rng := rand.New(rand.NewSource(42))
	rootFeature := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 2}
	tree := tm.GenerateTreeStructure(rootFeature, TraceTypeNormal, rng)

	if tree == nil {
		t.Fatal("expected non-nil tree")
	}
	if len(tree.Children) != 2 {
		t.Errorf("expected 2 children, got %d", len(tree.Children))
	}
}

func TestTopologyModelProtobufRoundtrip(t *testing.T) {
	config := DefaultConfig()
	tm := NewTopologyModel(config)
	tm.MaxNodes = 50

	parentFeature := NodeFeature{NodeIdx: 0, ChildIdx: -1, ChildCount: 1}
	childFeature := NodeFeature{NodeIdx: 1, ChildIdx: 0, ChildCount: 0}
	tm.AddEdge(TraceTypeNormal, parentFeature, 0, childFeature, 5)
	tm.BuildChildCandidatesCache()

	// Serialize
	models := &pb.TPackModels{}
	tm.SaveStateDict(models)
	data, err := proto.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}

	// Deserialize
	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models2); err != nil {
		t.Fatal(err)
	}

	tm2 := NewTopologyModel(config)
	tm2.LoadStateDict(models2)

	// Verify max nodes
	if tm2.MaxNodes != 50 {
		t.Errorf("expected max nodes 50, got %d", tm2.MaxNodes)
	}

	// Verify we can generate from loaded model
	rng := rand.New(rand.NewSource(42))
	rootFeature := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 1}
	tree := tm2.GenerateTreeStructure(rootFeature, TraceTypeNormal, rng)

	if tree == nil {
		t.Fatal("expected non-nil tree from loaded model")
	}
	if len(tree.Children) != 1 {
		t.Errorf("expected 1 child, got %d", len(tree.Children))
	}
}

func TestTopologyModelMaxNodesLimit(t *testing.T) {
	config := DefaultConfig()
	tm := NewTopologyModel(config)
	tm.MaxNodes = 3 // Limit to 3 nodes total

	parentFeature := NodeFeature{NodeIdx: 0, ChildIdx: -1, ChildCount: 5}
	for i := range int32(5) {
		child := NodeFeature{NodeIdx: i + 1, ChildIdx: i, ChildCount: 0}
		tm.AddEdge(TraceTypeNormal, parentFeature, i, child, 1)
	}
	tm.BuildChildCandidatesCache()

	rng := rand.New(rand.NewSource(42))
	rootFeature := NodeFeature{NodeIdx: 0, ChildIdx: 0, ChildCount: 5}
	tree := tm.GenerateTreeStructure(rootFeature, TraceTypeNormal, rng)

	if tree == nil {
		t.Fatal("expected non-nil tree")
	}

	// Count total nodes
	count := countNodes(tree)
	if count > 3 {
		t.Errorf("expected at most 3 nodes (maxNodes), got %d", count)
	}
}

func countNodes(n *TreeNode) int {
	c := 1
	for _, child := range n.Children {
		c += countNodes(child)
	}
	return c
}

// ── Template tests ──

func makeTestTemplate() ([]NodeFeature, []int32) {
	// Pattern A from running example: Checkout(CC=2) → GetCart(CC=1), ChargeCard(CC=0); GetCart → CacheGet(CC=0)
	nodes := []NodeFeature{
		{NodeIdx: 0, ChildIdx: -1, ChildCount: 2}, // root: Checkout
		{NodeIdx: 1, ChildIdx: -1, ChildCount: 1}, // child 0: GetCart
		{NodeIdx: 2, ChildIdx: -1, ChildCount: 0}, // child 1: ChargeCard
		{NodeIdx: 5, ChildIdx: -1, ChildCount: 0}, // grandchild: CacheGet
	}
	parentIndices := []int32{-1, 0, 0, 1}
	return nodes, parentIndices
}

func TestTopologyModelAddTemplate(t *testing.T) {
	config := DefaultConfig()
	config.TopologyMode = "template"
	tm := NewTopologyModel(config)

	nodes, parentIndices := makeTestTemplate()

	// Add same template 3 times
	tm.AddTemplate(TraceTypeNormal, nodes, parentIndices)
	tm.AddTemplate(TraceTypeNormal, nodes, parentIndices)
	tm.AddTemplate(TraceTypeNormal, nodes, parentIndices)

	if len(tm.Templates[TraceTypeNormal]) != 1 {
		t.Errorf("expected 1 unique template, got %d", len(tm.Templates[TraceTypeNormal]))
	}

	for _, tmpl := range tm.Templates[TraceTypeNormal] {
		if tmpl.Count != 3 {
			t.Errorf("expected count 3, got %d", tmpl.Count)
		}
	}

	// Add a different template (Pattern B: CC=3)
	nodesB := []NodeFeature{
		{NodeIdx: 0, ChildIdx: -1, ChildCount: 3},
		{NodeIdx: 1, ChildIdx: -1, ChildCount: 1},
		{NodeIdx: 3, ChildIdx: -1, ChildCount: 0}, // ChargeCard-ERR
		{NodeIdx: 2, ChildIdx: -1, ChildCount: 0},
		{NodeIdx: 5, ChildIdx: -1, ChildCount: 0},
	}
	parentIndicesB := []int32{-1, 0, 0, 0, 1}
	tm.AddTemplate(TraceTypeNormal, nodesB, parentIndicesB)

	if len(tm.Templates[TraceTypeNormal]) != 2 {
		t.Errorf("expected 2 unique templates, got %d", len(tm.Templates[TraceTypeNormal]))
	}
}

func TestTopologyModelGenerateTreeFromTemplate(t *testing.T) {
	config := DefaultConfig()
	config.TopologyMode = "template"
	tm := NewTopologyModel(config)

	nodes, parentIndices := makeTestTemplate()
	tmpl := &TraceTemplate{Nodes: nodes, ParentIndices: parentIndices, Count: 1}

	tree := tm.GenerateTreeFromTemplate(tmpl)
	if tree == nil {
		t.Fatal("expected non-nil tree")
	}

	// Root should have 2 children
	if len(tree.Children) != 2 {
		t.Fatalf("expected root to have 2 children, got %d", len(tree.Children))
	}

	// First child (GetCart) should have 1 child (CacheGet)
	if len(tree.Children[0].Children) != 1 {
		t.Errorf("expected GetCart to have 1 child, got %d", len(tree.Children[0].Children))
	}

	// Second child (ChargeCard) should be a leaf
	if len(tree.Children[1].Children) != 0 {
		t.Errorf("expected ChargeCard to be leaf, got %d children", len(tree.Children[1].Children))
	}

	// Verify node features
	if tree.Feature.NodeIdx != 0 {
		t.Errorf("expected root NodeIdx=0, got %d", tree.Feature.NodeIdx)
	}
	if tree.Children[0].Feature.NodeIdx != 1 {
		t.Errorf("expected first child NodeIdx=1, got %d", tree.Children[0].Feature.NodeIdx)
	}
	if tree.Children[0].Children[0].Feature.NodeIdx != 5 {
		t.Errorf("expected grandchild NodeIdx=5, got %d", tree.Children[0].Children[0].Feature.NodeIdx)
	}

	// Verify depths
	if tree.Depth != 0 {
		t.Errorf("expected root depth 0, got %d", tree.Depth)
	}
	if tree.Children[0].Depth != 1 {
		t.Errorf("expected child depth 1, got %d", tree.Children[0].Depth)
	}
	if tree.Children[0].Children[0].Depth != 2 {
		t.Errorf("expected grandchild depth 2, got %d", tree.Children[0].Children[0].Depth)
	}
}

func TestTopologyModelTemplateProtobufRoundtrip(t *testing.T) {
	config := DefaultConfig()
	config.TopologyMode = "template"
	tm := NewTopologyModel(config)
	tm.MaxNodes = 10

	nodes, parentIndices := makeTestTemplate()
	tm.AddTemplate(TraceTypeNormal, nodes, parentIndices)
	tm.AddTemplate(TraceTypeNormal, nodes, parentIndices) // count=2
	tm.AddTemplate(TraceTypeError, nodes, parentIndices)  // separate trace type

	// Serialize
	models := &pb.TPackModels{}
	tm.SaveStateDict(models)
	data, err := proto.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}

	// Deserialize
	models2 := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models2); err != nil {
		t.Fatal(err)
	}

	tm2 := NewTopologyModel(config)
	tm2.LoadStateDict(models2)

	// Verify templates survived
	if len(tm2.Templates[TraceTypeNormal]) != 1 {
		t.Errorf("expected 1 normal template, got %d", len(tm2.Templates[TraceTypeNormal]))
	}
	if len(tm2.Templates[TraceTypeError]) != 1 {
		t.Errorf("expected 1 error template, got %d", len(tm2.Templates[TraceTypeError]))
	}

	for _, tmpl := range tm2.Templates[TraceTypeNormal] {
		if tmpl.Count != 2 {
			t.Errorf("expected normal template count=2, got %d", tmpl.Count)
		}
		if len(tmpl.Nodes) != 4 {
			t.Errorf("expected 4 nodes in template, got %d", len(tmpl.Nodes))
		}

		// Verify we can generate a tree from the loaded template
		tree := tm2.GenerateTreeFromTemplate(tmpl)
		if tree == nil {
			t.Fatal("expected non-nil tree from loaded template")
		}
		if len(tree.Children) != 2 {
			t.Errorf("expected 2 children after roundtrip, got %d", len(tree.Children))
		}
	}
}

func TestTopologyModelTemplateMerge(t *testing.T) {
	config := DefaultConfig()
	config.TopologyMode = "template"

	tm1 := NewTopologyModel(config)
	tm2 := NewTopologyModel(config)

	nodes, parentIndices := makeTestTemplate()

	tm1.AddTemplate(TraceTypeNormal, nodes, parentIndices) // count=1
	tm1.AddTemplate(TraceTypeNormal, nodes, parentIndices) // count=2

	tm2.AddTemplate(TraceTypeNormal, nodes, parentIndices) // count=1
	tm2.MaxNodes = 20

	tm1.MergeFrom(tm2)

	if len(tm1.Templates[TraceTypeNormal]) != 1 {
		t.Errorf("expected 1 template after merge, got %d", len(tm1.Templates[TraceTypeNormal]))
	}
	for _, tmpl := range tm1.Templates[TraceTypeNormal] {
		if tmpl.Count != 3 {
			t.Errorf("expected merged count=3, got %d", tmpl.Count)
		}
	}
	if tm1.MaxNodes != 20 {
		t.Errorf("expected MaxNodes=20 after merge, got %d", tm1.MaxNodes)
	}
}

func TestTopologyModelGetAllTemplateSamples(t *testing.T) {
	config := DefaultConfig()
	config.TopologyMode = "template"
	tm := NewTopologyModel(config)

	nodes, parentIndices := makeTestTemplate()
	tm.AddTemplate(TraceTypeNormal, nodes, parentIndices)
	tm.AddTemplate(TraceTypeNormal, nodes, parentIndices) // count=2

	nodesB := []NodeFeature{
		{NodeIdx: 0, ChildIdx: -1, ChildCount: 3},
		{NodeIdx: 1, ChildIdx: -1, ChildCount: 0},
		{NodeIdx: 2, ChildIdx: -1, ChildCount: 0},
		{NodeIdx: 3, ChildIdx: -1, ChildCount: 0},
	}
	parentIndicesB := []int32{-1, 0, 0, 0}
	tm.AddTemplate(TraceTypeNormal, nodesB, parentIndicesB) // count=1

	samples := tm.GetAllTemplateSamples()
	if len(samples) != 3 { // 2 from template A + 1 from template B
		t.Errorf("expected 3 samples, got %d", len(samples))
	}

	// All should have non-nil Template
	for i, s := range samples {
		if s.Template == nil {
			t.Errorf("sample %d has nil Template", i)
		}
	}
}
