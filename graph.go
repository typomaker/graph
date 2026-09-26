// Package graph implements a small, type-oriented directed acyclic graph.
package graph

import (
	"fmt"
	"iter"
	"reflect"
	"sort"
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
	id              uint64
	key             uint64
	attrs           map[reflect.Type]any
	attrRev         map[reflect.Type]uint64
	removedAttrs    map[reflect.Type]uint64
	children        map[*node]uint64 // edge creation revision
	removedChildren map[uint64]removedChild
	parents         map[*node]struct{}
	selfRev         uint64
	treeRev         uint64
	baseline        uint64
}

type selection struct {
	nodes  []*node
	closed bool
}

type subgraph struct {
	edges      map[*node][]*node
	changes    map[*node]nodeChange
	matchTypes []reflect.Type
}

type removedChild struct {
	revision uint64
	attrs    map[reflect.Type]any
}

type nodeChange struct {
	attrs           map[reflect.Type]any
	removedAttrs    []reflect.Type
	addedChildren   map[uint64]struct{}
	removedChildren []removedChildChange
	snapshot        map[reflect.Type]any
}

type removedChildChange struct {
	key   uint64
	attrs map[reflect.Type]any
}

type typeMatcher struct{ typ reflect.Type }

var state struct {
	sync.RWMutex
	nextID   uint64
	revision uint64
	queries  map[*query]struct{}
	nodes    map[*node]struct{}
	index    map[indexKey]*indexBucket
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
	scope    map[*node]uint32
	root     map[*node]struct{}
	buckets  []indexKey
}

type indexKey struct {
	typ   reflect.Type
	value any
	any   bool
}

type indexBucket struct {
	nodes map[*node]struct{}
	users uint64
}

func init() {
	state.queries = make(map[*query]struct{})
	state.nodes = make(map[*node]struct{})
	state.index = make(map[indexKey]*indexBucket)
}

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
	n := newNodeLocked(state.nextID, attrs, rev)
	state.nodes[n] = struct{}{}
	updateIndexesLocked(n, nil)
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
			n.attrRev[t] = 0
			delete(n.removedAttrs, t)
			changedTypes[t] = struct{}{}
		}
	}
	if len(changedTypes) == 0 {
		return Graph{}
	}
	rev := nextRevisionLocked()
	n.selfRev = rev
	for t := range changedTypes {
		n.attrRev[t] = rev
	}
	propagateLocked(n, rev)
	updateIndexesLocked(n, changedTypes)
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
		delete(n.attrRev, r.typ)
		changed[r.typ] = struct{}{}
	}
	if len(changed) == 0 {
		return Graph{}
	}
	rev := nextRevisionLocked()
	n.selfRev = rev
	for t := range changed {
		n.removedAttrs[t] = rev
	}
	propagateLocked(n, rev)
	updateIndexesLocked(n, changed)
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
		delete(parent.removedChildren, child.key)
		child.parents[parent] = struct{}{}
		parent.selfRev = rev
		propagateLocked(parent, rev)
		updateStructuralQueriesLocked(parent, child, true)
		out = append(out, child)
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
		parent.removedChildren[child.key] = removedChild{revision: rev, attrs: cloneAttrs(child.attrs)}
		parent.selfRev = rev
		propagateLocked(parent, rev)
		updateStructuralQueriesLocked(parent, child, false)
		out = append(out, child)
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
	q := &query{roots: append([]*node(nil), selected(g)...), matchers: ms, result: &selection{}, root: make(map[*node]struct{})}
	for _, m := range ms {
		key := indexKey(m)
		acquireIndexLocked(key)
		q.buckets = append(q.buckets, key)
	}
	recomputeQueryLocked(q)
	state.queries[q] = struct{}{}
	state.Unlock()
	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			state.Lock()
			delete(state.queries, q)
			q.result.closed = true
			for _, key := range q.buckets {
				releaseIndexLocked(key)
			}
			state.Unlock()
		})
	}
	return Graph{live: q.result}, closeFn
}

// Commit returns the current paths from each selected root to changes since its
// preceding commit. The returned Graph is a pruned structural view. Optional
// matchBy attributes define the stable composite identity Apply must use.
func Commit(g Graph, matchBy ...any) Graph {
	state.Lock()
	defer state.Unlock()
	keyTypes := identityTypes(matchBy)
	view := &subgraph{edges: make(map[*node][]*node), changes: make(map[*node]nodeChange), matchTypes: keyTypes}
	roots := []*node{}
	dirtyRoots := []*node{}
	for _, root := range selected(g) {
		base := root.baseline
		if root.treeRev <= base {
			continue
		}
		dirtyRoots = append(dirtyRoots, root)
		if buildCommitLocked(root, base, view, map[*node]bool{}) {
			roots = append(roots, root)
		}
	}
	if len(keyTypes) != 0 {
		for n, change := range view.changes {
			nodeMatchKey(n, keyTypes)
			for _, removed := range change.removedChildren {
				attrsMatchKey(removed.attrs, keyTypes)
			}
		}
	}
	for _, root := range dirtyRoots {
		root.baseline = state.revision
	}
	if len(roots) == 0 {
		return Graph{}
	}
	return Graph{nodes: roots, view: view}
}

// Apply merges changes into g and returns the corresponding roots. Commit
// deltas use their internal node identity. An ordinary graph is deeply merged
// by the composite attribute key described by matchBy; its roots correspond by
// selection order, while descendants match when every key attribute is equal.
func Apply(g, delta Graph, matchBy ...any) Graph {
	state.Lock()
	defer state.Unlock()
	if len(selected(delta)) == 0 {
		return g
	}
	if delta.view == nil {
		return mergeGraphLocked(g, delta, matchTypes(matchBy))
	}
	keyTypes := identityTypes(matchBy)
	if !sameTypes(keyTypes, delta.view.matchTypes) {
		panic("graph: apply match attributes differ from commit")
	}

	targets := make(map[uint64]*node)
	targetScope := reachableLocked(selected(g))
	if len(keyTypes) == 0 {
		for _, n := range targetScope {
			if _, duplicate := targets[n.key]; duplicate {
				panic("graph: replica contains duplicate node identity")
			}
			targets[n.key] = n
		}
	}
	deltaNodes := reachableViewLocked(selected(delta), delta.view)
	matches := make(map[*node]*node, len(deltaNodes))
	for _, source := range deltaNodes {
		if len(keyTypes) == 0 {
			matches[source] = targets[source.key]
			continue
		}
		matches[source] = findIdentityMatch(source.attrs, targetScope, keyTypes)
	}
	rev := nextRevisionLocked()
	created := make(map[*node]bool)
	for _, source := range deltaNodes {
		if matches[source] != nil {
			continue
		}
		change := delta.view.changes[source]
		state.nextID++
		key := source.key
		if len(keyTypes) != 0 {
			key = state.nextID
		}
		target := newNodeLocked(key, cloneAttrs(change.snapshot), rev)
		target.id = state.nextID
		targets[source.key] = target
		matches[source] = target
		targetScope = append(targetScope, target)
		created[target] = true
		state.nodes[target] = struct{}{}
	}

	changed := make(map[*node]struct{})
	for _, source := range deltaNodes {
		target := matches[source]
		change := delta.view.changes[source]
		if !created[target] {
			for typ, value := range change.attrs {
				if old, ok := target.attrs[typ]; !ok || old != value {
					target.attrs[typ] = value
					target.attrRev[typ] = rev
					delete(target.removedAttrs, typ)
					changed[target] = struct{}{}
				}
			}
			for _, typ := range change.removedAttrs {
				if _, ok := target.attrs[typ]; ok {
					delete(target.attrs, typ)
					delete(target.attrRev, typ)
					target.removedAttrs[typ] = rev
					changed[target] = struct{}{}
				}
			}
		}
	}
	for _, source := range deltaNodes {
		parent := matches[source]
		change := delta.view.changes[source]
		for _, removed := range change.removedChildren {
			var child *node
			if len(keyTypes) == 0 {
				child = targets[removed.key]
			} else {
				child = findIdentityMatch(removed.attrs, orderedChildren(parent), keyTypes)
			}
			if child == nil {
				continue
			}
			if _, ok := parent.children[child]; ok {
				delete(parent.children, child)
				delete(child.parents, parent)
				parent.removedChildren[child.key] = removedChild{revision: rev, attrs: cloneAttrs(child.attrs)}
				changed[parent] = struct{}{}
			}
		}
		for _, sourceChild := range delta.view.edges[source] {
			if _, added := change.addedChildren[sourceChild.key]; !added {
				continue
			}
			child := matches[sourceChild]
			if _, ok := parent.children[child]; !ok {
				if child == parent || reachesLocked(child, parent) {
					panic("graph: delta would create a cycle")
				}
				parent.children[child] = rev
				delete(parent.removedChildren, child.key)
				child.parents[parent] = struct{}{}
				changed[parent] = struct{}{}
			}
		}
	}
	for target := range created {
		changed[target] = struct{}{}
	}
	for target := range changed {
		target.selfRev = rev
		propagateLocked(target, rev)
		updateIndexesLocked(target, nil)
	}
	if len(changed) != 0 {
		for q := range state.queries {
			recomputeQueryLocked(q)
		}
	}

	roots := make([]*node, 0, len(selected(delta)))
	for _, source := range selected(delta) {
		roots = append(roots, matches[source])
	}
	return Graph{nodes: roots}
}

type mergePair struct {
	source *node
	target *node
}

func mergeGraphLocked(g, patch Graph, keyTypes []reflect.Type) Graph {
	if len(keyTypes) == 0 {
		panic("graph: ordinary patch requires match attributes")
	}
	targetRoots := selected(g)
	pairs := make([]mergePair, 0)
	mapped := make(map[*node]*node)
	for i, source := range selected(patch) {
		var target *node
		if i < len(targetRoots) {
			target = targetRoots[i]
		}
		planMergeLocked(source, target, keyTypes, mapped, &pairs)
	}

	rev := nextRevisionLocked()
	created := make(map[*node]struct{})
	for i := range pairs {
		if pairs[i].target != nil {
			continue
		}
		state.nextID++
		target := newNodeLocked(state.nextID, cloneAttrs(pairs[i].source.attrs), rev)
		state.nodes[target] = struct{}{}
		pairs[i].target = target
		mapped[pairs[i].source] = target
		created[target] = struct{}{}
	}

	changed := make(map[*node]struct{})
	for _, pair := range pairs {
		if _, isNew := created[pair.target]; isNew {
			changed[pair.target] = struct{}{}
			continue
		}
		for typ, value := range pair.source.attrs {
			if old, ok := pair.target.attrs[typ]; ok && old == value {
				continue
			}
			pair.target.attrs[typ] = value
			pair.target.attrRev[typ] = rev
			delete(pair.target.removedAttrs, typ)
			changed[pair.target] = struct{}{}
		}
	}
	for _, pair := range pairs {
		for sourceChild := range pair.source.children {
			targetChild := mapped[sourceChild]
			if _, exists := pair.target.children[targetChild]; exists {
				continue
			}
			if targetChild == pair.target || reachesLocked(targetChild, pair.target) {
				panic("graph: patch would create a cycle")
			}
			pair.target.children[targetChild] = rev
			delete(pair.target.removedChildren, targetChild.key)
			targetChild.parents[pair.target] = struct{}{}
			changed[pair.target] = struct{}{}
		}
	}
	for target := range changed {
		target.selfRev = rev
		propagateLocked(target, rev)
		updateIndexesLocked(target, nil)
	}
	if len(changed) != 0 {
		for q := range state.queries {
			recomputeQueryLocked(q)
		}
	}

	roots := make([]*node, 0, len(selected(patch)))
	for _, source := range selected(patch) {
		roots = append(roots, mapped[source])
	}
	return Graph{nodes: roots}
}

func planMergeLocked(source, target *node, keyTypes []reflect.Type, mapped map[*node]*node, pairs *[]mergePair) {
	if existing, seen := mapped[source]; seen {
		if target != nil {
			if existing != nil && existing != target {
				panic("graph: patch identity matches multiple target nodes")
			}
			if existing == nil {
				mapped[source] = target
				for i := range *pairs {
					if (*pairs)[i].source == source {
						(*pairs)[i].target = target
						break
					}
				}
				planMergeChildrenLocked(source, target, keyTypes, mapped, pairs)
			}
		}
		return
	}
	mapped[source] = target
	*pairs = append(*pairs, mergePair{source: source, target: target})
	planMergeChildrenLocked(source, target, keyTypes, mapped, pairs)
}

func planMergeChildrenLocked(source, target *node, keyTypes []reflect.Type, mapped map[*node]*node, pairs *[]mergePair) {
	seenKeys := make([][]any, 0, len(source.children))
	for _, sourceChild := range orderedChildren(source) {
		key := nodeMatchKey(sourceChild, keyTypes)
		for _, seen := range seenKeys {
			if equalMatchKey(key, seen) {
				panic("graph: duplicate patch child match key")
			}
		}
		seenKeys = append(seenKeys, key)
		var match *node
		if target != nil {
			for _, candidate := range orderedChildren(target) {
				candidateKey, ok := optionalNodeMatchKey(candidate, keyTypes)
				if !ok || !equalMatchKey(key, candidateKey) {
					continue
				}
				if match != nil {
					panic("graph: multiple target children match patch key")
				}
				match = candidate
			}
		}
		planMergeLocked(sourceChild, match, keyTypes, mapped, pairs)
	}
}

func matchTypes(values []any) []reflect.Type {
	return identityTypes(values)
}

func identityTypes(values []any) []reflect.Type {
	if len(values) == 0 {
		return nil
	}
	requests := inspectRequests(values, false)
	types := make([]reflect.Type, len(requests))
	for i, request := range requests {
		types[i] = request.typ
	}
	return types
}

func sameTypes(a, b []reflect.Type) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func nodeMatchKey(n *node, types []reflect.Type) []any {
	key, ok := optionalNodeMatchKey(n, types)
	if !ok {
		panic("graph: patch child lacks a match attribute")
	}
	return key
}

func optionalNodeMatchKey(n *node, types []reflect.Type) ([]any, bool) {
	return optionalAttrsMatchKey(n.attrs, types)
}

func attrsMatchKey(attrs map[reflect.Type]any, types []reflect.Type) []any {
	key, ok := optionalAttrsMatchKey(attrs, types)
	if !ok {
		panic("graph: node lacks a match attribute")
	}
	return key
}

func optionalAttrsMatchKey(attrs map[reflect.Type]any, types []reflect.Type) ([]any, bool) {
	key := make([]any, len(types))
	for i, typ := range types {
		value, ok := attrs[typ]
		if !ok {
			return nil, false
		}
		key[i] = value
	}
	return key, true
}

func findIdentityMatch(attrs map[reflect.Type]any, candidates []*node, types []reflect.Type) *node {
	key := attrsMatchKey(attrs, types)
	var match *node
	for _, candidate := range candidates {
		candidateKey, ok := optionalNodeMatchKey(candidate, types)
		if !ok || !equalMatchKey(key, candidateKey) {
			continue
		}
		if match != nil {
			panic("graph: multiple target nodes match identity")
		}
		match = candidate
	}
	return match
}

func equalMatchKey(a, b []any) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func buildCommitLocked(n *node, base uint64, view *subgraph, visiting map[*node]bool) bool {
	if visiting[n] {
		return false
	}
	visiting[n] = true
	defer delete(visiting, n)
	include := n.selfRev > base
	change := nodeChange{
		attrs:         make(map[reflect.Type]any),
		addedChildren: make(map[uint64]struct{}),
		snapshot:      cloneAttrs(n.attrs),
	}
	for typ, rev := range n.attrRev {
		if rev > base {
			change.attrs[typ] = n.attrs[typ]
		}
	}
	for typ, rev := range n.removedAttrs {
		if rev > base {
			change.removedAttrs = append(change.removedAttrs, typ)
		}
	}
	for key, removed := range n.removedChildren {
		if removed.revision > base {
			change.removedChildren = append(change.removedChildren, removedChildChange{key: key, attrs: cloneAttrs(removed.attrs)})
		}
	}
	children := orderedChildren(n)
	for _, c := range children {
		edgeRev := n.children[c]
		if edgeRev > base {
			view.edges[n] = append(view.edges[n], c)
			change.addedChildren[c.key] = struct{}{}
			buildCommitLocked(c, 0, view, visiting)
			include = true
			continue
		}
		if c.treeRev > base && buildCommitLocked(c, base, view, visiting) {
			view.edges[n] = append(view.edges[n], c)
			include = true
		}
	}
	if include {
		view.changes[n] = change
	}
	return include
}

func selected(g Graph) []*node {
	if g.live != nil {
		return g.live.nodes
	}
	return g.nodes
}

func newNodeLocked(key uint64, attrs map[reflect.Type]any, rev uint64) *node {
	attrRev := make(map[reflect.Type]uint64, len(attrs))
	for typ := range attrs {
		attrRev[typ] = rev
	}
	return &node{
		id: state.nextID, key: key, attrs: attrs, attrRev: attrRev,
		removedAttrs: make(map[reflect.Type]uint64), children: make(map[*node]uint64),
		removedChildren: make(map[uint64]removedChild), parents: make(map[*node]struct{}),
		selfRev: rev, treeRev: rev,
	}
}

func cloneAttrs(attrs map[reflect.Type]any) map[reflect.Type]any {
	clone := make(map[reflect.Type]any, len(attrs))
	for typ, value := range attrs {
		clone[typ] = value
	}
	return clone
}

func reachableViewLocked(roots []*node, view *subgraph) []*node {
	seen := make(map[*node]struct{})
	out := make([]*node, 0)
	var visit func(*node)
	visit = func(n *node) {
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
		for _, child := range view.edges[n] {
			visit(child)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return out
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
	if len(out) > 1 {
		sortNodes(out)
	}
	return out
}
func sortNodes(ns []*node) {
	sort.Slice(ns, func(i, j int) bool { return ns[i].id < ns[j].id })
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
	q.scope = make(map[*node]uint32)
	q.root = make(map[*node]struct{})
	queue := make([]*node, 0, len(q.roots))
	for _, root := range q.roots {
		if _, duplicate := q.root[root]; duplicate {
			continue
		}
		q.root[root] = struct{}{}
		if q.scope[root] == 0 {
			queue = append(queue, root)
		}
		q.scope[root]++
	}
	reachable := make([]*node, 0)
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		reachable = append(reachable, n)
		for _, child := range orderedChildren(n) {
			if q.scope[child] == 0 {
				queue = append(queue, child)
			}
			q.scope[child]++
		}
	}
	candidates := reachable
	for _, key := range q.buckets {
		bucket := state.index[key]
		if bucket == nil || len(bucket.nodes) >= len(candidates) {
			continue
		}
		candidates = candidates[:0]
		for n := range bucket.nodes {
			candidates = append(candidates, n)
		}
		sortNodes(candidates)
	}
	out := make([]*node, 0, len(candidates))
	for _, n := range candidates {
		if q.scope[n] != 0 && matchesLocked(n, q.matchers) {
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
		if q.scope[n] != 0 {
			updateQueryNodeLocked(q, n)
		}
	}
}

func updateQueryNodeLocked(q *query, n *node) {
	index := -1
	for i, current := range q.result.nodes {
		if current == n {
			index = i
			break
		}
	}
	matches := matchesLocked(n, q.matchers)
	if matches && index < 0 {
		q.result.nodes = append(q.result.nodes, n)
	} else if !matches && index >= 0 {
		copy(q.result.nodes[index:], q.result.nodes[index+1:])
		q.result.nodes[len(q.result.nodes)-1] = nil
		q.result.nodes = q.result.nodes[:len(q.result.nodes)-1]
	}
}
func updateStructuralQueriesLocked(parent, child *node, linked bool) {
	for q := range state.queries {
		if q.scope[parent] == 0 {
			continue
		}
		if linked {
			wasUnreachable := q.scope[child] == 0
			q.scope[child]++
			if wasUnreachable {
				addQuerySubtreeLocked(q, child)
			}
		} else {
			removeQueryReferenceLocked(q, child)
		}
	}
}

func addQuerySubtreeLocked(q *query, n *node) {
	updateQueryNodeLocked(q, n)
	for _, child := range orderedChildren(n) {
		wasUnreachable := q.scope[child] == 0
		q.scope[child]++
		if wasUnreachable {
			addQuerySubtreeLocked(q, child)
		}
	}
}

func removeQueryReferenceLocked(q *query, n *node) {
	if q.scope[n] == 0 {
		return
	}
	q.scope[n]--
	if q.scope[n] != 0 {
		return
	}
	delete(q.scope, n)
	removeQueryResultNodeLocked(q, n)
	for _, child := range orderedChildren(n) {
		removeQueryReferenceLocked(q, child)
	}
}

func removeQueryResultNodeLocked(q *query, n *node) {
	for index, current := range q.result.nodes {
		if current != n {
			continue
		}
		copy(q.result.nodes[index:], q.result.nodes[index+1:])
		q.result.nodes[len(q.result.nodes)-1] = nil
		q.result.nodes = q.result.nodes[:len(q.result.nodes)-1]
		return
	}
}

func acquireIndexLocked(key indexKey) {
	if bucket := state.index[key]; bucket != nil {
		bucket.users++
		return
	}
	bucket := &indexBucket{nodes: make(map[*node]struct{}), users: 1}
	for n := range state.nodes {
		if indexMatchesLocked(n, key) {
			bucket.nodes[n] = struct{}{}
		}
	}
	state.index[key] = bucket
}

func releaseIndexLocked(key indexKey) {
	bucket := state.index[key]
	if bucket == nil {
		return
	}
	bucket.users--
	if bucket.users == 0 {
		delete(state.index, key)
	}
}

func updateIndexesLocked(n *node, changed map[reflect.Type]struct{}) {
	for key, bucket := range state.index {
		if changed != nil {
			if _, relevant := changed[key.typ]; !relevant {
				continue
			}
		}
		if indexMatchesLocked(n, key) {
			bucket.nodes[n] = struct{}{}
		} else {
			delete(bucket.nodes, n)
		}
	}
}

func indexMatchesLocked(n *node, key indexKey) bool {
	value, exists := n.attrs[key.typ]
	return exists && (key.any || value == key.value)
}
