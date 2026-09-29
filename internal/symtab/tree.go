package symtab

import (
	"cmp"
	"slices"
)

// isArrayElement reports whether a child is an array element ("[3]") rather
// than a struct member.
func isArrayElement(s *Symbol) bool { return len(s.Name) > 0 && s.Name[0] == '[' }

// sortedElements returns an array's elements in index order. Elements sit
// back to back, so offset order is index order.
func sortedElements(children map[string]*Symbol) []*Symbol {
	out := make([]*Symbol, 0, len(children))
	for _, c := range children {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *Symbol) int { return cmp.Compare(a.Offset, b.Offset) })
	return out
}

// valueTree assembles a composite's value from its children's values:
// []any for an array, map[string]any for a struct. Caller holds cache.lock.
func (s *Symbol) valueTree() any {
	if len(s.Children) == 0 {
		return s.Value
	}
	for _, c := range s.Children {
		if isArrayElement(c) {
			elems := sortedElements(s.Children)
			out := make([]any, len(elems))
			for i, e := range elems {
				out[i] = e.valueTree()
			}
			return out
		}
		break
	}
	out := make(map[string]any, len(s.Children))
	for name, c := range s.Children {
		out[name] = c.valueTree()
	}
	return out
}
