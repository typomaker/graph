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
}

type matcher struct {
	typ   reflect.Type
	value any
	any   bool
}

type matchExpression struct {
	matchers []matcher
}

type pathExpression struct {
	steps [][]matcher
}

type pathLevel struct {
	matchers []matcher
	active   map[*node]struct{}
	support  map[*node]uint32
}

type query struct {
	roots    []*node
	matchers []matcher
	path     []pathLevel
	result   *selection
	scope    map[*node]uint32
	root     map[*node]struct{}
}

func init() {
	state.queries = make(map[*query]struct{})
}

// Type returns a matcher meaning "an attribute of type T, with any value".
//
// For example, this query selects every reachable node with a Position
// attribute:
//
//	positions, closePositions := Query(root, Type[Position]())
//	defer closePositions()
func Type[T any]() any {
	t := reflect.TypeOf((*T)(nil)).Elem()
	return typeMatcher{typ: normalizeType(t)}
}

// Match groups attribute matchers that must all match the same node in a Path.
//
// For example, this path step matches an Actor having a specific ID:
//
//	Match(Type[Actor](), ID("actor-1"))
func Match(values ...any) any {
	return matchExpression{matchers: queryMatchers(values)}
}

// Path creates a structural matcher whose steps must be connected by
// immediate outgoing relations. A query returns the nodes matching the first
// step when the entire path matches.
//
// For example, this matches Location nodes connected through Contains nodes
// to a particular Actor:
//
//	Path(
//		Type[Location](),
//		Type[Contains](),
//		Match(Type[Actor](), ID("actor-1")),
//	)
func Path(values ...any) any {
	if len(values) == 0 {
		panic("graph: path requires at least one step")
	}
	steps := make([][]matcher, len(values))
	for i, value := range values {
		if group, ok := value.(matchExpression); ok {
			steps[i] = append([]matcher(nil), group.matchers...)
			continue
		}
		steps[i] = queryMatchers([]any{value})
	}
	return pathExpression{steps: steps}
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

// Get returns the first node when at least one requested attribute matches.
// Pointer arguments receive the stored value when present.
//
// For example, this retrieves a Position attribute and reports whether it was
// present:
//
//	var position Position
//	found := !Empty(Get(player, &position))
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
//
// For example, this updates the first selected node and detects a change:
//
//	changed := !Empty(Set(player, Position{X: 20, Y: 30}))
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
	updateAttributeQueriesLocked(n, changedTypes)
	return singleton(n, g.view)
}

// Unset removes requested attributes from the first node and optionally writes
// old values to pointer arguments.
//
// For example, this removes Position while retaining its previous value:
//
//	var previous Position
//	removed := !Empty(Unset(player, &previous))
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
	updateAttributeQueriesLocked(n, changed)
	return singleton(n, g.view)
}

// At returns all immediate children, or the supplied candidates that are
// immediate children, preserving their argument order.
//
// For example, this selects all children and then filters two candidates:
//
//	children := At(parent)
//	selected := At(parent, secondChild, firstChild)
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
//
// For example, this attaches two children and returns the newly linked nodes:
//
//	linked := Link(parent, firstChild, secondChild)
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
//
// For example, this detaches child and reports whether the relation existed:
//
//	removed := !Empty(Unlink(parent, child))
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
//	count := Len(At(parent))
func Len(g Graph) int { state.RLock(); defer state.RUnlock(); return len(selected(g)) }

// Empty reports whether the selection contains no nodes.
//
// For example:
//
//	if Empty(Get(player, Type[Position]())) {
//		Set(player, Position{})
//	}
func Empty(g Graph) bool { return Len(g) == 0 }

// Merge returns the ordered union of all selections.
//
// For example, duplicates are removed while the first occurrence order is
// retained:
//
//	all := Merge(firstSelection, secondSelection, firstSelection)
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
//
// For example, this keeps nodes present in both selections:
//
//	visiblePlayers := Include(players, visible)
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
//
// For example, this selects players that are not hidden or disconnected:
//
//	activePlayers := Exclude(players, hidden, disconnected)
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
// The returned close function releases the query; callers should always call
// it when they no longer need live updates.
//
// For example, this selection tracks reachable nodes having both Player and
// Position attributes:
//
//	players, closePlayers := Query(root, Type[Player](), Type[Position]())
//	defer closePlayers()
func Query(g Graph, values ...any) (Graph, func()) {
	ms, pathMatchers := queryCriteria(values)
	path := make([]pathLevel, len(pathMatchers))
	for i, matchers := range pathMatchers {
		path[i] = pathLevel{
			matchers: matchers,
			active:   make(map[*node]struct{}),
			support:  make(map[*node]uint32),
		}
	}
	state.Lock()
	q := &query{roots: append([]*node(nil), selected(g)...), matchers: ms, path: path, result: &selection{}, root: make(map[*node]struct{})}
	recomputeQueryLocked(q)
	state.queries[q] = struct{}{}
	state.Unlock()
	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			state.Lock()
			delete(state.queries, q)
			q.result.closed = true
			state.Unlock()
		})
	}
	return Graph{live: q.result}, closeFn
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

// Apply merges changes into g and returns the corresponding roots. Deltas use
// their internal node identity unless matchBy attributes were supplied to
// Delta. An ordinary graph is additively merged by the composite attribute
// key, matching nodes across the entire target graph. For compatibility, a
// patch root without the key attributes corresponds by selection order.
//
// For example, this applies an EntityID-keyed delta to a replica:
//
//	changes := Delta(source, Type[EntityID]())
//	replica = Apply(replica, changes, Type[EntityID]())
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
		panic("graph: apply match attributes differ from delta")
	}

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

func queryCriteria(values []any) ([]matcher, [][]matcher) {
	if len(values) == 1 {
		switch expression := values[0].(type) {
		case matchExpression:
			return append([]matcher(nil), expression.matchers...), nil
		case pathExpression:
			path := make([][]matcher, len(expression.steps))
			for i, step := range expression.steps {
				path[i] = append([]matcher(nil), step...)
			}
			return nil, path
		}
	}
	for _, value := range values {
		switch value.(type) {
		case matchExpression, pathExpression:
			panic("graph: Match and Path must be a single query expression")
		}
	}
	return queryMatchers(values), nil
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

func queryMatchesLocked(q *query, n *node) bool {
	if len(q.path) != 0 {
		_, ok := q.path[0].active[n]
		return ok
	}
	return matchesLocked(n, q.matchers)
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
	buildPathIndexLocked(q, reachable)
	out := make([]*node, 0, len(reachable))
	for _, n := range reachable {
		if queryMatchesLocked(q, n) {
			out = append(out, n)
		}
	}
	q.result.nodes = out
}

func buildPathIndexLocked(q *query, nodes []*node) {
	if len(q.path) == 0 {
		return
	}
	for i := range q.path {
		q.path[i].active = make(map[*node]struct{})
		q.path[i].support = make(map[*node]uint32)
	}
	for step := len(q.path) - 1; step >= 0; step-- {
		level := &q.path[step]
		for _, n := range nodes {
			if step < len(q.path)-1 {
				for child := range n.children {
					if _, ok := q.path[step+1].active[child]; ok {
						level.support[n]++
					}
				}
			}
			if matchesLocked(n, level.matchers) && (step == len(q.path)-1 || level.support[n] != 0) {
				level.active[n] = struct{}{}
			}
		}
	}
}

func updateAttributeQueriesLocked(n *node, changed map[reflect.Type]struct{}) {
	for q := range state.queries {
		if len(q.path) != 0 {
			if q.scope[n] != 0 {
				for step := len(q.path) - 1; step >= 0; step-- {
					if matchersUseTypes(q.path[step].matchers, changed) {
						refreshPathNodeLocked(q, n, step)
					}
				}
			}
			continue
		}
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

func matchersUseTypes(matchers []matcher, changed map[reflect.Type]struct{}) bool {
	for _, matcher := range matchers {
		if _, ok := changed[matcher.typ]; ok {
			return true
		}
	}
	return false
}

func refreshPathNodeLocked(q *query, n *node, step int) {
	level := &q.path[step]
	_, wasActive := level.active[n]
	isActive := matchesLocked(n, level.matchers) && (step == len(q.path)-1 || level.support[n] != 0)
	if wasActive == isActive {
		return
	}
	if isActive {
		level.active[n] = struct{}{}
	} else {
		delete(level.active, n)
	}
	if step == 0 {
		updatePathResultNodeLocked(q, n, isActive)
		return
	}
	for parent := range n.parents {
		if q.scope[parent] == 0 {
			continue
		}
		parentLevel := &q.path[step-1]
		if isActive {
			parentLevel.support[parent]++
		} else {
			parentLevel.support[parent]--
		}
		refreshPathNodeLocked(q, parent, step-1)
	}
}

func updatePathResultNodeLocked(q *query, n *node, active bool) {
	index := -1
	for i, current := range q.result.nodes {
		if current == n {
			index = i
			break
		}
	}
	if active && index < 0 {
		q.result.nodes = append(q.result.nodes, n)
	} else if !active && index >= 0 {
		removeQueryResultNodeLocked(q, n)
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
	matches := queryMatchesLocked(q, n)
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
		if len(q.path) != 0 {
			updatePathStructureLocked(q, parent, child, linked)
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

func updatePathStructureLocked(q *query, parent, child *node, linked bool) {
	if linked {
		if q.scope[child] != 0 {
			q.scope[child]++
			updatePathEdgeLocked(q, parent, child, true)
			return
		}
		added := make(map[*node]struct{})
		addPathScopeLocked(q, child, added)
		initializeAddedPathNodesLocked(q, added)
		return
	}

	updatePathEdgeLocked(q, parent, child, false)
	removed := make(map[*node]struct{})
	removePathScopeLocked(q, child, removed)
	for n := range removed {
		for i := range q.path {
			delete(q.path[i].active, n)
			delete(q.path[i].support, n)
		}
		removeQueryResultNodeLocked(q, n)
	}
}

func updatePathEdgeLocked(q *query, parent, child *node, linked bool) {
	for step := 0; step < len(q.path)-1; step++ {
		if _, active := q.path[step+1].active[child]; !active {
			continue
		}
		if linked {
			q.path[step].support[parent]++
		} else {
			q.path[step].support[parent]--
		}
		refreshPathNodeLocked(q, parent, step)
	}
}

func addPathScopeLocked(q *query, n *node, added map[*node]struct{}) {
	wasUnreachable := q.scope[n] == 0
	q.scope[n]++
	if !wasUnreachable {
		return
	}
	added[n] = struct{}{}
	for _, child := range orderedChildren(n) {
		addPathScopeLocked(q, child, added)
	}
}

func initializeAddedPathNodesLocked(q *query, added map[*node]struct{}) {
	for step := len(q.path) - 1; step >= 0; step-- {
		level := &q.path[step]
		for n := range added {
			if step < len(q.path)-1 {
				for child := range n.children {
					if _, active := q.path[step+1].active[child]; active {
						level.support[n]++
					}
				}
			}
			if matchesLocked(n, level.matchers) && (step == len(q.path)-1 || level.support[n] != 0) {
				level.active[n] = struct{}{}
			}
		}
	}
	for step := len(q.path) - 1; step > 0; step-- {
		for n := range added {
			if _, active := q.path[step].active[n]; !active {
				continue
			}
			for parent := range n.parents {
				if q.scope[parent] == 0 {
					continue
				}
				if _, parentAdded := added[parent]; parentAdded {
					continue
				}
				q.path[step-1].support[parent]++
				refreshPathNodeLocked(q, parent, step-1)
			}
		}
	}
	for n := range added {
		if _, active := q.path[0].active[n]; active {
			updatePathResultNodeLocked(q, n, true)
		}
	}
}

func removePathScopeLocked(q *query, n *node, removed map[*node]struct{}) {
	if q.scope[n] == 0 {
		return
	}
	q.scope[n]--
	if q.scope[n] != 0 {
		return
	}
	delete(q.scope, n)
	removed[n] = struct{}{}
	for _, child := range orderedChildren(n) {
		removePathScopeLocked(q, child, removed)
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
