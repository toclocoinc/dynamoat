package expr

import (
	"sort"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
)

// Project returns the parts of item named by paths (ProjectionExpression).
func Project(item attr.Item, paths []Path) attr.Item {
	root := &projNode{}
	for _, p := range paths {
		if v, ok := Resolve(item, p); ok {
			root.merge(p, v)
		}
	}
	return root.item()
}

// projNode is one node of the projected tree: either a whole value, or a
// partially built map or list.
type projNode struct {
	whole bool
	val   attr.Value
	kids  map[string]*projNode
	elems map[int]*projNode
}

// merge inserts value at path. A whole ancestor already covers it, so the
// insertion is dropped; setting a whole node discards any children it subsumes.
func (n *projNode) merge(p Path, v attr.Value) {
	if n.whole {
		return
	}
	if len(p) == 0 {
		n.whole = true
		n.val = v
		n.kids = nil
		n.elems = nil
		return
	}
	e := p[0]
	if e.IsIndex {
		if n.elems == nil {
			n.elems = map[int]*projNode{}
		}
		child := n.elems[e.Index]
		if child == nil {
			child = &projNode{}
			n.elems[e.Index] = child
		}
		child.merge(p[1:], v)
		return
	}
	if n.kids == nil {
		n.kids = map[string]*projNode{}
	}
	child := n.kids[e.Name]
	if child == nil {
		child = &projNode{}
		n.kids[e.Name] = child
	}
	child.merge(p[1:], v)
}

// item converts the tree to an attr.Item; the root is always a map of
// top-level attribute names.
func (n *projNode) item() attr.Item {
	it := attr.Item{}
	for name, child := range n.kids {
		it[name] = child.value()
	}
	return it
}

func (n *projNode) value() attr.Value {
	if n.whole {
		return n.val
	}
	if n.kids != nil {
		m := make(map[string]attr.Value, len(n.kids))
		for name, child := range n.kids {
			m[name] = child.value()
		}
		return attr.Value{Type: attr.TypeM, M: m}
	}
	if n.elems != nil {
		idxs := make([]int, 0, len(n.elems))
		for i := range n.elems {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		l := make([]attr.Value, 0, len(idxs))
		for _, i := range idxs {
			l = append(l, n.elems[i].value())
		}
		return attr.Value{Type: attr.TypeL, L: l}
	}
	return attr.Value{}
}
