// Package graph implements a small, type-oriented directed acyclic graph.
package graph

import (
	"fmt"
	"iter"
	"math/bits"
	"reflect"
	"slices"
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
	dirtyChildren   map[*node]struct{}
	dirtyChildOrder []*node
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
	byKey             map[uint64]*node
	sameKey           map[*node]*node
	compositeIndexes  []cachedCompositeIndex
	dirty             map[uint32]struct{}
	dirtyOrdinals     posting
	dirtyByType       map[reflect.Type]posting
	dirtyByValue      map[reflect.Type]map[any]posting
	dirtySnapshots    map[uint32]map[reflect.Type]any
	dirtyIndexed      bool
	overlayRevision   uint64
	removed           map[uint32]struct{}
	removedOrdinals   posting
	affected          map[uint32]struct{}
	affectedOrdinals  posting
	added             []*node
	addedOrdinal      map[*node]uint32
	addedAll          posting
	addedByType       map[reflect.Type]posting
	addedByValue      map[reflect.Type]map[any]posting
	addedIndexed      bool
	orderByOrdinal    []uint32
	orderIdentity     bool
	orderOverlayBuilt bool
	support           map[*node]uint32
	active            map[*node]struct{}
	activeByOrdinal   []bool
	activeAdded       int
	orderDirty        bool
	addedEdges        map[graphEdge]struct{}
	removedEdges      map[graphEdge]struct{}
	committedAdded    map[graphEdge]struct{}
	committedRemoved  map[graphEdge]struct{}
}

const addedPostingThreshold = 32
const dirtyPostingThreshold = 32

type posting []uint32

type graphEdge struct {
	parent *node
	child  *node
}

type structuralChange struct {
	parent *node
	child  *node
	linked bool
}

type cachedCompositeIndex struct {
	types []reflect.Type
	index *compositeIndex
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

const pooledVisitedLimit = 65536

var visitedNodeSets sync.Pool

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
		markDirtyChildLocked(parent, child)
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

// Query returns a reusable materialized iterator over the nodes matching
// Search. It evaluates the search immediately, then refreshes the result lazily
// at the beginning of an iteration when the reachable graph has changed.
//
// For example, this query always iterates over the currently visible nodes:
//
//	visible, close := Query(world, Visible(true))
//	defer close()
//	for node := range visible {
//		// ...
//	}
func Query(g Graph, values ...any) (iter.Seq[Graph], func()) {
	query := newReactiveQuery(g, values)
	sequence := func(yield func(Graph) bool) {
		result := query.snapshot()
		for node := range Each(result) {
			if !yield(node) {
				return
			}
		}
	}
	return sequence, query.close
}

type reactiveQuery struct {
	sync.Mutex
	source      Graph
	conditions  []any
	result      Graph
	revisions   []uint64
	searchCount uint64
	closed      bool
}

func newReactiveQuery(g Graph, values []any) *reactiveQuery {
	conditions := append([]any(nil), values...)
	query := &reactiveQuery{source: g, conditions: conditions}
	query.Lock()
	query.refreshLocked()
	query.Unlock()
	return query
}

func (query *reactiveQuery) snapshot() Graph {
	query.Lock()
	defer query.Unlock()
	if query.closed {
		return Graph{}
	}
	if revisions := graphRevisions(query.source); !slices.Equal(query.revisions, revisions) {
		query.refreshLocked()
	}
	return Graph{
		nodes: append([]*node(nil), query.result.nodes...),
		view:  query.result.view,
		index: query.result.index,
	}
}

func (query *reactiveQuery) refreshLocked() {
	for {
		before := graphRevisions(query.source)
		query.searchCount++
		result := Search(query.source, query.conditions...)
		after := graphRevisions(query.source)
		if slices.Equal(before, after) {
			query.result = result
			query.revisions = after
			return
		}
	}
}

func (query *reactiveQuery) close() {
	query.Lock()
	defer query.Unlock()
	if query.closed {
		return
	}
	query.source = Graph{}
	query.conditions = nil
	query.result = Graph{}
	query.revisions = nil
	query.closed = true
}

func graphRevisions(g Graph) []uint64 {
	state.RLock()
	defer state.RUnlock()
	revisions := make([]uint64, len(g.nodes))
	for i, root := range g.nodes {
		revisions[i] = root.treeRev
	}
	return revisions
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
		index := roots[0].index
		if out, streamed := index.streamCurrentMatches(condition); streamed {
			return Graph{nodes: out, index: index}
		}
		if out, streamed := index.streamCleanOrMatches(condition); streamed {
			return Graph{nodes: out, index: index}
		}
		matches := index.orderMatches(index.matchCurrent(condition))
		out := make([]*node, len(matches))
		for i, ordinal := range matches {
			out[i] = index.nodeAt(ordinal)
		}
		return Graph{nodes: out, index: index}
	}
	visited, releaseVisited := acquireVisitedNodeSet()
	defer releaseVisited()
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

func (index *searchIndex) streamCleanOrMatches(condition *predicate) ([]*node, bool) {
	if condition.op != predicateOr || len(condition.children) != 2 || len(index.dirty) != 0 ||
		len(index.addedEdges) != 0 || len(index.removedEdges) != 0 || len(index.removed) != 0 ||
		index.hasActiveAddedNodes() || !index.orderIdentity {
		return nil, false
	}
	operands := make([]posting, len(condition.children))
	total := 0
	for i, child := range condition.children {
		operands[i] = index.match(child)
		total += len(operands[i])
	}
	result := make([]*node, 0, total)
	left, right := operands[0], operands[1]
	for i, j := 0, 0; i < len(left) || j < len(right); {
		switch {
		case j == len(right) || i < len(left) && left[i] < right[j]:
			result = append(result, index.nodeAt(left[i]))
			i++
		case i == len(left) || right[j] < left[i]:
			result = append(result, index.nodeAt(right[j]))
			j++
		default:
			result = append(result, index.nodeAt(left[i]))
			i++
			j++
		}
	}
	return result, true
}

func acquireVisitedNodeSet() (map[*node]struct{}, func()) {
	visited, _ := visitedNodeSets.Get().(map[*node]struct{})
	if visited == nil {
		visited = make(map[*node]struct{})
	}
	return visited, func() {
		size := len(visited)
		clear(visited)
		if size <= pooledVisitedLimit {
			visitedNodeSets.Put(visited)
		}
	}
}

func (index *searchIndex) streamCurrentMatches(condition *predicate) ([]*node, bool) {
	if !index.orderIdentity || containsPath(condition) || len(index.dirty) == 0 && len(index.removed) == 0 && index.activeAdded == 0 {
		return nil, false
	}
	base := index.match(condition)
	excluded := unionPostings(index.dirtyOrdinals, index.removedOrdinals)
	replacements := index.activePosting(index.matchDirty(condition))
	if index.addedIndexed {
		added := index.matchAdded(condition)
		if len(base) == 0 && len(excluded) == 0 && len(replacements) == 0 {
			result := make([]*node, 0, len(added))
			for _, ordinal := range added {
				if index.activeByOrdinal[ordinal] {
					result = append(result, index.nodeAt(ordinal))
				}
			}
			return result, true
		}
		replacements = index.appendActiveAdded(replacements, added)
	} else {
		for offset, n := range index.added {
			ordinal := uint32(len(index.nodes) + offset)
			if index.activeByOrdinal[ordinal] && matchesPredicateLocked(n, condition, nil) {
				replacements = append(replacements, ordinal)
			}
		}
	}
	if len(excluded) == 0 && len(replacements) == 0 {
		result := make([]*node, len(base))
		for i, ordinal := range base {
			result[i] = index.nodeAt(ordinal)
		}
		return result, true
	}
	result := make([]*node, 0, len(base)+len(replacements))
	for i, removed, replacement := 0, 0, 0; i < len(base) || replacement < len(replacements); {
		for removed < len(excluded) && i < len(base) && excluded[removed] < base[i] {
			removed++
		}
		for i < len(base) && removed < len(excluded) && base[i] == excluded[removed] {
			i++
			removed++
		}
		if replacement < len(replacements) && (i == len(base) || replacements[replacement] < base[i]) {
			result = append(result, index.nodeAt(replacements[replacement]))
			replacement++
			continue
		}
		if i < len(base) {
			result = append(result, index.nodeAt(base[i]))
			i++
		}
	}
	return result, true
}

func searchWhileRebuildingOrder(g Graph, condition *predicate) (Graph, bool) {
	state.Lock()
	defer state.Unlock()
	roots := selected(g)
	if len(roots) != 1 || g.view != nil || !validSearchIndex(roots[0]) || !roots[0].index.orderDirty {
		return Graph{}, false
	}
	index := roots[0].index
	index.orderByOrdinal = make([]uint32, len(index.activeByOrdinal))
	index.orderIdentity = true
	seen := make(map[*node]struct{}, len(index.active))
	out := make([]*node, 0)
	var previousOrdinal uint32
	havePreviousOrdinal := false
	var visit func(*node)
	visit = func(n *node) {
		if _, duplicate := seen[n]; duplicate {
			return
		}
		seen[n] = struct{}{}
		position := uint32(len(seen) - 1)
		ordinal, exists := index.nodeOrdinal(n)
		if exists {
			index.orderByOrdinal[ordinal] = position
			if havePreviousOrdinal && ordinal <= previousOrdinal {
				index.orderIdentity = false
			}
			previousOrdinal = ordinal
			havePreviousOrdinal = true
		}
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

// Commit advances changed paths from each selected root to the current
// revision. The first commit builds a search index; later commits retain and
// update it incrementally unless accumulated fragmentation requires compaction.
// Changes at or before the baseline are omitted from subsequent Delta calls
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
		if !validSearchIndex(root) {
			nodes := reachableLocked([]*node{root})
			for _, n := range nodes {
				n.baseline = state.revision
				clearDirtyChildrenLocked(n)
			}
			root.index = buildSearchIndexLocked(root, nodes)
			continue
		}
		if root.treeRev <= root.baseline {
			continue
		}
		commitChangedLocked(root, state.revision, false, make(map[*node]struct{}))
		root.index.commitOverlayLocked()
		if root.index.shouldCompact() {
			nodes := reachableLocked([]*node{root})
			root.index = buildSearchIndexLocked(root, nodes)
		}
	}
}

func clearDirtyChildrenLocked(n *node) {
	clear(n.dirtyChildren)
	n.dirtyChildOrder = n.dirtyChildOrder[:0]
}

func commitChangedLocked(n *node, revision uint64, full bool, seen map[*node]struct{}) {
	if _, duplicate := seen[n]; duplicate {
		return
	}
	seen[n] = struct{}{}
	base := n.baseline
	n.baseline = revision
	children := n.dirtyChildOrder
	if full {
		children = n.childOrder
	}
	for _, child := range children {
		if _, linked := n.children[child]; !linked {
			continue
		}
		edgeAdded := n.children[child] > base
		if full || edgeAdded {
			commitChangedLocked(child, revision, true, seen)
		} else if child.treeRev > child.baseline {
			commitChangedLocked(child, revision, false, seen)
		}
	}
	clearDirtyChildrenLocked(n)
}

// Delta returns the registered changed paths from each selected root since its
// preceding Commit. It does not advance the baseline, so repeated calls return
// the same changes. Optional matchBy attributes define the stable composite
// identity Apply must use. A new edge to a committed node includes that node's
// identity without copying its unchanged descendants.
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
		if buildDeltaLocked(root, view, make(map[*node]bool), false) {
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
	var targetIndex *searchIndex
	if roots := selected(g); len(roots) == 1 && validSearchIndex(roots[0]) {
		targetIndex = roots[0].index
	}
	var targetScope []*node
	if targetIndex == nil {
		targetScope = reachableLocked(selected(g))
	}
	var keyedTargets *compositeIndex
	if len(keyTypes) == 0 {
		if targetIndex == nil {
			for _, n := range targetScope {
				if _, duplicate := targets[n.key]; duplicate {
					panic("graph: replica contains duplicate node identity")
				}
				targets[n.key] = n
			}
		}
	} else {
		if targetIndex != nil {
			keyedTargets = targetIndex.compositeIndexLocked(keyTypes)
		} else {
			keyedTargets = newCompositeIndex(keyTypes, targetScope)
		}
	}
	deltaNodes := reachableViewLocked(selected(delta), delta.view)
	matches := make(map[*node]*node, len(deltaNodes))
	rev := nextRevisionLocked()
	created := make(map[*node]bool)
	findTarget := func(key uint64) *node {
		if target := targets[key]; target != nil {
			return target
		}
		if targetIndex != nil {
			return targetIndex.activeNodeByKey(key)
		}
		return nil
	}
	for _, source := range deltaNodes {
		var target *node
		if len(keyTypes) == 0 {
			target = findTarget(source.key)
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

	attributeChanged := make(map[*node]struct{})
	structureChanged := make(map[*node]struct{})
	structuralIndexes := make(map[*searchIndex]struct{})
	structuralChanges := make([]structuralChange, 0)
	for _, source := range deltaNodes {
		target := matches[source]
		change := delta.view.changes[source]
		if !created[target] {
			for typ, value := range change.attrs {
				if old, ok := target.attrs[typ]; !ok || old != value {
					target.attrs[typ] = value
					target.attrRev[typ] = rev
					delete(target.removedAttrs, typ)
					attributeChanged[target] = struct{}{}
				}
			}
			for _, typ := range change.removedAttrs {
				if _, ok := target.attrs[typ]; ok {
					delete(target.attrs, typ)
					delete(target.attrRev, typ)
					target.removedAttrs[typ] = rev
					attributeChanged[target] = struct{}{}
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
				child = findTarget(removed.key)
			} else {
				child = keyedTargets.find(removed.attrs)
			}
			if child == nil {
				continue
			}
			if _, ok := parent.children[child]; ok {
				collectStructuralIndexesLocked(parent, structuralIndexes)
				removeChildLocked(parent, child)
				delete(child.parents, parent)
				parent.removedChildren[child.key] = removedChild{revision: rev, attrs: cloneAttrs(child.attrs)}
				structureChanged[parent] = struct{}{}
				structuralChanges = append(structuralChanges, structuralChange{parent: parent, child: child})
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
				collectStructuralIndexesLocked(parent, structuralIndexes)
				addChildLocked(parent, child, rev)
				delete(parent.removedChildren, child.key)
				child.parents[parent] = struct{}{}
				markDirtyChildLocked(parent, child)
				structureChanged[parent] = struct{}{}
				structuralChanges = append(structuralChanges, structuralChange{parent: parent, child: child, linked: true})
			}
		}
	}
	for target := range attributeChanged {
		target.selfRev = rev
		propagateAttributeLocked(target, rev)
	}
	for target := range structureChanged {
		target.selfRev = rev
		propagateStructureLocked(target, rev)
	}
	for target := range created {
		target.selfRev = rev
	}
	for index := range structuralIndexes {
		for i := len(structuralChanges) - 1; i >= 0; i-- {
			change := structuralChanges[i]
			index.updateStructureLocked(change.parent, change.child, change.linked)
		}
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
	var targets *compositeIndex
	if len(targetRoots) == 1 && validSearchIndex(targetRoots[0]) {
		targets = targetRoots[0].index.compositeIndexLocked(keyTypes)
	} else {
		targets = newCompositeIndex(keyTypes, reachableLocked(targetRoots))
	}
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

	attributeChanged := make(map[*node]struct{})
	structureChanged := make(map[*node]struct{})
	structuralIndexes := make(map[*searchIndex]struct{})
	structuralChanges := make([]structuralChange, 0)
	for _, pair := range pairs {
		if _, isNew := created[pair.target]; isNew {
			continue
		}
		for typ, value := range pair.source.attrs {
			if old, ok := pair.target.attrs[typ]; ok && old == value {
				continue
			}
			pair.target.attrs[typ] = value
			pair.target.attrRev[typ] = rev
			delete(pair.target.removedAttrs, typ)
			attributeChanged[pair.target] = struct{}{}
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
			collectStructuralIndexesLocked(pair.target, structuralIndexes)
			addChildLocked(pair.target, targetChild, rev)
			delete(pair.target.removedChildren, targetChild.key)
			targetChild.parents[pair.target] = struct{}{}
			markDirtyChildLocked(pair.target, targetChild)
			structureChanged[pair.target] = struct{}{}
			structuralChanges = append(structuralChanges, structuralChange{parent: pair.target, child: targetChild, linked: true})
		}
	}
	for target := range attributeChanged {
		target.selfRev = rev
		propagateAttributeLocked(target, rev)
	}
	for target := range structureChanged {
		target.selfRev = rev
		propagateStructureLocked(target, rev)
	}
	for target := range created {
		target.selfRev = rev
	}
	for index := range structuralIndexes {
		for i := len(structuralChanges) - 1; i >= 0; i-- {
			change := structuralChanges[i]
			index.updateStructureLocked(change.parent, change.child, change.linked)
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

func buildDeltaLocked(n *node, view *subgraph, memo map[*node]bool, full bool) (include bool) {
	if !full {
		if result, exists := memo[n]; exists {
			return result
		}
		defer func() { memo[n] = include }()
	}
	base := n.baseline
	if full {
		base = 0
	}
	include = n.selfRev > base
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
	children := n.dirtyChildOrder
	if full {
		children = orderedChildren(n)
	}
	for _, c := range children {
		if !full {
			if _, dirty := n.dirtyChildren[c]; !dirty {
				continue
			}
			if _, linked := n.children[c]; !linked {
				continue
			}
		}
		edgeRev := n.children[c]
		if edgeRev > base {
			view.edges[n] = append(view.edges[n], c)
			change.addedChildren[c.key] = struct{}{}
			if c.baseline == 0 {
				buildDeltaLocked(c, view, memo, true)
			} else {
				buildDeltaLocked(c, view, memo, false)
				if _, exists := view.changes[c]; !exists {
					view.changes[c] = nodeChange{
						attrs:         make(map[reflect.Type]any),
						addedChildren: make(map[uint64]struct{}),
						snapshot:      cloneAttrs(c.attrs),
					}
				}
			}
			include = true
			continue
		}
		if c.treeRev > c.baseline {
			if buildDeltaLocked(c, view, memo, false) {
				view.edges[n] = append(view.edges[n], c)
				include = true
			} else if !full {
				delete(n.dirtyChildren, c)
			}
		} else if !full {
			delete(n.dirtyChildren, c)
		}
	}
	if !full && len(n.dirtyChildOrder) > len(n.dirtyChildren)*2 {
		write := 0
		for _, child := range n.dirtyChildOrder {
			if _, dirty := n.dirtyChildren[child]; dirty {
				n.dirtyChildOrder[write] = child
				write++
			}
		}
		n.dirtyChildOrder = n.dirtyChildOrder[:write]
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
		dirtyChildren: make(map[*node]struct{}),
		selfRev:       rev, treeRev: rev, structureRev: rev,
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

func markDirtyChildLocked(parent, child *node) {
	if _, exists := parent.dirtyChildren[child]; exists {
		return
	}
	parent.dirtyChildren[child] = struct{}{}
	parent.dirtyChildOrder = append(parent.dirtyChildOrder, child)
}

func propagateAttributeLocked(n *node, rev uint64) {
	seen := map[*node]struct{}{}
	var visit func(*node, *node)
	visit = func(x, changedChild *node) {
		if changedChild != nil {
			markDirtyChildLocked(x, changedChild)
		}
		if _, ok := seen[x]; ok {
			return
		}
		seen[x] = struct{}{}
		if x.treeRev < rev {
			x.treeRev = rev
		}
		if x.index != nil {
			x.index.invalidateCompositeIndexesLocked(n, rev)
			if ordinal, exists := x.index.ordinal[n]; exists {
				if x.index.attributesCommitted(n, ordinal) {
					delete(x.index.dirty, ordinal)
					if x.index.dirtyIndexed {
						x.index.updateDirtyPostingsLocked(ordinal, nil)
					}
				} else {
					x.index.dirty[ordinal] = struct{}{}
					if x.index.dirtyIndexed {
						x.index.updateDirtyPostingsLocked(ordinal, n.attrs)
					} else if len(x.index.dirty) >= dirtyPostingThreshold {
						x.index.rebuildDirtyPostingsLocked()
					}
				}
			} else if _, exists := x.index.addedOrdinal[n]; exists && x.index.addedIndexed {
				x.index.rebuildAddedPostingsLocked()
			}
			x.index.dirtyOrdinals = postingFromSet(x.index.dirtyOrdinals, x.index.dirty)
		}
		for p := range x.parents {
			visit(p, x)
		}
	}
	visit(n, nil)
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

func (index *searchIndex) updateDirtyPostingsLocked(ordinal uint32, attrs map[reflect.Type]any) {
	previous := index.dirtySnapshots[ordinal]
	if previous != nil {
		for typ, value := range previous {
			index.dirtyByType[typ] = removePostingOrdinal(index.dirtyByType[typ], ordinal)
			values := index.dirtyByValue[typ]
			if values != nil {
				values[value] = removePostingOrdinal(values[value], ordinal)
				if len(values[value]) == 0 {
					delete(values, value)
				}
			}
		}
		clear(previous)
	}
	if attrs == nil {
		return
	}
	if previous == nil {
		previous = make(map[reflect.Type]any, len(attrs))
		index.dirtySnapshots[ordinal] = previous
	}
	for typ, value := range attrs {
		index.dirtyByType[typ] = insertPostingOrdinal(index.dirtyByType[typ], ordinal)
		if index.byValue[typ] != nil {
			previous[typ] = value
			values := index.dirtyByValue[typ]
			if values == nil {
				values = make(map[any]posting)
				index.dirtyByValue[typ] = values
			}
			values[value] = insertPostingOrdinal(values[value], ordinal)
		} else {
			previous[typ] = nil
		}
	}
}

func (index *searchIndex) rebuildDirtyPostingsLocked() {
	for ordinal := range index.dirty {
		index.updateDirtyPostingsLocked(ordinal, index.nodes[ordinal].attrs)
	}
	index.dirtyIndexed = true
}

func insertPostingOrdinal(values posting, ordinal uint32) posting {
	position, found := slices.BinarySearch(values, ordinal)
	if found {
		return values
	}
	values = append(values, 0)
	copy(values[position+1:], values[position:])
	values[position] = ordinal
	return values
}

func removePostingOrdinal(values posting, ordinal uint32) posting {
	position, found := slices.BinarySearch(values, ordinal)
	if !found {
		return values
	}
	copy(values[position:], values[position+1:])
	return values[:len(values)-1]
}

func postingHas(values posting, ordinal uint32) bool {
	position := sort.Search(len(values), func(i int) bool { return values[i] >= ordinal })
	return position < len(values) && values[position] == ordinal
}

func propagateStructureLocked(n *node, rev uint64) {
	seen := map[*node]struct{}{}
	var visit func(*node, *node)
	visit = func(x, changedChild *node) {
		if changedChild != nil {
			markDirtyChildLocked(x, changedChild)
		}
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
			visit(parent, x)
		}
	}
	visit(n, nil)
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

func collectStructuralIndexesLocked(n *node, indexes map[*searchIndex]struct{}) {
	seen := make(map[*node]struct{})
	var visit func(*node)
	visit = func(current *node) {
		if _, duplicate := seen[current]; duplicate {
			return
		}
		seen[current] = struct{}{}
		if current.index != nil {
			indexes[current.index] = struct{}{}
		}
		for parent := range current.parents {
			visit(parent)
		}
	}
	visit(n)
}

func (index *searchIndex) updateStructureLocked(parent, child *node, linked bool) {
	index.compositeIndexes = nil
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
	index.removedOrdinals = postingFromSet(index.removedOrdinals, index.removed)
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

func (index *searchIndex) commitOverlayLocked() {
	for _, parent := range index.added {
		parentOrdinal := index.addedOrdinal[parent]
		if !index.activeByOrdinal[parentOrdinal] {
			continue
		}
		for _, child := range parent.childOrder {
			childOrdinal, known := index.nodeOrdinal(child)
			if !known || !index.activeByOrdinal[childOrdinal] || index.hasOriginalEdge(parent, child) {
				continue
			}
			edge := graphEdge{parent: parent, child: child}
			index.committedAdded[edge] = struct{}{}
			delete(index.committedRemoved, edge)
		}
	}
	for edge := range index.addedEdges {
		if index.hasOriginalEdge(edge.parent, edge.child) {
			delete(index.committedAdded, edge)
			delete(index.committedRemoved, edge)
		} else {
			index.committedAdded[edge] = struct{}{}
			delete(index.committedRemoved, edge)
		}
	}
	for edge := range index.removedEdges {
		if index.hasOriginalEdge(edge.parent, edge.child) {
			index.committedRemoved[edge] = struct{}{}
			delete(index.committedAdded, edge)
		} else {
			delete(index.committedAdded, edge)
			delete(index.committedRemoved, edge)
		}
	}
	clear(index.addedEdges)
	clear(index.removedEdges)
	clear(index.affected)
	index.affectedOrdinals = index.affectedOrdinals[:0]
	index.structureRevision = index.root.structureRev
	index.overlayRevision = index.root.structureRev
}

func (index *searchIndex) shouldCompact() bool {
	total := len(index.nodes) + len(index.added)
	overlay := len(index.dirty) + len(index.removed) + len(index.added) + len(index.committedAdded) + len(index.committedRemoved)
	return overlay > 256 && overlay*4 > total
}

func (index *searchIndex) hasCommittedEdge(parent, child *node) bool {
	edge := graphEdge{parent: parent, child: child}
	if _, removed := index.committedRemoved[edge]; removed {
		return false
	}
	if _, added := index.committedAdded[edge]; added {
		return true
	}
	return index.hasOriginalEdge(parent, child)
}

func (index *searchIndex) hasOriginalEdge(parent, child *node) bool {
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
	index.affectedOrdinals = postingFromSet(index.affectedOrdinals, index.affected)
}

func (index *searchIndex) hasActiveAddedNodes() bool {
	return index.activeAdded != 0
}

func (index *searchIndex) resetCommittedOrderLocked() {
	if index.orderOverlayBuilt {
		for ordinal := range index.nodes {
			index.orderByOrdinal[ordinal] = uint32(ordinal)
		}
	}
	index.orderIdentity = true
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
		index.activeByOrdinal[ordinal] = true
	} else {
		ordinal, known := index.addedOrdinal[n]
		if !known {
			ordinal = uint32(len(index.nodes) + len(index.added))
			index.addedOrdinal[n] = ordinal
			index.added = append(index.added, n)
			index.addedAll = append(index.addedAll, ordinal)
			index.activeByOrdinal = append(index.activeByOrdinal, false)
			index.orderByOrdinal = append(index.orderByOrdinal, 0)
			index.addKeyNode(n)
			if index.addedIndexed {
				index.indexAddedNodeLocked(n, ordinal)
			} else if len(index.added) > addedPostingThreshold {
				index.rebuildAddedPostingsLocked()
			}
		}
		index.activeByOrdinal[ordinal] = true
		index.activeAdded++
	}
	for _, child := range n.childOrder {
		wasInactive := index.support[child] == 0
		index.support[child]++
		if wasInactive {
			index.activateLocked(child)
		}
	}
}

func (index *searchIndex) rebuildAddedPostingsLocked() {
	index.addedByType = make(map[reflect.Type]posting)
	index.addedByValue = make(map[reflect.Type]map[any]posting)
	index.addedIndexed = true
	for offset, n := range index.added {
		index.indexAddedNodeLocked(n, uint32(len(index.nodes)+offset))
	}
}

func (index *searchIndex) indexAddedNodeLocked(n *node, ordinal uint32) {
	for typ, value := range n.attrs {
		index.addedByType[typ] = append(index.addedByType[typ], ordinal)
		values := index.addedByValue[typ]
		if values == nil {
			values = make(map[any]posting)
			index.addedByValue[typ] = values
		}
		values[value] = append(values[value], ordinal)
	}
}

func (index *searchIndex) addKeyNode(n *node) {
	if previous := index.byKey[n.key]; previous != nil {
		index.sameKey[n] = previous
	}
	index.byKey[n.key] = n
}

func (index *searchIndex) activeNodeByKey(key uint64) *node {
	var match *node
	for n := index.byKey[key]; n != nil; n = index.sameKey[n] {
		ordinal, exists := index.nodeOrdinal(n)
		if !exists || !index.activeByOrdinal[ordinal] {
			continue
		}
		if match != nil && match != n {
			panic("graph: replica contains duplicate node identity")
		}
		match = n
	}
	return match
}

func (index *searchIndex) compositeIndexLocked(types []reflect.Type) *compositeIndex {
	for _, cached := range index.compositeIndexes {
		if sameTypes(cached.types, types) {
			return cached.index
		}
	}
	result := &compositeIndex{types: append([]reflect.Type(nil), types...)}
	for n := range index.active {
		result.add(n)
	}
	index.compositeIndexes = append(index.compositeIndexes, cachedCompositeIndex{types: result.types, index: result})
	return result
}

func sameTypes(left, right []reflect.Type) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (index *searchIndex) invalidateCompositeIndexesLocked(n *node, rev uint64) {
	kept := index.compositeIndexes[:0]
	for _, cached := range index.compositeIndexes {
		invalid := false
		for _, typ := range cached.types {
			if n.attrRev[typ] == rev || n.removedAttrs[typ] == rev {
				invalid = true
				break
			}
		}
		if !invalid {
			kept = append(kept, cached)
		}
	}
	index.compositeIndexes = kept
}

func (index *searchIndex) deactivateLocked(n *node) {
	if _, active := index.active[n]; !active {
		return
	}
	delete(index.active, n)
	if ordinal, committed := index.ordinal[n]; committed {
		index.removed[ordinal] = struct{}{}
		index.activeByOrdinal[ordinal] = false
	} else if ordinal, known := index.addedOrdinal[n]; known {
		index.activeByOrdinal[ordinal] = false
		index.activeAdded--
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
	index.orderByOrdinal = make([]uint32, len(index.activeByOrdinal))
	index.orderIdentity = true
	var previousOrdinal uint32
	havePreviousOrdinal := false
	for position, n := range current {
		order := uint32(position)
		ordinal, exists := index.nodeOrdinal(n)
		if exists {
			index.orderByOrdinal[ordinal] = order
			if havePreviousOrdinal && ordinal <= previousOrdinal {
				index.orderIdentity = false
			}
			previousOrdinal = ordinal
			havePreviousOrdinal = true
		}
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
		byKey:             make(map[uint64]*node, len(nodes)),
		sameKey:           make(map[*node]*node),
		dirty:             make(map[uint32]struct{}),
		dirtyByType:       make(map[reflect.Type]posting),
		dirtyByValue:      make(map[reflect.Type]map[any]posting),
		dirtySnapshots:    make(map[uint32]map[reflect.Type]any),
		overlayRevision:   root.structureRev,
		removed:           make(map[uint32]struct{}),
		affected:          make(map[uint32]struct{}),
		addedOrdinal:      make(map[*node]uint32),
		orderByOrdinal:    make([]uint32, len(nodes)),
		orderIdentity:     true,
		support:           make(map[*node]uint32, len(nodes)),
		active:            make(map[*node]struct{}, len(nodes)),
		activeByOrdinal:   make([]bool, len(nodes)),
		addedEdges:        make(map[graphEdge]struct{}),
		removedEdges:      make(map[graphEdge]struct{}),
		committedAdded:    make(map[graphEdge]struct{}),
		committedRemoved:  make(map[graphEdge]struct{}),
	}
	for ordinal, n := range nodes {
		if uint64(ordinal) > uint64(^uint32(0)) {
			panic("graph: search index exceeds uint32 capacity")
		}
		id := uint32(ordinal)
		index.ordinal[n] = id
		index.all[ordinal] = id
		index.orderByOrdinal[ordinal] = id
		index.active[n] = struct{}{}
		index.activeByOrdinal[ordinal] = true
		index.addKeyNode(n)
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
		slices.Sort(parents)
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
			current = index.parentPostingMatching(current, index.match(condition.children[step]))
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
	if path {
		if structural {
			return index.matchOverlay(condition)
		}
		affected := index.dirtyPosting(true)
		current := make(posting, 0, len(affected))
		for _, ordinal := range affected {
			if matchesPredicateLocked(index.nodes[ordinal], condition, nil) {
				current = append(current, ordinal)
			}
		}
		return mergePostingOverlay(base, affected, current)
	}
	affected := index.dirtyPosting(path)
	removed := index.removedOrdinals
	excluded := unionPostings(affected, removed)
	current := index.activePosting(index.matchDirty(condition))
	if index.addedIndexed && !path {
		current = index.appendActiveAdded(current, index.matchAdded(condition))
	} else {
		for offset, n := range index.added {
			ordinal := uint32(len(index.nodes) + offset)
			if index.activeByOrdinal[ordinal] && matchesPredicateLocked(n, condition, nil) {
				current = append(current, ordinal)
			}
		}
	}
	return mergePostingOverlay(base, excluded, current)
}

func (index *searchIndex) matchOverlay(condition *predicate) posting {
	switch condition.op {
	case predicateMatch, predicateAny:
		return index.matchAtomicOverlay(condition)
	case predicateAnd:
		operands := make([]posting, len(condition.children))
		for i, child := range condition.children {
			operands[i] = index.matchOverlay(child)
			if len(operands[i]) == 0 {
				return nil
			}
		}
		sort.Slice(operands, func(i, j int) bool { return len(operands[i]) < len(operands[j]) })
		result := operands[0]
		for _, operand := range operands[1:] {
			result = intersectPostings(result, operand)
		}
		return result
	case predicateOr:
		var result posting
		for _, child := range condition.children {
			result = unionPostings(result, index.matchOverlay(child))
		}
		return result
	case predicatePath:
		current := index.matchOverlay(condition.children[len(condition.children)-1])
		for step := len(condition.children) - 2; step >= 0 && len(current) != 0; step-- {
			current = index.currentParentPostingMatching(current, index.matchOverlay(condition.children[step]))
		}
		return index.currentParentPostingMatching(current, nil)
	default:
		panic("graph: invalid predicate")
	}
}

func (index *searchIndex) matchDirty(condition *predicate) posting {
	if !index.dirtyIndexed {
		result := make(posting, 0, len(index.dirtyOrdinals))
		for _, ordinal := range index.dirtyOrdinals {
			if matchesPredicateLocked(index.nodes[ordinal], condition, nil) {
				result = append(result, ordinal)
			}
		}
		return result
	}
	switch condition.op {
	case predicateMatch:
		if condition.matcher.any {
			return index.dirtyByType[condition.matcher.typ]
		}
		if index.byValue[condition.matcher.typ] == nil {
			result := make(posting, 0)
			for _, ordinal := range index.dirtyOrdinals {
				if matchesMatcherLocked(index.nodes[ordinal], condition.matcher) {
					result = append(result, ordinal)
				}
			}
			return result
		}
		return index.dirtyByValue[condition.matcher.typ][condition.matcher.value]
	case predicateAny:
		return index.dirtyOrdinals
	case predicateAnd:
		result := index.dirtyOrdinals
		for _, child := range condition.children {
			result = intersectPostings(result, index.matchDirty(child))
			if len(result) == 0 {
				return nil
			}
		}
		return result
	case predicateOr:
		var result posting
		for _, child := range condition.children {
			result = unionPostings(result, index.matchDirty(child))
		}
		return result
	default:
		return nil
	}
}

func (index *searchIndex) activePosting(values posting) posting {
	result := make(posting, 0, len(values))
	for _, ordinal := range values {
		if index.activeByOrdinal[ordinal] {
			result = append(result, ordinal)
		}
	}
	return result
}

func (index *searchIndex) matchAtomicOverlay(condition *predicate) posting {
	base := index.match(condition)
	dirty := index.dirtyOrdinals
	removed := index.removedOrdinals
	excluded := unionPostings(dirty, removed)
	replacements := index.activePosting(index.matchDirty(condition))
	if index.addedIndexed {
		replacements = index.appendActiveAdded(replacements, index.matchAdded(condition))
	} else {
		for offset, n := range index.added {
			ordinal := uint32(len(index.nodes) + offset)
			if index.activeByOrdinal[ordinal] && matchesPredicateLocked(n, condition, nil) {
				replacements = append(replacements, ordinal)
			}
		}
	}
	return mergePostingOverlay(base, excluded, replacements)
}

func (index *searchIndex) appendActiveAdded(result, values posting) posting {
	if len(result) == 0 && index.activeAdded == len(index.added) {
		return values
	}
	if available := cap(result) - len(result); available < len(values) {
		grown := make(posting, len(result), len(result)+len(values))
		copy(grown, result)
		result = grown
	}
	for _, ordinal := range values {
		if index.activeByOrdinal[ordinal] {
			result = append(result, ordinal)
		}
	}
	return result
}

func (index *searchIndex) matchAdded(condition *predicate) posting {
	switch condition.op {
	case predicateMatch:
		if condition.matcher.any {
			return index.addedByType[condition.matcher.typ]
		}
		return index.addedByValue[condition.matcher.typ][condition.matcher.value]
	case predicateAny:
		return index.addedAll
	case predicateAnd:
		operands := make([]posting, len(condition.children))
		for i, child := range condition.children {
			operands[i] = index.matchAdded(child)
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
			operands[i] = index.matchAdded(child)
		}
		sort.Slice(operands, func(i, j int) bool { return len(operands[i]) < len(operands[j]) })
		var result posting
		for _, operand := range operands {
			result = unionPostings(result, operand)
		}
		return result
	case predicatePath:
		panic("graph: added path matching requires traversal")
	default:
		panic("graph: invalid predicate")
	}
}

func postingFromSet(result posting, values map[uint32]struct{}) posting {
	result = result[:0]
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func (index *searchIndex) dirtyPosting(includeAncestors bool) posting {
	if !includeAncestors {
		return index.dirtyOrdinals
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
	ordinal, exists := index.nodeOrdinal(n)
	if !exists {
		return false
	}
	position := sort.Search(len(values), func(i int) bool { return values[i] >= ordinal })
	return position < len(values) && values[position] == ordinal
}

func (index *searchIndex) nodeOrdinal(n *node) (uint32, bool) {
	ordinal, exists := index.ordinal[n]
	if !exists {
		ordinal, exists = index.addedOrdinal[n]
	}
	return ordinal, exists
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
	if index.orderIdentity {
		return values
	}
	ordered := true
	for i := 1; i < len(values); i++ {
		if index.orderByOrdinal[values[i-1]] > index.orderByOrdinal[values[i]] {
			ordered = false
			break
		}
	}
	if ordered {
		return values
	}
	result := append(posting(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		return index.orderByOrdinal[result[i]] < index.orderByOrdinal[result[j]]
	})
	return result
}

func (index *searchIndex) parentPosting(children posting) posting {
	return index.parentPostingMatching(children, nil)
}

func (index *searchIndex) parentPostingMatching(children, candidates posting) posting {
	if len(children) == 0 {
		return nil
	}
	parentCount := 0
	for _, child := range children {
		parentCount += len(index.parents[child])
	}
	if parentCount <= 64 {
		result := make(posting, 0, parentCount)
		for _, child := range children {
			for _, parent := range index.parents[child] {
				if candidates == nil || postingHas(candidates, parent) {
					result = append(result, parent)
				}
			}
		}
		slices.Sort(result)
		write := 0
		for _, ordinal := range result {
			if write == 0 || result[write-1] != ordinal {
				result[write] = ordinal
				write++
			}
		}
		return result[:write]
	}
	words := make([]uint64, (len(index.nodes)+63)/64)
	for _, child := range children {
		for _, parent := range index.parents[child] {
			words[parent/64] |= uint64(1) << (parent % 64)
		}
	}
	values := candidates
	if values == nil {
		values = index.all
	}
	result := make(posting, 0, min(parentCount, len(values)))
	for _, id := range values {
		if words[id/64]&(uint64(1)<<(id%64)) != 0 {
			result = append(result, id)
		}
	}
	return result
}

func (index *searchIndex) currentParentPostingMatching(children, candidates posting) posting {
	if len(children) == 0 {
		return nil
	}
	result := make(posting, 0)
	for _, child := range children {
		for parent := range index.nodeAt(child).parents {
			ordinal, exists := index.nodeOrdinal(parent)
			if !exists || !index.activeByOrdinal[ordinal] || candidates != nil && !postingHas(candidates, ordinal) {
				continue
			}
			result = append(result, ordinal)
		}
	}
	slices.Sort(result)
	write := 0
	for _, ordinal := range result {
		if write == 0 || result[write-1] != ordinal {
			result[write] = ordinal
			write++
		}
	}
	return result[:write]
}

func intersectPostings(left, right posting) posting {
	if len(left) > len(right) {
		left, right = right, left
	}
	if len(left)*16 < len(right) {
		result := make(posting, 0, len(left))
		for _, ordinal := range left {
			if postingHas(right, ordinal) {
				result = append(result, ordinal)
			}
		}
		return result
	}
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
	if len(base) == 0 {
		return replacements
	}
	if len(excluded) == 0 && len(replacements) == 0 {
		return base
	}
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
	if len(left) == 0 {
		return right
	}
	if len(right) == 0 {
		return left
	}
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
