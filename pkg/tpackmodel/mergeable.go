package tpackmodel

// RemapNodeIdx translates all internal NodeIdx references using the given mapping.
// mapping[oldIdx] = newIdx. Called after merging NodeEncoders to unify indices.
func RemapNodeFeature(f NodeFeature, mapping []int32) NodeFeature {
	if int(f.NodeIdx) < len(mapping) {
		f.NodeIdx = mapping[f.NodeIdx]
	}
	return f
}
