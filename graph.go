// Package graph implements a small, type-oriented directed acyclic graph.
package graph

import (
	"fmt"
	"iter"
	"reflect"
	"sync"
)

// Graph is a selection of zero or more graph nodes. Its representation is
// deliberately private; nodes and relations are not values in the public API.
type Graph struct {
	nodes []*node
	live  *selection
	view  *subgraph
}

type node struct {
	id       uint64
	attrs    map[reflect.Type]any
	children map[*node]uint64 // edge creation revision
	parents  map[*node]struct{}
	selfRev  uint64
	treeRev  uint64
	baseline uint64
}

type selection struct {
	nodes  []*node
	closed bool
}

type subgraph struct{ edges map[*node][]*node }

type typeMatcher struct{ typ reflect.Type }

var state struct {
	sync.RWMutex
	nextID   uint64
	revision uint64
	queries  map[*query]struct{}
}

type matcher struct {
	typ   reflect.Type
	value any
	any   bool
}

type query struct {
	roots    []*node
	matchers []matcher
	result   *selection
}

func init() { state.queries = make(map[*query]struct{}) }

// Type returns a matcher meaning "an attribute of type T, with any value".
func Type[T any]() any {
	t := reflect.TypeOf((*T)(nil)).Elem()
	return typeMatcher{typ: normalizeType(t)}
}

// New creates one independent node containing the supplied attributes.
func New(value any, values ...any) Graph {
	all := append([]any{value}, values...)
	attrs := storedAttributes(all)
	state.Lock()
	defer state.Unlock()
	state.nextID++
	rev := nextRevisionLocked()
	n := &node{id: state.nextID, attrs: attrs, children: make(map[*node]uint64), parents: make(map[*node]struct{}), selfRev: rev, treeRev: rev}
	return Graph{nodes: []*node{n}}
}

// Get returns the first node when at least one requested attribute matches.
// Pointer arguments receive the stored value when present.
func Get(g Graph, values ...any) Graph {
	requests := inspectRequests(values, true)
	state.RLock()
	defer state.RUnlock()
	n := first(g)
	if n == nil {
		return Graph{}
	}
	matched := false
	for _, r := range requests {
		v, ok := n.attrs[r.typ]
		if !ok {
			continue
		}
		if r.dest.IsValid() {
			r.dest.Elem().Set(reflect.ValueOf(v))
			matched = true
		} else if r.any || v == r.value {
			matched = true
		}
	}
	if matched {
		return singleton(n, g.view)
	}
	return Graph{}
}

type request struct {
	typ   reflect.Type
	value any
	any   bool
	dest  reflect.Value
}

// Set adds or replaces attributes on the first node and returns it iff changed.
func Set(g Graph, values ...any) Graph {
	attrs := storedAttributes(values)
	state.Lock()
	defer state.Unlock()
	n := first(g)
	if n == nil {
		return Graph{}
	}
	changedTypes := make(map[reflect.Type]struct{})
	for t, v := range attrs {
		old, ok := n.attrs[t]
		if !ok || old != v {
			n.attrs[t] = v
			changedTypes[t] = struct{}{}
		}
	}
	if len(changedTypes) == 0 {
		return Graph{}
	}
	rev := nextRevisionLocked()
	n.selfRev = rev
	propagateLocked(n, rev)
	updateAttributeQueriesLocked(n, changedTypes)
	return singleton(n, g.view)
}

// Unset removes requested attributes from the first node and optionally writes
// old values to pointer arguments.
func Unset(g Graph, values ...any) Graph {
	requests := inspectRequests(values, true)
	state.Lock()
	defer state.Unlock()
	n := first(g)
	if n == nil {
		return Graph{}
	}
	changed := make(map[reflect.Type]struct{})
	for _, r := range requests {
		v, ok := n.attrs[r.typ]
		if !ok {
			continue
		}
		if r.dest.IsValid() {
			r.dest.Elem().Set(reflect.ValueOf(v))
		}
		delete(n.attrs, r.typ)
		changed[r.typ] = struct{}{}
	}
	if len(changed) == 0 {
		return Graph{}
	}
	rev := nextRevisionLocked()
	n.selfRev = rev
	propagateLocked(n, rev)
	updateAttributeQueriesLocked(n, changed)
	return singleton(n, g.view)
}

// At returns all immediate children, or the supplied candidates that are
// immediate children, preserving their argument order.
func At(g Graph, graphs ...Graph) Graph {
	state.RLock()
	defer state.RUnlock()
	n := first(g)
	if n == nil {
		return Graph{}
	}
	allowed := n.children
	if g.view != nil {
		allowed = make(map[*node]uint64)
		for _, child := range g.view.edges[n] {
			allowed[child] = 0
		}
	}
	if len(graphs) == 0 {
		out := make([]*node, 0, len(allowed))
		if g.view != nil {
			out = append(out, g.view.edges[n]...)
		} else {
			// Node IDs provide stable insertion order without exposing identity.
			for child := range allowed {
				out = append(out, child)
			}
			sortNodes(out)
		}
		return Graph{nodes: out, view: g.view}
	}
	out := make([]*node, 0, len(graphs))
	seen := make(map[*node]struct{})
	for _, candidate := range graphs {
		c := first(candidate)
		if c == nil {
			continue
		}
		if _, ok := allowed[c]; ok {
			if _, dup := seen[c]; !dup {
				seen[c] = struct{}{}
				out = append(out, c)
			}
		}
	}
	return Graph{nodes: out, view: g.view}
}

// Link creates outgoing relations from the first node to the first node of
// each argument. It panics if any relation would create a cycle.
func Link(g Graph, graphs ...Graph) Graph {
	state.Lock()
	defer state.Unlock()
	parent := first(g)
	if parent == nil {
		return Graph{}
	}
	candidates := graphFirsts(graphs)
	for _, child := range candidates {
		if _, exists := parent.children[child]; exists {
			continue
		}
		if child == parent || reachesLocked(child, parent) {
			panic("graph: link would create a cycle")
		}
	}
	out := make([]*node, 0, len(candidates))
	for _, child := range candidates {
		if _, exists := parent.children[child]; exists {
			continue
		}
		rev := nextRevisionLocked()
		parent.children[child] = rev
		child.parents[parent] = struct{}{}
		parent.selfRev = rev
		propagateLocked(parent, rev)
		out = append(out, child)
	}
	if len(out) != 0 {
		updateStructuralQueriesLocked(parent)
	}
	return Graph{nodes: out}
}

// Unlink removes existing outgoing relations.
func Unlink(g Graph, graphs ...Graph) Graph {
	state.Lock()
	defer state.Unlock()
	parent := first(g)
	if parent == nil {
		return Graph{}
	}
	out := make([]*node, 0, len(graphs))
	for _, child := range graphFirsts(graphs) {
		if _, exists := parent.children[child]; !exists {
			continue
		}
		delete(parent.children, child)
		delete(child.parents, parent)
		rev := nextRevisionLocked()
		parent.selfRev = rev
		propagateLocked(parent, rev)
		out = append(out, child)
	}
	if len(out) != 0 {
		updateStructuralQueriesLocked(parent)
	}
	return Graph{nodes: out}
}

// Each iterates over singleton Graph values in selection order.
func Each(g Graph) iter.Seq[Graph] {
	state.RLock()
	nodes := append([]*node(nil), selected(g)...)
	view := g.view
	state.RUnlock()
	return func(yield func(Graph) bool) {
		for _, n := range nodes {
			if !yield(singleton(n, view)) {
				return
			}
		}
	}
}

// Len returns the number of nodes in the selection.
func Len(g Graph) int { state.RLock(); defer state.RUnlock(); return len(selected(g)) }

// Empty reports whether the selection contains no nodes.
func Empty(g Graph) bool { return Len(g) == 0 }

// Merge returns the ordered union of all selections.
func Merge(graphs ...Graph) Graph {
	state.RLock()
	defer state.RUnlock()
	out := []*node{}
	seen := map[*node]struct{}{}
	for _, g := range graphs {
		for _, n := range selected(g) {
			if _, ok := seen[n]; !ok {
				seen[n] = struct{}{}
				out = append(out, n)
			}
		}
	}
	return Graph{nodes: out}
}

// Include returns the intersection of all selections in first-selection order.
func Include(graphs ...Graph) Graph {
	state.RLock()
	defer state.RUnlock()
	if len(graphs) == 0 {
		return Graph{}
	}
	counts := make([]map[*node]struct{}, len(graphs)-1)
	for i, g := range graphs[1:] {
		counts[i] = nodeSet(selected(g))
	}
	out := []*node{}
	seen := map[*node]struct{}{}
	for _, n := range selected(graphs[0]) {
		if _, dup := seen[n]; dup {
			continue
		}
		ok := true
		for _, s := range counts {
			if _, yes := s[n]; !yes {
				ok = false
				break
			}
		}
		if ok {
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return Graph{nodes: out}
}

// Exclude subtracts every later selection from the first.
func Exclude(graphs ...Graph) Graph {
	state.RLock()
	defer state.RUnlock()
	if len(graphs) == 0 {
		return Graph{}
	}
	removed := map[*node]struct{}{}
	for _, g := range graphs[1:] {
		for _, n := range selected(g) {
			removed[n] = struct{}{}
		}
	}
	out := []*node{}
	seen := map[*node]struct{}{}
	for _, n := range selected(graphs[0]) {
		if _, x := removed[n]; x {
			continue
		}
		if _, x := seen[n]; !x {
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return Graph{nodes: out}
}

// Query creates a live AND query over nodes reachable from g (including g).
func Query(g Graph, values ...any) (Graph, func()) {
	ms := queryMatchers(values)
	state.Lock()
	q := &query{roots: append([]*node(nil), selected(g)...), matchers: ms, result: &selection{}}
	recomputeQueryLocked(q)
	state.queries[q] = struct{}{}
	state.Unlock()
	var once sync.Once
	closeFn := func() {
		once.Do(func() { state.Lock(); delete(state.queries, q); q.result.closed = true; state.Unlock() })
	}
	return Graph{live: q.result}, closeFn
}

// Commit returns the current paths from each selected root to changes since its
// preceding commit. The returned Graph is a pruned structural view.
func Commit(g Graph) Graph {
	state.Lock()
	defer state.Unlock()
	view := &subgraph{edges: make(map[*node][]*node)}
	roots := []*node{}
	for _, root := range selected(g) {
		base := root.baseline
		if root.treeRev <= base {
			continue
		}
		if buildCommitLocked(root, base, view, map[*node]bool{}) {
			roots = append(roots, root)
		}
		root.baseline = state.revision
	}
	if len(roots) == 0 {
		return Graph{}
	}
	return Graph{nodes: roots, view: view}
}

func buildCommitLocked(n *node, base uint64, view *subgraph, visiting map[*node]bool) bool {
	if visiting[n] {
		return false
	}
	visiting[n] = true
	defer delete(visiting, n)
	include := n.selfRev > base
	children := orderedChildren(n)
	for _, c := range children {
		edgeRev := n.children[c]
		if edgeRev > base {
			view.edges[n] = append(view.edges[n], c)
			include = true
			continue
		}
		if c.treeRev > base && buildCommitLocked(c, base, view, visiting) {
			view.edges[n] = append(view.edges[n], c)
			include = true
		}
	}
	return include
}

func selected(g Graph) []*node {
	if g.live != nil {
		return g.live.nodes
	}
	return g.nodes
}
func first(g Graph) *node {
	ns := selected(g)
	if len(ns) == 0 {
		return nil
	}
	return ns[0]
}
func singleton(n *node, v *subgraph) Graph {
	if n == nil {
		return Graph{}
	}
	return Graph{nodes: []*node{n}, view: v}
}
func normalizeType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func storedAttributes(values []any) map[reflect.Type]any {
	if len(values) == 0 {
		panic("graph: at least one attribute is required")
	}
	out := make(map[reflect.Type]any, len(values))
	for _, v := range values {
		if v == nil {
			panic("graph: nil attribute")
		}
		t := reflect.TypeOf(v)
		if t.Kind() == reflect.Pointer {
			panic("graph: pointer attribute")
		}
		if t.Kind() == reflect.Interface || !t.Comparable() {
			panic(fmt.Sprintf("graph: attribute %v is not a comparable value", t))
		}
		t = normalizeType(t)
		if _, ok := out[t]; ok {
			panic("graph: duplicate attribute type " + t.String())
		}
		out[t] = v
	}
	return out
}

func inspectRequests(values []any, allowDestination bool) []request {
	if len(values) == 0 {
		panic("graph: at least one attribute request is required")
	}
	out := make([]request, 0, len(values))
	seen := map[reflect.Type]struct{}{}
	for _, v := range values {
		if v == nil {
			panic("graph: nil attribute request")
		}
		r := request{}
		switch x := v.(type) {
		case typeMatcher:
			r.typ = x.typ
			r.any = true
		default:
			rv := reflect.ValueOf(v)
			t := rv.Type()
			if t.Kind() == reflect.Pointer {
				if !allowDestination || rv.IsNil() {
					panic("graph: invalid attribute destination")
				}
				r.typ = normalizeType(t)
				r.dest = rv
			} else {
				if t.Kind() == reflect.Interface || !t.Comparable() {
					panic("graph: attribute request must be comparable")
				}
				r.typ = t
				r.value = v
			}
		}
		if _, ok := seen[r.typ]; ok {
			panic("graph: duplicate attribute type " + r.typ.String())
		}
		seen[r.typ] = struct{}{}
		out = append(out, r)
	}
	return out
}

func queryMatchers(values []any) []matcher {
	rs := inspectRequests(values, false)
	out := make([]matcher, len(rs))
	for i, r := range rs {
		out[i] = matcher{typ: r.typ, value: r.value, any: r.any}
	}
	return out
}
func nextRevisionLocked() uint64 { state.revision++; return state.revision }
func propagateLocked(n *node, rev uint64) {
	seen := map[*node]struct{}{}
	var visit func(*node)
	visit = func(x *node) {
		if _, ok := seen[x]; ok {
			return
		}
		seen[x] = struct{}{}
		if x.treeRev < rev {
			x.treeRev = rev
		}
		for p := range x.parents {
			visit(p)
		}
	}
	visit(n)
}
func reachesLocked(from, target *node) bool {
	seen := map[*node]struct{}{}
	stack := []*node{from}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == target {
			return true
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		for c := range n.children {
			stack = append(stack, c)
		}
	}
	return false
}
func graphFirsts(gs []Graph) []*node {
	out := []*node{}
	seen := map[*node]struct{}{}
	for _, g := range gs {
		if n := first(g); n != nil {
			if _, ok := seen[n]; !ok {
				seen[n] = struct{}{}
				out = append(out, n)
			}
		}
	}
	return out
}
func nodeSet(ns []*node) map[*node]struct{} {
	s := make(map[*node]struct{}, len(ns))
	for _, n := range ns {
		s[n] = struct{}{}
	}
	return s
}
func orderedChildren(n *node) []*node {
	out := make([]*node, 0, len(n.children))
	for c := range n.children {
		out = append(out, c)
	}
	sortNodes(out)
	return out
}
func sortNodes(ns []*node) {
	for i := 1; i < len(ns); i++ {
		for j := i; j > 0 && ns[j].id < ns[j-1].id; j-- {
			ns[j], ns[j-1] = ns[j-1], ns[j]
		}
	}
}
func reachableLocked(roots []*node) []*node {
	seen := map[*node]struct{}{}
	out := []*node{}
	var visit func(*node)
	visit = func(n *node) {
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
		for _, c := range orderedChildren(n) {
			visit(c)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return out
}
func matchesLocked(n *node, ms []matcher) bool {
	for _, m := range ms {
		v, ok := n.attrs[m.typ]
		if !ok || (!m.any && v != m.value) {
			return false
		}
	}
	return true
}
func recomputeQueryLocked(q *query) {
	out := []*node{}
	for _, n := range reachableLocked(q.roots) {
		if matchesLocked(n, q.matchers) {
			out = append(out, n)
		}
	}
	q.result.nodes = out
}
func updateAttributeQueriesLocked(n *node, changed map[reflect.Type]struct{}) {
	for q := range state.queries {
		relevant := false
		for _, m := range q.matchers {
			if _, ok := changed[m.typ]; ok {
				relevant = true
				break
			}
		}
		if !relevant {
			continue
		}
		if reachesFromRootsLocked(q.roots, n) {
			recomputeQueryLocked(q)
		}
	}
}
func reachesFromRootsLocked(roots []*node, target *node) bool {
	for _, r := range roots {
		if reachesLocked(r, target) {
			return true
		}
	}
	return false
}
func updateStructuralQueriesLocked(parent *node) {
	for q := range state.queries {
		if reachesFromRootsLocked(q.roots, parent) {
			recomputeQueryLocked(q)
		}
	}
}
