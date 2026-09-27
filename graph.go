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
	propagateLocked(n, rev)
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
	propagateLocked(n, rev)
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
		parent.children[child] = rev
		delete(parent.removedChildren, child.key)
		child.parents[parent] = struct{}{}
		parent.selfRev = rev
		propagateLocked(parent, rev)
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
		delete(parent.children, child)
		delete(child.parents, parent)
		rev := nextRevisionLocked()
		parent.removedChildren[child.key] = removedChild{revision: rev, attrs: cloneAttrs(child.attrs)}
		parent.selfRev = rev
		propagateLocked(parent, rev)
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
	return selectNodes(g, false, false, values)
}

// Follow filters the currently selected nodes and returns the endpoints of
// matching expressions. Without Path it is equivalent to Select.
func Follow(g Graph, values ...any) Graph {
	return selectNodes(g, false, true, values)
}

// Search recursively filters every node reachable from the current selection
// and returns matching starts.
func Search(g Graph, values ...any) Graph {
	return selectNodes(g, true, false, values)
}

func selectNodes(g Graph, recursive, endpoints bool, values []any) Graph {
	condition := buildPredicate(predicateAnd, values)
	state.RLock()
	defer state.RUnlock()
	candidates := selected(g)
	if recursive {
		if g.view != nil {
			candidates = reachableViewLocked(candidates, g.view)
		} else {
			candidates = reachableLocked(candidates)
		}
	}
	out := make([]*node, 0, len(candidates))
	seen := make(map[*node]struct{})
	for _, n := range candidates {
		result := evaluatePredicateLocked(n, condition, g.view)
		if !result.matched {
			continue
		}
		matches := []*node{n}
		if endpoints {
			matches = result.endpoints
		}
		for _, match := range matches {
			if _, duplicate := seen[match]; duplicate {
				continue
			}
			seen[match] = struct{}{}
			out = append(out, match)
		}
	}
	return Graph{nodes: out}
}

// Commit advances the baseline of every node reachable from the selection to
// the current revision. Changes at or before that revision are omitted from
// subsequent Delta calls through any root that reaches those nodes.
//
// For example, commit after synchronizing a replica so that the next delta
// contains only later changes:
//
//	replica = Apply(replica, Delta(source))
//	Commit(source)
func Commit(g Graph) {
	state.Lock()
	defer state.Unlock()
	for _, n := range reachableLocked(selected(g)) {
		n.baseline = state.revision
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
			pair.target.children[targetChild] = rev
			delete(pair.target.removedChildren, targetChild.key)
			targetChild.parents[pair.target] = struct{}{}
			changed[pair.target] = struct{}{}
		}
	}
	for target := range changed {
		target.selfRev = rev
		propagateLocked(target, rev)
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
func matchesMatcherLocked(n *node, m matcher) bool {
	v, ok := n.attrs[m.typ]
	return ok && (m.any || v == m.value)
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
				if evaluatePredicateLocked(candidate, step, view).matched {
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
