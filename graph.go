// Package graph implements a small, type-oriented directed acyclic graph.
package graph

import (
	"fmt"
	"iter"
	"math/bits"
	"reflect"
	"sort"
	"sync"
)

// Graph is a selection of zero or more graph nodes. Its representation is
// deliberately private; nodes and relations are not values in the public API.
type Graph struct {
	nodes []*node
	view  *subgraph
	index *searchIndex
}

type node struct {
	id              uint64
	key             uint64
	attrs           map[reflect.Type]any
	attrRev         map[reflect.Type]uint64
	removedAttrs    map[reflect.Type]uint64
	children        map[*node]uint64 // edge creation revision
	childOrder      []*node
	removedChildren map[uint64]removedChild
	parents         map[*node]struct{}
	selfRev         uint64
	treeRev         uint64
	structureRev    uint64
	baseline        uint64
	index           *searchIndex
}

type searchIndex struct {
	root              *node
	structureRevision uint64
	nodes             []*node
	ordinal           map[*node]uint32
	parents           [][]uint32
	all               posting
	byType            map[reflect.Type]posting
	byValue           map[reflect.Type]map[any]posting
	dirty             map[uint32]struct{}
	overlayRevision   uint64
	removed           map[uint32]struct{}
	affected          map[uint32]struct{}
	added             []*node
	addedOrdinal      map[*node]uint32
	order             map[*node]uint32
	support           map[*node]uint32
	active            map[*node]struct{}
	orderDirty        bool
	orderOverlayBuilt bool
	addedEdges        map[graphEdge]struct{}
	removedEdges      map[graphEdge]struct{}
}

type posting []uint32

type graphEdge struct {
	parent *node
	child  *node
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
}

type matcher struct {
	typ   reflect.Type
	value any
	any   bool
}

type predicateOp uint8

const (
	predicateMatch predicateOp = iota
	predicateAny
	predicateAnd
	predicateOr
	predicatePath
)

type predicate struct {
	op       predicateOp
	matcher  matcher
	children []*predicate
}

type predicateExpression struct{ predicate *predicate }

// Type returns a matcher meaning "an attribute of type T, with any value".
//
// For example, this searches every reachable node with a Position attribute:
//
//	positions := Search(root, Type[Position]())
func Type[T any]() any {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if t == reflect.TypeOf((*any)(nil)).Elem() {
		return predicateExpression{predicate: &predicate{op: predicateAny}}
	}
	return typeMatcher{typ: normalizeType(t)}
}

// And groups predicates that must all match the same node.
//
// For example, this path step matches an Actor having a specific ID:
//
//	And(Type[Actor](), ID("actor-1"))
func And(values ...any) any {
	return predicateExpression{predicate: buildPredicate(predicateAnd, values)}
}

// Or groups predicates when at least one must match the same node.
//
// For example, this matches either faction:
//
//	Or(Faction("red"), Faction("blue"))
func Or(values ...any) any {
	return predicateExpression{predicate: buildPredicate(predicateOr, values)}
}

// Path creates a structural matcher starting at immediate children. Its steps
// must be connected by immediate outgoing relations.
//
// For example, this matches Location nodes connected through Contains nodes
// to a particular Actor:
//
//	Search(locations, Type[Location](), Path(
//		Type[Contains](),
//		And(Type[Actor](), ID("actor-1")),
//	))
func Path(values ...any) any {
	if len(values) == 0 {
		panic("graph: path requires at least one step")
	}
	steps := make([]*predicate, len(values))
	for i, value := range values {
		steps[i] = predicateFromValue(value)
		if containsPath(steps[i]) {
			panic("graph: Path cannot contain another Path")
		}
	}
	return predicateExpression{predicate: &predicate{op: predicatePath, children: steps}}
}

func containsPath(condition *predicate) bool {
	if condition.op == predicatePath {
		return true
	}
	for _, child := range condition.children {
		if containsPath(child) {
			return true
		}
	}
	return false
}

// New creates one independent node containing the supplied attributes.
//
// For example, this creates a node with Name and Position attributes:
//
//	player := New(Name("player"), Position{X: 10, Y: 20})
func New(value any, values ...any) Graph {
	all := append([]any{value}, values...)
	attrs := storedAttributes(all)
	state.Lock()
	defer state.Unlock()
	state.nextID++
	rev := nextRevisionLocked()
	n := newNodeLocked(state.nextID, attrs, rev)
	return Graph{nodes: []*node{n}}
}

type request struct {
	typ   reflect.Type
	value any
	any   bool
}

// Get returns an attribute from the first selected node.
func Get[T any](g Graph) (T, bool) {
	var zero T
	typ := attributeType[T]()
	state.RLock()
	defer state.RUnlock()
	n := first(g)
	if n == nil {
		return zero, false
	}
	value, ok := n.attrs[typ]
	if !ok {
		return zero, false
	}
	return value.(T), true
}

// Set adds or replaces an attribute on the first selected node. It reports
// whether the attribute was inserted or changed.
func Set[T any](g Graph, value T) bool {
	attrs := storedAttributes([]any{value})
	state.Lock()
	defer state.Unlock()
	n := first(g)
	if n == nil {
		return false
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
		return false
	}
	rev := nextRevisionLocked()
	n.selfRev = rev
	for t := range changedTypes {
		n.attrRev[t] = rev
	}
	propagateAttributeLocked(n, rev)
	return true
}

// Unset removes an attribute from the first selected node and returns its old
// value.
func Unset[T any](g Graph) (T, bool) {
	var zero T
	typ := attributeType[T]()
	state.Lock()
	defer state.Unlock()
	n := first(g)
	if n == nil {
		return zero, false
	}
	value, ok := n.attrs[typ]
	if !ok {
		return zero, false
	}
	delete(n.attrs, typ)
	delete(n.attrRev, typ)
	rev := nextRevisionLocked()
	n.selfRev = rev
	n.removedAttrs[typ] = rev
	propagateAttributeLocked(n, rev)
	return value.(T), true
}

// Link creates outgoing relations from the first node to the first node of
// each argument. It panics if any relation would create a cycle.
//
// For example, this attaches two children and returns the newly linked nodes:
//
//	linked := Link(parent, firstChild, secondChild)
func Link(g Graph, graphs ...Graph) bool {
	state.Lock()
	defer state.Unlock()
	parent := first(g)
	if parent == nil {
		return false
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
	changed := false
	for _, child := range candidates {
		if _, exists := parent.children[child]; exists {
			continue
		}
		rev := nextRevisionLocked()
		addChildLocked(parent, child, rev)
		delete(parent.removedChildren, child.key)
		child.parents[parent] = struct{}{}
		parent.selfRev = rev
		propagateStructureLocked(parent, rev)
		updateStructuralIndexesLocked(parent, child, true)
		changed = true
	}
	return changed
}

// Unlink removes existing outgoing relations.
//
// For example, this detaches child and reports whether the relation existed:
//
//	removed := !Empty(Unlink(parent, child))
func Unlink(g Graph, graphs ...Graph) bool {
	state.Lock()
	defer state.Unlock()
	parent := first(g)
	if parent == nil {
		return false
	}
	changed := false
	for _, child := range graphFirsts(graphs) {
		if _, exists := parent.children[child]; !exists {
			continue
		}
		removeChildLocked(parent, child)
		delete(child.parents, parent)
		rev := nextRevisionLocked()
		parent.removedChildren[child.key] = removedChild{revision: rev, attrs: cloneAttrs(child.attrs)}
		parent.selfRev = rev
		propagateStructureLocked(parent, rev)
		updateStructuralIndexesLocked(parent, child, false)
		changed = true
	}
	return changed
}

// Each iterates over singleton Graph values in selection order.
//
// For example, this visits every node in a selection:
//
//	for node := range Each(selection) {
//		Set(node, Visible(true))
//	}
func Each(g Graph) iter.Seq[Graph] {
	state.RLock()
	nodes := append([]*node(nil), selected(g)...)
	view := g.view
	index := graphSearchIndex(g)
	state.RUnlock()
	return func(yield func(Graph) bool) {
		for _, n := range nodes {
			if !yield(Graph{nodes: []*node{n}, view: view, index: index}) {
				return
			}
		}
	}
}

// Len returns the number of nodes in the selection.
//
// For example:
//
//	count := Len(Follow(parent, Path(Type[any]())))
func Len(g Graph) int { state.RLock(); defer state.RUnlock(); return len(selected(g)) }

// Empty reports whether the selection contains no nodes.
//
// For example:
//
//	if Empty(Select(player, Type[Position]())) {
//		Set(player, Position{})
//	}
func Empty(g Graph) bool { return Len(g) == 0 }

// Union returns the ordered union of all selections.
//
// For example, duplicates are removed while the first occurrence order is
// retained:
//
//	all := Union(firstSelection, secondSelection, firstSelection)
func Union(graphs ...Graph) Graph {
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

// Intersect returns the intersection of all selections in first-selection order.
//
// For example, this keeps nodes present in both selections:
//
//	visiblePlayers := Intersect(players, visible)
func Intersect(graphs ...Graph) Graph {
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

// Difference subtracts every later selection from the first.
//
// For example, this selects players that are not hidden or disconnected:
//
//	activePlayers := Difference(players, hidden, disconnected)
func Difference(graphs ...Graph) Graph {
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

// Select filters the currently selected nodes and returns matching starts.
func Select(g Graph, values ...any) Graph {
	condition := buildPredicate(predicateAnd, values)
	state.RLock()
	defer state.RUnlock()
	index := graphSearchIndex(g)
	var indexedMatches posting
	if index != nil && containsPath(condition) {
		indexedMatches = index.matchCurrent(condition)
	}
	out := make([]*node, 0, len(selected(g)))
	for _, n := range selected(g) {
		if index != nil && containsPath(condition) && !index.postingContains(indexedMatches, n) {
			continue
		}
		if matchesPredicateLocked(n, condition, g.view) {
			out = append(out, n)
		}
	}
	return Graph{nodes: out, view: g.view, index: index}
}

// Follow filters the currently selected nodes and returns the endpoints of
// matching expressions. Without Path it is equivalent to Select.
func Follow(g Graph, values ...any) Graph {
	condition := buildPredicate(predicateAnd, values)
	state.RLock()
	defer state.RUnlock()
	index := graphSearchIndex(g)
	var indexedMatches posting
	if index != nil && containsPath(condition) {
		indexedMatches = index.matchCurrent(condition)
	}
	out := make([]*node, 0)
	seen := make(map[*node]struct{})
	for _, n := range selected(g) {
		if index != nil && containsPath(condition) && !index.postingContains(indexedMatches, n) {
			continue
		}
		result := evaluatePredicateLocked(n, condition, g.view)
		if !result.matched {
			continue
		}
		for _, match := range result.endpoints {
			if _, duplicate := seen[match]; duplicate {
				continue
			}
			seen[match] = struct{}{}
			out = append(out, match)
		}
	}
	return Graph{nodes: out, view: g.view, index: index}
}

// Search recursively filters every node reachable from the current selection
// and returns matching starts.
func Search(g Graph, values ...any) Graph {
	condition := buildPredicate(predicateAnd, values)
	if result, rebuilt := searchWhileRebuildingOrder(g, condition); rebuilt {
		return result
	}
	prepareSearchOrder(g)
	state.RLock()
	defer state.RUnlock()
	roots := selected(g)
	if len(roots) == 1 && g.view == nil && validSearchIndex(roots[0]) {
		matches := roots[0].index.orderMatches(roots[0].index.matchCurrent(condition))
		out := make([]*node, len(matches))
		for i, ordinal := range matches {
			out[i] = roots[0].index.nodeAt(ordinal)
		}
		return Graph{nodes: out, index: roots[0].index}
	}
	visited := make(map[*node]struct{})
	var emitted map[*node]struct{}
	if len(roots) > 1 {
		emitted = make(map[*node]struct{})
	}
	out := make([]*node, 0)
	var visit func(*node)
	visit = func(n *node) {
		if _, duplicate := visited[n]; duplicate {
			return
		}
		visited[n] = struct{}{}
		if matchesPredicateLocked(n, condition, g.view) {
			if emitted == nil {
				out = append(out, n)
			} else if _, duplicate := emitted[n]; !duplicate {
				emitted[n] = struct{}{}
				out = append(out, n)
			}
		}
		for _, child := range selectedChildrenLocked(n, g.view) {
			visit(child)
		}
	}
	for _, root := range roots {
		if root == nil {
			continue
		}
		if g.view == nil && validSearchIndex(root) {
			for _, ordinal := range root.index.orderMatches(root.index.matchCurrent(condition)) {
				n := root.index.nodeAt(ordinal)
				if _, duplicate := emitted[n]; !duplicate {
					emitted[n] = struct{}{}
					out = append(out, n)
				}
			}
			continue
		}
		visit(root)
	}
	return Graph{nodes: out, view: g.view}
}

func searchWhileRebuildingOrder(g Graph, condition *predicate) (Graph, bool) {
	state.Lock()
	defer state.Unlock()
	roots := selected(g)
	if len(roots) != 1 || g.view != nil || !validSearchIndex(roots[0]) || !roots[0].index.orderDirty {
		return Graph{}, false
	}
	index := roots[0].index
	index.order = make(map[*node]uint32, len(index.active))
	seen := make(map[*node]struct{}, len(index.active))
	out := make([]*node, 0)
	var visit func(*node)
	visit = func(n *node) {
		if _, duplicate := seen[n]; duplicate {
			return
		}
		seen[n] = struct{}{}
		index.order[n] = uint32(len(index.order))
		if matchesPredicateLocked(n, condition, nil) {
			out = append(out, n)
		}
		for _, child := range n.childOrder {
			visit(child)
		}
	}
	visit(roots[0])
	index.orderDirty = false
	index.orderOverlayBuilt = true
	return Graph{nodes: out, index: index}, true
}

func prepareSearchOrder(g Graph) {
	state.Lock()
	defer state.Unlock()
	for _, root := range selected(g) {
		if validSearchIndex(root) && root.index.orderDirty {
			root.index.rebuildOrderLocked()
		}
	}
}

// Commit advances the baseline of every node reachable from the selection to
// the current revision and publishes an immutable search index for each root.
// Changes at or before that revision are omitted from subsequent Delta calls
// through any root that reaches those nodes.
//
// For example, commit after synchronizing a replica so that the next delta
// contains only later changes:
//
//	replica = Apply(replica, Delta(source))
//	Commit(source)
func Commit(g Graph) {
	state.Lock()
	defer state.Unlock()
	seenRoots := make(map[*node]struct{})
	for _, root := range selected(g) {
		if _, duplicate := seenRoots[root]; duplicate {
			continue
		}
		seenRoots[root] = struct{}{}
		nodes := reachableLocked([]*node{root})
		for _, n := range nodes {
			n.baseline = state.revision
		}
		root.index = buildSearchIndexLocked(root, nodes)
	}
}

// Delta returns the current paths from each selected root to changes since its
// preceding Commit. It does not advance the baseline, so repeated calls return
// the same changes. Optional matchBy attributes define the stable composite
// identity Apply must use.
//
// For example, this creates a delta whose nodes are identified by EntityID:
//
//	changes := Delta(source, Type[EntityID]())
func Delta(g Graph, matchBy ...any) Graph {
	state.Lock()
	defer state.Unlock()
	keyTypes := identityTypes(matchBy)
	view := &subgraph{edges: make(map[*node][]*node), changes: make(map[*node]nodeChange), matchTypes: keyTypes}
	roots := []*node{}
	for _, root := range selected(g) {
		if root.treeRev <= root.baseline {
			continue
		}
		if buildDeltaLocked(root, view, map[*node]bool{}, false) {
			roots = append(roots, root)
		}
	}
	if len(keyTypes) != 0 {
		identities := &compositeIndex{types: keyTypes}
		for n, change := range view.changes {
			nodeMatchKey(n, keyTypes)
			if !identities.addAttrs(change.snapshot, n) {
				panic("graph: multiple delta nodes have the same identity")
			}
			for _, removed := range change.removedChildren {
				attrsMatchKey(removed.attrs, keyTypes)
			}
		}
	}
	if len(roots) == 0 {
		return Graph{}
	}
	return Graph{nodes: roots, view: view}
}

// Apply merges a Delta result into g and returns the corresponding roots.
// Deltas use their internal node identity unless identity attributes were
// supplied to Delta.
//
// For example, this applies an EntityID-keyed delta to a replica:
//
//	changes := Delta(source, Type[EntityID]())
//	replica = Apply(replica, changes)
func Apply(g, delta Graph) Graph {
	state.Lock()
	defer state.Unlock()
	if len(selected(delta)) == 0 {
		return g
	}
	if delta.view == nil {
		panic("graph: apply requires a delta")
	}
	keyTypes := delta.view.matchTypes

	targets := make(map[uint64]*node)
	targetScope := reachableLocked(selected(g))
	var keyedTargets *compositeIndex
	if len(keyTypes) == 0 {
		for _, n := range targetScope {
			if _, duplicate := targets[n.key]; duplicate {
				panic("graph: replica contains duplicate node identity")
			}
			targets[n.key] = n
		}
	} else {
		keyedTargets = newCompositeIndex(keyTypes, targetScope)
	}
	deltaNodes := reachableViewLocked(selected(delta), delta.view)
	matches := make(map[*node]*node, len(deltaNodes))
	rev := nextRevisionLocked()
	created := make(map[*node]bool)
	for _, source := range deltaNodes {
		var target *node
		if len(keyTypes) == 0 {
			target = targets[source.key]
		} else {
			target = keyedTargets.find(delta.view.changes[source].snapshot)
		}
		if target != nil {
			matches[source] = target
			continue
		}
		change := delta.view.changes[source]
		state.nextID++
		key := source.key
		if len(keyTypes) != 0 {
			key = state.nextID
		}
		target = newNodeLocked(key, cloneAttrs(change.snapshot), rev)
		target.id = state.nextID
		targets[source.key] = target
		matches[source] = target
		if keyedTargets != nil {
			keyedTargets.add(target)
		}
		created[target] = true
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
				child = keyedTargets.find(removed.attrs)
			}
			if child == nil {
				continue
			}
			if _, ok := parent.children[child]; ok {
				removeChildLocked(parent, child)
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
				addChildLocked(parent, child, rev)
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
		propagateStructureLocked(target, rev)
	}
	roots := make([]*node, 0, len(selected(delta)))
	for _, source := range selected(delta) {
		roots = append(roots, matches[source])
	}
	return Graph{nodes: roots}
}

// Patch additively merges an ordinary graph by a composite attribute key,
// matching nodes across the entire target graph. A patch root without the key
// attributes corresponds by selection order.
//
// For example:
//
//	Patch(world, changes, Type[EntityID]())
func Patch(g, patch Graph, matchBy ...any) Graph {
	state.Lock()
	defer state.Unlock()
	if len(selected(patch)) == 0 {
		return g
	}
	if patch.view != nil {
		panic("graph: patch requires an ordinary graph")
	}
	return mergeGraphLocked(g, patch, matchTypes(matchBy))
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
	targets := newCompositeIndex(keyTypes, reachableLocked(targetRoots))
	pairs := make([]mergePair, 0)
	mapped := make(map[*node]*node)
	patchNodes := &compositeIndex{types: keyTypes}
	for i, source := range selected(patch) {
		var target *node
		if _, keyed := optionalNodeMatchKey(source, keyTypes); keyed {
			target = targets.find(source.attrs)
		} else if i < len(targetRoots) {
			target = targetRoots[i]
		}
		planMergeLocked(source, target, targets, patchNodes, mapped, &pairs)
	}

	rev := nextRevisionLocked()
	created := make(map[*node]struct{})
	for i := range pairs {
		if pairs[i].target != nil {
			continue
		}
		state.nextID++
		target := newNodeLocked(state.nextID, cloneAttrs(pairs[i].source.attrs), rev)
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
			addChildLocked(pair.target, targetChild, rev)
			delete(pair.target.removedChildren, targetChild.key)
			targetChild.parents[pair.target] = struct{}{}
			changed[pair.target] = struct{}{}
		}
	}
	for target := range changed {
		target.selfRev = rev
		propagateStructureLocked(target, rev)
	}
	roots := make([]*node, 0, len(selected(patch)))
	for _, source := range selected(patch) {
		roots = append(roots, mapped[source])
	}
	return Graph{nodes: roots}
}

func planMergeLocked(source, target *node, targets, patchNodes *compositeIndex, mapped map[*node]*node, pairs *[]mergePair) {
	if _, seen := mapped[source]; seen {
		return
	}
	if _, keyed := optionalNodeMatchKey(source, targets.types); keyed && !patchNodes.add(source) {
		panic("graph: duplicate patch match key")
	}
	mapped[source] = target
	*pairs = append(*pairs, mergePair{source: source, target: target})
	planMergeChildrenLocked(source, targets, patchNodes, mapped, pairs)
}

func planMergeChildrenLocked(source *node, targets, patchNodes *compositeIndex, mapped map[*node]*node, pairs *[]mergePair) {
	for _, sourceChild := range orderedChildren(source) {
		match := targets.find(sourceChild.attrs)
		planMergeLocked(sourceChild, match, targets, patchNodes, mapped, pairs)
	}
}

func matchTypes(values []any) []reflect.Type {
	return identityTypes(values)
}

func identityTypes(values []any) []reflect.Type {
	if len(values) == 0 {
		return nil
	}
	requests := inspectRequests(values)
	types := make([]reflect.Type, len(requests))
	for i, request := range requests {
		types[i] = request.typ
	}
	return types
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

type compositeIndex struct {
	types []reflect.Type
	root  compositeIndexLevel
}

type compositeIndexLevel struct {
	children  map[any]*compositeIndexLevel
	node      *node
	duplicate bool
}

func newCompositeIndex(types []reflect.Type, nodes []*node) *compositeIndex {
	index := &compositeIndex{types: types}
	for _, n := range nodes {
		index.add(n)
	}
	return index
}

func (i *compositeIndex) add(n *node) bool {
	return i.addAttrs(n.attrs, n)
}

func (i *compositeIndex) addAttrs(attrs map[reflect.Type]any, n *node) bool {
	key, ok := optionalAttrsMatchKey(attrs, i.types)
	if !ok {
		return true
	}
	level := &i.root
	for _, value := range key {
		if level.children == nil {
			level.children = make(map[any]*compositeIndexLevel)
		}
		next := level.children[value]
		if next == nil {
			next = &compositeIndexLevel{}
			level.children[value] = next
		}
		level = next
	}
	if level.node != nil && level.node != n {
		level.duplicate = true
		return false
	}
	level.node = n
	return true
}

func (i *compositeIndex) find(attrs map[reflect.Type]any) *node {
	key := attrsMatchKey(attrs, i.types)
	level := &i.root
	for _, value := range key {
		level = level.children[value]
		if level == nil {
			return nil
		}
	}
	if level.duplicate {
		panic("graph: multiple target nodes match identity")
	}
	return level.node
}

func buildDeltaLocked(n *node, view *subgraph, visiting map[*node]bool, full bool) bool {
	if visiting[n] {
		return false
	}
	visiting[n] = true
	defer delete(visiting, n)
	base := n.baseline
	if full {
		base = 0
	}
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
			buildDeltaLocked(c, view, visiting, true)
			include = true
			continue
		}
		if c.treeRev > c.baseline && buildDeltaLocked(c, view, visiting, false) {
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
		selfRev: rev, treeRev: rev, structureRev: rev,
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

func inspectRequests(values []any) []request {
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
				panic("graph: pointer attribute request")
			}
			if t.Kind() == reflect.Interface || !t.Comparable() {
				panic("graph: attribute request must be comparable")
			}
			r.typ = t
			r.value = v
		}
		if _, ok := seen[r.typ]; ok {
			panic("graph: duplicate attribute type " + r.typ.String())
		}
		seen[r.typ] = struct{}{}
		out = append(out, r)
	}
	return out
}

func buildPredicate(op predicateOp, values []any) *predicate {
	if len(values) == 0 {
		panic("graph: logical expression requires at least one predicate")
	}
	children := make([]*predicate, len(values))
	for i, value := range values {
		children[i] = predicateFromValue(value)
	}
	return combinePredicates(op, children)
}

func combinePredicates(op predicateOp, children []*predicate) *predicate {
	if len(children) == 1 {
		return children[0]
	}
	return &predicate{op: op, children: children}
}

func predicateFromValue(value any) *predicate {
	if expression, ok := value.(predicateExpression); ok {
		return expression.predicate
	}
	request := inspectRequests([]any{value})[0]
	return &predicate{op: predicateMatch, matcher: matcher(request)}
}

func attributeType[T any]() reflect.Type {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	if typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Interface || !typ.Comparable() {
		panic("graph: attribute type must be a comparable non-pointer value")
	}
	return typ
}

func nextRevisionLocked() uint64 { state.revision++; return state.revision }
func propagateAttributeLocked(n *node, rev uint64) {
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
		if x.index != nil {
			if ordinal, exists := x.index.ordinal[n]; exists {
				if x.index.attributesCommitted(n, ordinal) {
					delete(x.index.dirty, ordinal)
				} else {
					x.index.dirty[ordinal] = struct{}{}
				}
			}
		}
		for p := range x.parents {
			visit(p)
		}
	}
	visit(n)
}

func (index *searchIndex) attributesCommitted(n *node, ordinal uint32) bool {
	committedCount := 0
	for typ, values := range index.byType {
		if postingHas(values, ordinal) {
			committedCount++
			value, exists := n.attrs[typ]
			if !exists || !postingHas(index.byValue[typ][value], ordinal) {
				return false
			}
		}
	}
	return committedCount == len(n.attrs)
}

func postingHas(values posting, ordinal uint32) bool {
	position := sort.Search(len(values), func(i int) bool { return values[i] >= ordinal })
	return position < len(values) && values[position] == ordinal
}

func propagateStructureLocked(n *node, rev uint64) {
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
		if x.structureRev < rev {
			x.structureRev = rev
		}
		for parent := range x.parents {
			visit(parent)
		}
	}
	visit(n)
}

func updateStructuralIndexesLocked(parent, child *node, linked bool) {
	seen := make(map[*node]struct{})
	var visit func(*node)
	visit = func(n *node) {
		if _, duplicate := seen[n]; duplicate {
			return
		}
		seen[n] = struct{}{}
		if n.index != nil {
			n.index.updateStructureLocked(parent, child, linked)
		}
		for ancestor := range n.parents {
			visit(ancestor)
		}
	}
	visit(parent)
}

func (index *searchIndex) updateStructureLocked(parent, child *node, linked bool) {
	if _, parentActive := index.active[parent]; !parentActive {
		index.overlayRevision = index.root.structureRev
		return
	}
	index.normalizeEdgeLocked(parent, child, linked)
	if linked {
		wasInactive := index.support[child] == 0
		index.support[child]++
		if wasInactive {
			index.activateLocked(child)
		}
	} else {
		if index.support[child] == 0 {
			panic("graph: invalid structural index support")
		}
		index.support[child]--
		if index.support[child] == 0 {
			index.deactivateLocked(child)
		}
	}
	index.rebuildAffectedLocked()
	if len(index.addedEdges) == 0 && len(index.removedEdges) == 0 && len(index.removed) == 0 && !index.hasActiveAddedNodes() {
		index.resetCommittedOrderLocked()
	} else {
		index.orderDirty = true
	}
	index.overlayRevision = index.root.structureRev
}

func (index *searchIndex) normalizeEdgeLocked(parent, child *node, linked bool) {
	edge := graphEdge{parent: parent, child: child}
	committed := index.hasCommittedEdge(parent, child)
	if linked {
		if committed {
			delete(index.removedEdges, edge)
		} else {
			index.addedEdges[edge] = struct{}{}
		}
		return
	}
	if committed {
		index.removedEdges[edge] = struct{}{}
	} else {
		delete(index.addedEdges, edge)
	}
}

func (index *searchIndex) hasCommittedEdge(parent, child *node) bool {
	parentOrdinal, parentOK := index.ordinal[parent]
	childOrdinal, childOK := index.ordinal[child]
	if !parentOK || !childOK {
		return false
	}
	parents := index.parents[childOrdinal]
	position := sort.Search(len(parents), func(i int) bool { return parents[i] >= parentOrdinal })
	return position < len(parents) && parents[position] == parentOrdinal
}

func (index *searchIndex) rebuildAffectedLocked() {
	clear(index.affected)
	for edge := range index.addedEdges {
		index.markAffectedAncestorsLocked(edge.parent)
	}
	for edge := range index.removedEdges {
		index.markAffectedAncestorsLocked(edge.parent)
	}
}

func (index *searchIndex) hasActiveAddedNodes() bool {
	for _, n := range index.added {
		if _, active := index.active[n]; active {
			return true
		}
	}
	return false
}

func (index *searchIndex) resetCommittedOrderLocked() {
	if index.orderOverlayBuilt {
		index.order = make(map[*node]uint32, len(index.nodes))
		for ordinal, n := range index.nodes {
			index.order[n] = uint32(ordinal)
		}
	}
	index.orderDirty = false
	index.orderOverlayBuilt = false
}

func (index *searchIndex) activateLocked(n *node) {
	if _, active := index.active[n]; active {
		return
	}
	index.active[n] = struct{}{}
	if ordinal, committed := index.ordinal[n]; committed {
		delete(index.removed, ordinal)
	} else if _, known := index.addedOrdinal[n]; !known {
		ordinal := uint32(len(index.nodes) + len(index.added))
		index.addedOrdinal[n] = ordinal
		index.added = append(index.added, n)
	}
	for _, child := range n.childOrder {
		wasInactive := index.support[child] == 0
		index.support[child]++
		if wasInactive {
			index.activateLocked(child)
		}
	}
}

func (index *searchIndex) deactivateLocked(n *node) {
	if _, active := index.active[n]; !active {
		return
	}
	delete(index.active, n)
	if ordinal, committed := index.ordinal[n]; committed {
		index.removed[ordinal] = struct{}{}
	}
	for _, child := range n.childOrder {
		if index.support[child] == 0 {
			continue
		}
		index.support[child]--
		if index.support[child] == 0 {
			index.deactivateLocked(child)
		}
	}
}

func (index *searchIndex) markAffectedAncestorsLocked(start *node) {
	seen := make(map[*node]struct{})
	var visit func(*node)
	visit = func(n *node) {
		if _, duplicate := seen[n]; duplicate {
			return
		}
		seen[n] = struct{}{}
		if ordinal, committed := index.ordinal[n]; committed {
			if _, active := index.active[n]; active {
				index.affected[ordinal] = struct{}{}
			}
		}
		for parent := range n.parents {
			visit(parent)
		}
	}
	visit(start)
}

func (index *searchIndex) rebuildOrderLocked() {
	current := reachableLocked([]*node{index.root})
	index.order = make(map[*node]uint32, len(current))
	for position, n := range current {
		index.order[n] = uint32(position)
	}
	index.orderDirty = false
	index.orderOverlayBuilt = true
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
	return n.childOrder
}

func addChildLocked(parent, child *node, revision uint64) {
	parent.children[child] = revision
	index := sort.Search(len(parent.childOrder), func(i int) bool {
		return parent.childOrder[i].id >= child.id
	})
	parent.childOrder = append(parent.childOrder, nil)
	copy(parent.childOrder[index+1:], parent.childOrder[index:])
	parent.childOrder[index] = child
}

func removeChildLocked(parent, child *node) {
	delete(parent.children, child)
	for index, current := range parent.childOrder {
		if current != child {
			continue
		}
		copy(parent.childOrder[index:], parent.childOrder[index+1:])
		parent.childOrder[len(parent.childOrder)-1] = nil
		parent.childOrder = parent.childOrder[:len(parent.childOrder)-1]
		return
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

func buildSearchIndexLocked(root *node, nodes []*node) *searchIndex {
	index := &searchIndex{
		root:              root,
		structureRevision: root.structureRev,
		nodes:             append([]*node(nil), nodes...),
		ordinal:           make(map[*node]uint32, len(nodes)),
		parents:           make([][]uint32, len(nodes)),
		all:               make(posting, len(nodes)),
		byType:            make(map[reflect.Type]posting),
		byValue:           make(map[reflect.Type]map[any]posting),
		dirty:             make(map[uint32]struct{}),
		overlayRevision:   root.structureRev,
		removed:           make(map[uint32]struct{}),
		affected:          make(map[uint32]struct{}),
		addedOrdinal:      make(map[*node]uint32),
		order:             make(map[*node]uint32, len(nodes)),
		support:           make(map[*node]uint32, len(nodes)),
		active:            make(map[*node]struct{}, len(nodes)),
		addedEdges:        make(map[graphEdge]struct{}),
		removedEdges:      make(map[graphEdge]struct{}),
	}
	for ordinal, n := range nodes {
		if uint64(ordinal) > uint64(^uint32(0)) {
			panic("graph: search index exceeds uint32 capacity")
		}
		id := uint32(ordinal)
		index.ordinal[n] = id
		index.all[ordinal] = id
		index.order[n] = id
		index.active[n] = struct{}{}
	}
	for ordinal, n := range nodes {
		id := uint32(ordinal)
		for typ, value := range n.attrs {
			index.byType[typ] = appendIndexPosting(index.byType[typ], id, index.all)
			values := index.byValue[typ]
			if values == nil {
				values = make(map[any]posting)
				index.byValue[typ] = values
			}
			values[value] = appendIndexPosting(values[value], id, index.all)
		}
	}
	for ordinal, n := range nodes {
		parents := make([]uint32, 0, len(n.parents))
		for parent := range n.parents {
			if id, exists := index.ordinal[parent]; exists {
				parents = append(parents, id)
			}
		}
		sort.Slice(parents, func(i, j int) bool { return parents[i] < parents[j] })
		index.parents[ordinal] = parents
	}
	index.support[root] = 1
	for _, parent := range nodes {
		for _, child := range parent.childOrder {
			if _, exists := index.ordinal[child]; exists {
				index.support[child]++
			}
		}
	}
	return index
}

func appendIndexPosting(values posting, ordinal uint32, all posting) posting {
	if len(values) == 0 {
		index := int(ordinal)
		return all[index : index+1 : index+1]
	}
	return append(values, ordinal)
}

func validSearchIndex(root *node) bool {
	return root.index != nil && root.index.root == root && root.index.overlayRevision == root.structureRev
}

func (index *searchIndex) match(condition *predicate) posting {
	switch condition.op {
	case predicateMatch:
		if condition.matcher.any {
			return index.byType[condition.matcher.typ]
		}
		return index.byValue[condition.matcher.typ][condition.matcher.value]
	case predicateAny:
		return index.all
	case predicateAnd:
		operands := make([]posting, len(condition.children))
		for i, child := range condition.children {
			operands[i] = index.match(child)
			if len(operands[i]) == 0 {
				return nil
			}
		}
		sort.Slice(operands, func(i, j int) bool { return len(operands[i]) < len(operands[j]) })
		result := operands[0]
		for _, operand := range operands[1:] {
			result = intersectPostings(result, operand)
			if len(result) == 0 {
				return nil
			}
		}
		return result
	case predicateOr:
		operands := make([]posting, len(condition.children))
		for i, child := range condition.children {
			operands[i] = index.match(child)
		}
		sort.Slice(operands, func(i, j int) bool { return len(operands[i]) < len(operands[j]) })
		var result posting
		for _, operand := range operands {
			result = unionPostings(result, operand)
		}
		return result
	case predicatePath:
		current := index.match(condition.children[len(condition.children)-1])
		for step := len(condition.children) - 2; step >= 0 && len(current) != 0; step-- {
			current = intersectPostings(index.parentPosting(current), index.match(condition.children[step]))
		}
		return index.parentPosting(current)
	default:
		panic("graph: invalid predicate")
	}
}

func (index *searchIndex) matchCurrent(condition *predicate) posting {
	base := index.match(condition)
	structural := len(index.addedEdges) != 0 || len(index.removedEdges) != 0 || len(index.removed) != 0 || index.hasActiveAddedNodes()
	if len(index.dirty) == 0 && !structural {
		return base
	}
	path := containsPath(condition)
	affected := index.dirtyPosting(path)
	if path {
		affected = unionPostings(affected, postingFromSet(index.affected))
	}
	removed := postingFromSet(index.removed)
	excluded := unionPostings(affected, removed)
	current := make(posting, 0, len(affected))
	for _, ordinal := range affected {
		if _, unreachable := index.removed[ordinal]; !unreachable && matchesPredicateLocked(index.nodes[ordinal], condition, nil) {
			current = append(current, ordinal)
		}
	}
	for offset, n := range index.added {
		if _, active := index.active[n]; active && matchesPredicateLocked(n, condition, nil) {
			current = append(current, uint32(len(index.nodes)+offset))
		}
	}
	return mergePostingOverlay(base, excluded, current)
}

func postingFromSet(values map[uint32]struct{}) posting {
	result := make(posting, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func (index *searchIndex) dirtyPosting(includeAncestors bool) posting {
	if !includeAncestors {
		result := make(posting, 0, len(index.dirty))
		for ordinal := range index.dirty {
			result = append(result, ordinal)
		}
		sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
		return result
	}
	words := make([]uint64, (len(index.nodes)+63)/64)
	queue := make([]uint32, 0, len(index.dirty))
	for ordinal := range index.dirty {
		words[ordinal/64] |= uint64(1) << (ordinal % 64)
		queue = append(queue, ordinal)
	}
	for len(queue) != 0 {
		ordinal := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, parent := range index.parents[ordinal] {
			mask := uint64(1) << (parent % 64)
			if words[parent/64]&mask != 0 {
				continue
			}
			words[parent/64] |= mask
			queue = append(queue, parent)
		}
	}
	result := make(posting, 0, len(index.dirty))
	for wordIndex, word := range words {
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			result = append(result, uint32(wordIndex*64+bit))
			word &= word - 1
		}
	}
	return result
}

func graphSearchIndex(g Graph) *searchIndex {
	if g.view != nil {
		return nil
	}
	if g.index != nil && g.index.root.index == g.index && g.index.overlayRevision == g.index.root.structureRev {
		return g.index
	}
	if len(g.nodes) == 1 && validSearchIndex(g.nodes[0]) {
		return g.nodes[0].index
	}
	return nil
}

func (index *searchIndex) postingContains(values posting, n *node) bool {
	ordinal, exists := index.ordinal[n]
	if !exists {
		ordinal, exists = index.addedOrdinal[n]
	}
	if !exists {
		return false
	}
	position := sort.Search(len(values), func(i int) bool { return values[i] >= ordinal })
	return position < len(values) && values[position] == ordinal
}

func (index *searchIndex) nodeAt(ordinal uint32) *node {
	if int(ordinal) < len(index.nodes) {
		return index.nodes[ordinal]
	}
	return index.added[int(ordinal)-len(index.nodes)]
}

func (index *searchIndex) orderMatches(values posting) posting {
	if len(index.addedEdges) == 0 && len(index.removedEdges) == 0 && len(index.removed) == 0 && !index.hasActiveAddedNodes() {
		return values
	}
	ordered := true
	for i := 1; i < len(values); i++ {
		if index.order[index.nodeAt(values[i-1])] > index.order[index.nodeAt(values[i])] {
			ordered = false
			break
		}
	}
	if ordered {
		return values
	}
	result := append(posting(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		return index.order[index.nodeAt(result[i])] < index.order[index.nodeAt(result[j])]
	})
	return result
}

func (index *searchIndex) parentPosting(children posting) posting {
	if len(children) == 0 {
		return nil
	}
	words := make([]uint64, (len(index.nodes)+63)/64)
	for _, child := range children {
		for _, parent := range index.parents[child] {
			words[parent/64] |= uint64(1) << (parent % 64)
		}
	}
	result := make(posting, 0)
	for ordinal := range index.nodes {
		id := uint32(ordinal)
		if words[id/64]&(uint64(1)<<(id%64)) != 0 {
			result = append(result, id)
		}
	}
	return result
}

func intersectPostings(left, right posting) posting {
	result := make(posting, 0, min(len(left), len(right)))
	for i, j := 0, 0; i < len(left) && j < len(right); {
		switch {
		case left[i] < right[j]:
			i++
		case left[i] > right[j]:
			j++
		default:
			result = append(result, left[i])
			i++
			j++
		}
	}
	return result
}

func mergePostingOverlay(base, excluded, replacements posting) posting {
	result := make(posting, 0, len(base)+len(replacements))
	for i, removed, replacement := 0, 0, 0; i < len(base) || replacement < len(replacements); {
		for removed < len(excluded) && i < len(base) && excluded[removed] < base[i] {
			removed++
		}
		for i < len(base) && removed < len(excluded) && base[i] == excluded[removed] {
			i++
			removed++
		}
		if replacement < len(replacements) && (i == len(base) || replacements[replacement] < base[i]) {
			result = append(result, replacements[replacement])
			replacement++
			continue
		}
		if i < len(base) {
			result = append(result, base[i])
			i++
		}
	}
	return result
}

func unionPostings(left, right posting) posting {
	result := make(posting, 0, len(left)+len(right))
	for i, j := 0, 0; i < len(left) || j < len(right); {
		switch {
		case j == len(right) || i < len(left) && left[i] < right[j]:
			result = append(result, left[i])
			i++
		case i == len(left) || right[j] < left[i]:
			result = append(result, right[j])
			j++
		default:
			result = append(result, left[i])
			i++
			j++
		}
	}
	return result
}

func matchesMatcherLocked(n *node, m matcher) bool {
	v, ok := n.attrs[m.typ]
	return ok && (m.any || v == m.value)
}

func matchesPredicateLocked(n *node, condition *predicate, view *subgraph) bool {
	switch condition.op {
	case predicateMatch:
		return matchesMatcherLocked(n, condition.matcher)
	case predicateAny:
		return true
	case predicateAnd:
		// Cheap local predicates reject candidates before structural traversal.
		for _, child := range condition.children {
			if child.op != predicatePath && !matchesPredicateLocked(n, child, view) {
				return false
			}
		}
		for _, child := range condition.children {
			if child.op == predicatePath && !matchesPredicateLocked(n, child, view) {
				return false
			}
		}
		return true
	case predicateOr:
		for _, child := range condition.children {
			if matchesPredicateLocked(n, child, view) {
				return true
			}
		}
		return false
	case predicatePath:
		return matchesPathLocked(n, condition.children, 0, view)
	default:
		panic("graph: invalid predicate")
	}
}

func matchesPathLocked(parent *node, steps []*predicate, index int, view *subgraph) bool {
	for _, child := range selectedChildrenLocked(parent, view) {
		if !matchesPredicateLocked(child, steps[index], view) {
			continue
		}
		if index == len(steps)-1 || matchesPathLocked(child, steps, index+1, view) {
			return true
		}
	}
	return false
}

type predicateResult struct {
	matched   bool
	traversed bool
	endpoints []*node
}

func evaluatePredicateLocked(n *node, condition *predicate, view *subgraph) predicateResult {
	switch condition.op {
	case predicateMatch:
		matched := matchesMatcherLocked(n, condition.matcher)
		return predicateResult{matched: matched, endpoints: nodeIf(n, matched)}
	case predicateAny:
		return predicateResult{matched: true, endpoints: []*node{n}}
	case predicateAnd:
		results := make([]predicateResult, 0, len(condition.children))
		traversed := false
		for _, child := range condition.children {
			result := evaluatePredicateLocked(n, child, view)
			if !result.matched {
				return predicateResult{}
			}
			results = append(results, result)
			traversed = traversed || result.traversed
		}
		if !traversed {
			return predicateResult{matched: true, endpoints: []*node{n}}
		}
		out := make([]*node, 0)
		for _, result := range results {
			if result.traversed {
				out = appendUniqueNodes(out, result.endpoints)
			}
		}
		return predicateResult{matched: true, traversed: true, endpoints: out}
	case predicateOr:
		out := make([]*node, 0)
		matched := false
		traversed := false
		for _, child := range condition.children {
			result := evaluatePredicateLocked(n, child, view)
			if result.matched {
				matched = true
				traversed = traversed || result.traversed
				out = appendUniqueNodes(out, result.endpoints)
			}
		}
		return predicateResult{matched: matched, traversed: traversed, endpoints: out}
	case predicatePath:
		frontier := selectedChildrenLocked(n, view)
		for index, step := range condition.children {
			next := make([]*node, 0, len(frontier))
			seen := make(map[*node]struct{}, len(frontier))
			for _, candidate := range frontier {
				if matchesPredicateLocked(candidate, step, view) {
					if _, duplicate := seen[candidate]; !duplicate {
						seen[candidate] = struct{}{}
						next = append(next, candidate)
					}
				}
			}
			frontier = nil
			for _, candidate := range next {
				frontier = append(frontier, selectedChildrenLocked(candidate, view)...)
			}
			if index == len(condition.children)-1 {
				frontier = next
			}
		}
		return predicateResult{matched: len(frontier) != 0, traversed: true, endpoints: frontier}
	default:
		panic("graph: invalid predicate")
	}
}

func selectedChildrenLocked(n *node, view *subgraph) []*node {
	if view != nil {
		return view.edges[n]
	}
	return orderedChildren(n)
}

func nodeIf(n *node, include bool) []*node {
	if include {
		return []*node{n}
	}
	return nil
}

func appendUniqueNodes(dst, source []*node) []*node {
	seen := make(map[*node]struct{}, len(dst)+len(source))
	for _, n := range dst {
		seen[n] = struct{}{}
	}
	for _, n := range source {
		if _, duplicate := seen[n]; duplicate {
			continue
		}
		seen[n] = struct{}{}
		dst = append(dst, n)
	}
	return dst
}
