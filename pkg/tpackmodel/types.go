package tpackmodel

import (
	"fmt"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
)

// NodeFeature represents a node in the trace tree with its structural properties.
// It is the primary key for looking up models and sampling.
type NodeFeature struct {
	NodeIdx    int32 // Index into the shared node encoder vocabulary
	ChildIdx   int32 // Index of this node among its parent's children (sorted by startTime)
	ChildCount int32 // Number of children this node has
}

func (nf NodeFeature) String() string {
	return fmt.Sprintf("(%d, %d, %d)", nf.NodeIdx, nf.ChildIdx, nf.ChildCount)
}

// ToProto converts a NodeFeature to its protobuf representation.
func (nf NodeFeature) ToProto() *pb.NodeFeature {
	return &pb.NodeFeature{
		NodeIdx:    nf.NodeIdx,
		ChildIdx:   nf.ChildIdx,
		ChildCount: nf.ChildCount,
	}
}

// NodeFeatureFromProto converts a protobuf NodeFeature to the Go type.
func NodeFeatureFromProto(p *pb.NodeFeature) NodeFeature {
	if p == nil {
		return NodeFeature{}
	}
	return NodeFeature{
		NodeIdx:    p.NodeIdx,
		ChildIdx:   p.ChildIdx,
		ChildCount: p.ChildCount,
	}
}

// TraceType distinguishes normal traces from error traces for stratified sampling.
type TraceType int

const (
	TraceTypeNormal TraceType = iota
	TraceTypeError
)

func (tt TraceType) String() string {
	if tt == TraceTypeError {
		return "error"
	}
	return "normal"
}

// ToProto converts TraceType to its protobuf representation.
func (tt TraceType) ToProto() pb.TraceType {
	if tt == TraceTypeError {
		return pb.TraceType_ERROR
	}
	return pb.TraceType_NORMAL
}

// TraceTypeFromProto converts a protobuf TraceType to the Go type.
func TraceTypeFromProto(p pb.TraceType) TraceType {
	if p == pb.TraceType_ERROR {
		return TraceTypeError
	}
	return TraceTypeNormal
}

// TreeNode represents a node in a generated trace tree structure.
type TreeNode struct {
	Feature  NodeFeature
	Depth    int
	Children []*TreeNode
	Parent   *TreeNode
}

// AddChild adds a child node to this tree node.
func (tn *TreeNode) AddChild(child *TreeNode) {
	child.Parent = tn
	child.Depth = tn.Depth + 1
	tn.Children = append(tn.Children, child)
}

// InternalSpan represents a span in the internal TPack format,
// used between trace conversion and model operations.
type InternalSpan struct {
	SpanID       string
	ParentSpanID string // empty string for root spans
	NodeIdx      int32
	StartTime    int64 // microseconds
	Duration     int64 // microseconds
	Metadata     map[string]string // dynamic metadata columns (e.g. "http.status_code" → "200")
}
