package graph

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

type name string
type kind string
type ident string
type health struct{ Current, Maximum int }
type marker struct{}

func value[T any](t *testing.T, g Graph) T {
	t.Helper()
	v, ok := Get[T](g)
	if !ok {
		t.Fatalf("missing %T", v)
	}
	return v
}
func children(g Graph) Graph { return Follow(g, Path(Type[any]())) }

func TestAttributesAndMutations(t *testing.T) {
	n := New(name("one"), health{10, 10})
	if got := value[name](t, n); got != "one" {
		t.Fatal(got)
	}
	if Set(n, name("one")) {
		t.Fatal("equal value changed")
	}
	if !Set(n, name("two")) || value[name](t, n) != "two" {
		t.Fatal("set")
	}
	old, ok := Unset[health](n)
	if !ok || old != (health{10, 10}) {
		t.Fatal("unset")
	}
	if _, ok := Get[health](n); ok {
		t.Fatal("attribute remains")
	}
	if _, ok := Unset[health](n); ok {
		t.Fatal("second unset")
	}
	if Set(Graph{}, name("x")) {
		t.Fatal("empty set")
	}
	if _, ok := Get[name](Graph{}); ok {
		t.Fatal("empty get")
	}
}

func TestSelectFollowAndSearch(t *testing.T) {
	world := New(kind("world"), name("root"))
	locationA := New(kind("location"), name("a"))
	locationB := New(kind("location"), name("b"))
	containsA := New(kind("contains"))
	containsB := New(kind("contains"))
	actorA := New(kind("actor"), ident("actor-1"))
	actorB := New(kind("actor"), ident("actor-2"))
	Link(world, locationA, locationB)
	Link(locationA, containsA)
	Link(locationB, containsB)
	Link(containsA, actorA)
	Link(containsB, actorB)
	path := Path(kind("contains"), And(kind("actor"), ident("actor-1")))
	locations := Search(world, kind("location"), path)
	if Len(locations) != 1 || value[name](t, locations) != "a" {
		t.Fatal("search path")
	}
	if !Empty(Select(world, kind("location"))) {
		t.Fatal("select recursed")
	}
	if Len(Select(Union(locationA, locationB), kind("location"))) != 2 {
		t.Fatal("select")
	}
	ends := Follow(Union(locationA, locationB), kind("location"), path)
	if Len(ends) != 1 || value[ident](t, ends) != "actor-1" {
		t.Fatal("follow")
	}
	if Len(Follow(actorA, kind("actor"))) != 1 {
		t.Fatal("follow without path")
	}
	if Len(Follow(world, Path(Type[any]()))) != 2 {
		t.Fatal("wildcard children")
	}
	if Len(Search(world, Or(ident("actor-1"), ident("actor-2")))) != 2 {
		t.Fatal("or")
	}
	if Len(Search(world, And(kind("actor"), Or(ident("actor-1"), ident("actor-2"))))) != 2 {
		t.Fatal("and")
	}
}

func TestFollowCombinesStructuralExpressions(t *testing.T) {
	root := New(name("root"), marker{})
	a := New(kind("a"))
	b := New(kind("b"))
	Link(root, a, b)
	if Len(Follow(root, Or(marker{}, Path(kind("a"))))) != 2 {
		t.Fatal("or endpoints")
	}
	if Len(Follow(root, And(marker{}, Path(Or(kind("a"), kind("b")))))) != 2 {
		t.Fatal("and endpoints")
	}
	if !Empty(Follow(root, And(Path(kind("a")), Path(kind("missing"))))) {
		t.Fatal("all paths must match")
	}
}

func TestSelectionAndRelations(t *testing.T) {
	a := New(name("a"))
	b := New(name("b"))
	c := New(name("c"))
	if !Link(a, b, c) || Link(a, b) {
		t.Fatal("link result")
	}
	if Len(children(a)) != 2 {
		t.Fatal("children")
	}
	if !Unlink(a, b) || Unlink(a, b) || Unlink(Graph{}, b) {
		t.Fatal("unlink result")
	}
	if Len(children(a)) != 1 {
		t.Fatal("unlink")
	}
	if Len(Union(a, b, a)) != 2 {
		t.Fatal("union")
	}
	if Len(Intersect(Union(a, b, c), Union(c, a))) != 2 || !Empty(Intersect()) {
		t.Fatal("intersect")
	}
	if Len(Difference(Union(a, b, c), b, c)) != 1 || !Empty(Difference()) {
		t.Fatal("difference")
	}
	count := 0
	for range Each(Union(a, b)) {
		count++
		break
	}
	if count != 1 {
		t.Fatal("each")
	}
}

func TestCyclePanics(t *testing.T) {
	a := New(name("a"))
	b := New(name("b"))
	Link(a, b)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	Link(b, a)
}

func TestSnapshotSelectionsDoNotChange(t *testing.T) {
	root := New(name("root"))
	child := New(name("old"))
	Link(root, child)
	found := Search(root, name("old"))
	Set(child, name("new"))
	if Len(found) != 1 || !Empty(Search(root, name("old"))) {
		t.Fatal("snapshot")
	}
}

func TestQueryBootstrapAndAttributeChanges(t *testing.T) {
	root := New(name("root"))
	firstChild := New(kind("actor"), name("first"))
	secondChild := New(kind("item"), name("second"))
	Link(root, firstChild, secondChild)

	query, closeQuery := Query(root, kind("actor"))
	defer closeQuery()
	if got := collectNames(t, query); !reflect.DeepEqual(got, []name{"first"}) {
		t.Fatalf("initial query = %v", got)
	}

	Set(firstChild, kind("item"))
	Set(secondChild, kind("actor"))
	if got := collectNames(t, query); !reflect.DeepEqual(got, []name{"second"}) {
		t.Fatalf("query after attribute changes = %v", got)
	}
	Unset[kind](secondChild)
	if got := collectNames(t, query); len(got) != 0 {
		t.Fatalf("query after unset = %v", got)
	}
}

func TestQueryReflectsLinkChangesBeforeCommit(t *testing.T) {
	root := New(name("root"))
	firstChild := New(kind("actor"), name("first"))
	secondChild := New(kind("actor"), name("second"))
	Link(root, firstChild)

	query, closeQuery := Query(root, kind("actor"))
	defer closeQuery()
	Link(root, secondChild)
	Unlink(root, firstChild)
	if got := collectNames(t, query); !reflect.DeepEqual(got, []name{"second"}) {
		t.Fatalf("query after link changes = %v", got)
	}
}

func TestQueryRefreshesOnceAfterMutations(t *testing.T) {
	root := New(kind("world"))
	child := New(kind("item"))
	Link(root, child)
	query := newReactiveQuery(root, []any{kind("actor")})
	if query.searchCount != 1 {
		t.Fatalf("bootstrap searches = %d", query.searchCount)
	}

	for i := 0; i < 100; i++ {
		Set(child, kind("actor"))
		Set(child, kind("item"))
	}
	Set(child, kind("actor"))
	if query.searchCount != 1 {
		t.Fatalf("searches during mutations = %d", query.searchCount)
	}
	if Len(query.snapshot()) != 1 || query.searchCount != 2 {
		t.Fatalf("first iteration searches = %d", query.searchCount)
	}
	if Len(query.snapshot()) != 1 || query.searchCount != 2 {
		t.Fatalf("unchanged iteration searches = %d", query.searchCount)
	}
	query.close()
}

func TestQueryMutationDuringIterationAppearsNextTime(t *testing.T) {
	root := New(name("root"))
	firstChild := New(kind("actor"), name("first"))
	secondChild := New(kind("actor"), name("second"))
	Link(root, firstChild, secondChild)
	query, closeQuery := Query(root, kind("actor"))
	defer closeQuery()

	var current []name
	for node := range query {
		current = append(current, value[name](t, node))
		if len(current) == 1 {
			Set(secondChild, kind("item"))
		}
	}
	if !reflect.DeepEqual(current, []name{"first", "second"}) {
		t.Fatalf("current iteration = %v", current)
	}
	if got := collectNames(t, query); !reflect.DeepEqual(got, []name{"first"}) {
		t.Fatalf("next iteration = %v", got)
	}
}

func TestQueryConcurrentMutationAndIteration(t *testing.T) {
	root := New(kind("world"))
	child := New(kind("item"))
	Link(root, child)
	query, closeQuery := Query(root, kind("actor"))
	defer closeQuery()

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			Set(child, kind("actor"))
			Set(child, kind("item"))
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			for range query {
			}
		}
	}()
	workers.Wait()
	Set(child, kind("actor"))
	if count := len(collectGraphs(query)); count != 1 {
		t.Fatalf("final query length = %d", count)
	}
}

func TestQueryCloseReleasesState(t *testing.T) {
	root := New(kind("world"))
	query := newReactiveQuery(root, []any{kind("world")})
	query.close()
	query.close()
	if !query.closed || len(query.source.nodes) != 0 || query.conditions != nil || len(query.result.nodes) != 0 || query.revisions != nil {
		t.Fatal("close retained query state")
	}
	if !Empty(query.snapshot()) {
		t.Fatal("closed query is not empty")
	}
}

func collectGraphs(sequence func(func(Graph) bool)) []Graph {
	var graphs []Graph
	for node := range sequence {
		graphs = append(graphs, node)
	}
	return graphs
}

func TestQueryIsMaterializedAndSupportsEarlyStop(t *testing.T) {
	root := New(name("root"))
	firstChild := New(kind("item"), name("first"))
	secondChild := New(kind("item"), name("second"))
	Link(root, firstChild, secondChild)

	query, closeQuery := Query(root, kind("actor"))
	Set(firstChild, kind("actor"))
	Set(secondChild, kind("actor"))

	count := 0
	for range query {
		count++
		break
	}
	if count != 1 {
		t.Fatalf("early-stop count = %d", count)
	}

	closeQuery()
	closeQuery()
	if got := collectNames(t, query); len(got) != 0 {
		t.Fatalf("closed query = %v", got)
	}
}

func TestQueryUpdatesPathMatches(t *testing.T) {
	root := New(name("root"))
	location := New(kind("location"), name("location"))
	contains := New(kind("contains"))
	actor := New(kind("actor"), ident("old"))
	Link(root, location)
	Link(location, contains)
	Link(contains, actor)

	query, closeQuery := Query(root, kind("location"), Path(kind("contains"), ident("target")))
	defer closeQuery()
	if got := collectNames(t, query); len(got) != 0 {
		t.Fatalf("initial path query = %v", got)
	}

	Set(actor, ident("target"))
	if got := collectNames(t, query); !reflect.DeepEqual(got, []name{"location"}) {
		t.Fatalf("path query after match = %v", got)
	}

	Set(actor, ident("old"))
	if got := collectNames(t, query); len(got) != 0 {
		t.Fatalf("path query after removal = %v", got)
	}
}

func collectNames(t *testing.T, sequence func(func(Graph) bool)) []name {
	t.Helper()
	var names []name
	for node := range sequence {
		names = append(names, value[name](t, node))
	}
	return names
}

func TestDeltaApplyAndCommit(t *testing.T) {
	source := New(kind("world"), ident("main"), health{10, 10})
	actor := New(kind("actor"), ident("one"), name("old"))
	item := New(kind("item"), ident("old"))
	Link(source, actor)
	Link(actor, item)
	initial := Delta(source, Type[kind](), Type[ident]())
	if Len(children(initial)) != 1 || Len(Search(initial, kind("item"))) != 1 {
		t.Fatal("delta selection view")
	}
	replica := Apply(Graph{}, initial)
	Commit(source)
	if Len(children(replica)) != 1 || Len(children(children(replica))) != 1 {
		t.Fatal("bootstrap")
	}
	Set(source, health{7, 10})
	Unset[name](actor)
	Unlink(actor, item)
	added := New(kind("item"), ident("new"))
	Link(source, added)
	Apply(replica, Delta(source, Type[kind](), Type[ident]()))
	Commit(source)
	if value[health](t, replica) != (health{7, 10}) || Len(children(replica)) != 2 {
		t.Fatal("apply")
	}
	replicaActor := Search(replica, kind("actor"))
	if _, ok := Get[name](replicaActor); ok || !Empty(children(replicaActor)) {
		t.Fatal("removals")
	}
	if !Empty(Delta(source)) {
		t.Fatal("commit")
	}
}

func TestDeltaNewLinkToCommittedChildIncludesIdentityOnly(t *testing.T) {
	source := New(kind("world"), ident("world"))
	actor := New(kind("actor"), ident("actor"))
	inventory := New(kind("inventory"), ident("inventory"))
	contains := New(kind("contains"), ident("contains"))
	item := New(kind("item"), ident("item"))
	Link(source, actor)
	Link(actor, inventory)
	Link(inventory, contains)
	Link(contains, item)
	replica := Apply(Graph{}, Delta(source, Type[ident]()))
	Commit(source)

	focus := New(kind("focus"), ident("focus"))
	target := New(kind("target"), ident("target"))
	Link(source, focus)
	Link(focus, target)
	Link(target, inventory)
	delta := Delta(source, Type[ident]())

	if Len(Search(delta, kind("inventory"))) != 1 || !Empty(Search(delta, Or(kind("contains"), kind("item")))) {
		t.Fatal("delta included the unchanged subtree of a committed child")
	}
	deltaTarget := Search(delta, kind("target"))
	if Len(children(deltaTarget)) != 1 || value[ident](t, children(deltaTarget)) != ident("inventory") {
		t.Fatal("delta did not include the committed child's identity")
	}

	Apply(replica, delta)
	replicaInventory := Search(replica, ident("inventory"))
	replicaTarget := Search(replica, ident("target"))
	if Len(replicaInventory) != 1 || Len(children(replicaTarget)) != 1 || first(children(replicaTarget)) != first(replicaInventory) {
		t.Fatal("apply did not link target to the existing inventory")
	}
	if Len(Search(replicaInventory, kind("contains"), Path(kind("item")))) != 1 {
		t.Fatal("apply did not preserve the existing inventory subtree")
	}
}

func TestPatchDeepMerge(t *testing.T) {
	world := New(ident("world"), name("old"))
	actor := New(ident("actor"), kind("actor"), health{10, 10})
	Link(world, actor)
	patch := New(ident("world"), name("new"))
	actorPatch := New(ident("actor"), kind("actor"), name("renamed"))
	itemPatch := New(ident("item"), kind("item"))
	Link(actorPatch, itemPatch)
	Link(patch, actorPatch)
	result := Patch(world, patch, Type[ident]())
	if Len(result) != 1 || value[name](t, world) != "new" || value[name](t, actor) != "renamed" {
		t.Fatal("patch attrs")
	}
	if value[health](t, actor) != (health{10, 10}) || Len(children(actor)) != 1 {
		t.Fatal("patch merge")
	}
	Commit(world)
	Patch(world, patch, Type[ident]())
	if !Empty(Delta(world)) {
		t.Fatal("patch no-op")
	}
}

func TestApplyAndPatchValidation(t *testing.T) {
	t.Run("apply ordinary", func(t *testing.T) { defer expectPanic(t); Apply(New(name("target")), New(name("ordinary"))) })
	t.Run("patch delta", func(t *testing.T) {
		defer expectPanic(t)
		Patch(New(ident("target")), Delta(New(ident("source"))), Type[ident]())
	})
	t.Run("patch missing key", func(t *testing.T) {
		defer expectPanic(t)
		root := New(ident("root"))
		p := New(ident("root"))
		Link(p, New(name("child")))
		Patch(root, p, Type[ident]())
	})
	if Empty(Apply(New(name("target")), Graph{})) {
		t.Fatal("empty delta")
	}
}

func TestValidationPanics(t *testing.T) {
	tests := []func(){func() { New(nil) }, func() { New(name("x"), nil) }, func() { v := name("x"); New(&v) }, func() { New([]int{1}) }, func() { New(name("x"), name("y")) }, func() { Select(New(name("x"))) }, func() { Path() }, func() { Path(Path(name("x"))) }, func() { Get[any](New(name("x"))) }, func() { Unset[*name](New(name("x"))) }}
	for _, fn := range tests {
		func() { defer expectPanic(t); fn() }()
	}
}
func expectPanic(t *testing.T) {
	t.Helper()
	if recover() == nil {
		t.Fatal("expected panic")
	}
}

func TestSharedNodeCommit(t *testing.T) {
	a := New(name("a"))
	b := New(name("b"))
	shared := New(health{10, 10})
	Link(a, shared)
	Link(b, shared)
	Commit(Union(a, b))
	Set(shared, health{9, 10})
	if Empty(Delta(a)) || Empty(Delta(b)) {
		t.Fatal("shared delta")
	}
	Commit(a)
	if !Empty(Delta(b)) {
		t.Fatal("global baseline")
	}
	if len(first(b).dirtyChildren) != 0 {
		t.Fatal("delta did not prune a globally committed shared branch")
	}
}

func TestDeltaAndCommitUseChangedFrontier(t *testing.T) {
	root := New(kind("root"))
	children := make([]Graph, 100)
	for i := range children {
		children[i] = New(kind("child"), ident(string(rune(i))))
		Link(root, children[i])
	}
	Commit(root)
	Set(children[73], name("changed"))
	delta := Delta(root)
	if len(delta.view.changes) != 2 || len(delta.view.edges[first(root)]) != 1 || delta.view.edges[first(root)][0] != first(children[73]) {
		t.Fatal("delta traversed outside the changed frontier")
	}
	index := first(root).index
	Commit(root)
	if first(root).index != index || !Empty(Delta(root)) || len(first(root).dirtyChildren) != 0 {
		t.Fatal("incremental commit did not consume the changed frontier")
	}
	if result := Search(root, name("changed")); Len(result) != 1 || value[ident](t, result) != ident(string(rune(73))) {
		t.Fatal("incremental commit lost the attribute overlay")
	}
}

func TestSharedDiamondFrontierDeltaAndCommit(t *testing.T) {
	root := New(kind("root"))
	left := New(kind("parent"), name("left"))
	right := New(kind("parent"), name("right"))
	shared := New(kind("shared"), ident("shared"))
	Link(root, left, right)
	Link(left, shared)
	Link(right, shared)
	Commit(root)

	Set(shared, name("changed"))
	delta := Delta(root)
	if len(delta.view.edges[first(root)]) != 2 || len(delta.view.changes) != 4 {
		t.Fatal("delta did not retain every path to a shared change")
	}
	Commit(root)
	if !Empty(Delta(root)) {
		t.Fatal("commit did not deduplicate a shared changed node")
	}

	Set(shared, name("again"))
	Unlink(left, shared)
	Commit(root)
	if Len(Search(root, kind("shared"))) != 1 || !Empty(Delta(root)) {
		t.Fatal("commit did not discard a detached dirty path")
	}
}

func TestIncrementalStructuralCommitAndCompaction(t *testing.T) {
	root := New(kind("root"))
	Commit(root)
	index := first(root).index
	child := New(kind("child"))
	Link(root, child)
	Commit(root)
	edge := graphEdge{parent: first(root), child: first(child)}
	if first(root).index != index || len(index.addedEdges) != 0 {
		t.Fatal("structural commit rebuilt the small overlay")
	}
	if _, committed := index.committedAdded[edge]; !committed || !Empty(Delta(root)) || Len(Search(root, kind("child"))) != 1 {
		t.Fatal("added edge was not committed incrementally")
	}
	Unlink(root, child)
	if Empty(Delta(root)) {
		t.Fatal("committed added edge removal was not reported")
	}
	Commit(root)
	if _, retained := index.committedAdded[edge]; retained || len(index.committedRemoved) != 0 || !Empty(Search(root, kind("child"))) {
		t.Fatal("structural baseline did not normalize a removed added edge")
	}

	for i := 0; i < 300; i++ {
		Link(root, New(kind("bulk"), ident(string(rune(i)))))
	}
	Commit(root)
	if first(root).index == index || len(first(root).index.added) != 0 || Len(Search(root, kind("bulk"))) != 300 {
		t.Fatal("large structural overlay did not compact")
	}
}

func TestCommitBuildsAndInvalidatesSearchIndex(t *testing.T) {
	root := New(kind("world"))
	firstActor := New(kind("actor"), ident("one"))
	secondActor := New(kind("actor"), ident("two"))
	Link(root, firstActor, secondActor)
	Commit(root)

	index := first(root).index
	if index == nil || !validSearchIndex(first(root)) || len(index.nodes) != 3 {
		t.Fatal("commit did not publish a valid root index")
	}
	if got := Search(root, kind("actor"), ident("two")); Len(got) != 1 || value[ident](t, got) != "two" {
		t.Fatal("indexed attribute search")
	}

	Set(secondActor, ident("changed"))
	if !validSearchIndex(first(root)) || len(first(root).index.dirty) != 1 {
		t.Fatal("attribute mutation did not create an index overlay")
	}
	if !Empty(Search(root, ident("two"))) || Len(Search(root, ident("changed"))) != 1 {
		t.Fatal("dirty overlay did not replace the indexed attribute")
	}
	Unset[ident](secondActor)
	if result := Search(root, Type[ident]()); Len(result) != 1 || value[ident](t, result) != "one" || !validSearchIndex(first(root)) {
		t.Fatal("dirty overlay did not remove the indexed attribute")
	}

	Commit(root)
	if !validSearchIndex(first(root)) || first(root).index != index || !Empty(Delta(root)) {
		t.Fatal("incremental commit did not retain the current index")
	}
}

func TestAttributeOverlayClearsWhenCommittedStateReturns(t *testing.T) {
	root := New(kind("root"))
	child := New(ident("committed"), kind("actor"))
	Link(root, child)
	Commit(root)
	Set(child, ident("changed"))
	if len(first(root).index.dirty) != 1 {
		t.Fatal("attribute change was not tracked")
	}
	Set(child, ident("committed"))
	if len(first(root).index.dirty) != 0 || Len(Search(root, ident("committed"))) != 1 {
		t.Fatal("restored committed attributes left a dirty overlay")
	}
}

func TestCommittedPathSearchAndFollow(t *testing.T) {
	world := New(kind("world"))
	locationA := New(kind("location"), name("a"))
	locationB := New(kind("location"), name("b"))
	containsA := New(kind("contains"))
	containsB := New(kind("contains"))
	actorA := New(kind("actor"), ident("one"))
	actorB := New(kind("actor"), ident("two"))
	Link(world, locationA, locationB)
	Link(locationA, containsA)
	Link(locationB, containsB)
	Link(containsA, actorA)
	Link(containsB, actorB)
	Commit(world)

	expression := Path(kind("contains"), And(kind("actor"), ident("two")))
	locations := Search(world, kind("location"), expression)
	if Len(locations) != 1 || value[name](t, locations) != "b" {
		t.Fatal("indexed reverse path")
	}
	allLocations := Search(world, kind("location"), Or(
		Path(kind("contains"), And(kind("actor"), ident("one"))),
		Path(kind("contains"), And(kind("actor"), ident("two"))),
	))
	if Len(allLocations) != 2 || Len(Search(world, Type[any]())) != 7 {
		t.Fatal("indexed or or wildcard")
	}
	actors := Follow(locations, kind("location"), expression)
	if Len(actors) != 1 || value[ident](t, actors) != "two" {
		t.Fatal("indexed follow")
	}
	Set(actorB, ident("one"))
	if !Empty(Search(world, kind("location"), expression)) {
		t.Fatal("path index did not recheck ancestors of a dirty endpoint")
	}
	if Len(Search(world, kind("location"), Path(kind("contains"), And(kind("actor"), ident("one"))))) != 2 {
		t.Fatal("path overlay did not add a newly matching endpoint")
	}
	Set(actorB, ident("two"))

	Unlink(containsB, actorB)
	if !validSearchIndex(first(world)) || !Empty(Search(world, kind("location"), expression)) || !Empty(Follow(locationB, expression)) {
		t.Fatal("stale structural index was used")
	}
	Link(containsB, actorB)
	if Len(Search(world, kind("location"), expression)) != 1 {
		t.Fatal("relinked committed endpoint did not update path overlay")
	}
}

func TestStructuralOverlayAddsAndRemovesReachableNodes(t *testing.T) {
	root := New(kind("root"))
	branch := New(kind("branch"))
	Link(root, branch)
	Commit(root)

	added := New(kind("actor"), ident("added"))
	Link(branch, added)
	if !validSearchIndex(first(root)) || len(first(root).index.added) != 1 {
		t.Fatal("link invalidated the committed index")
	}
	if result := Search(root, kind("actor")); Len(result) != 1 || value[ident](t, result) != "added" {
		t.Fatal("added structural overlay node was not searched")
	}

	Unlink(branch, added)
	if index := first(root).index; !validSearchIndex(first(root)) || len(index.addedEdges) != 0 || len(index.removedEdges) != 0 || !Empty(Search(root, kind("actor"))) {
		t.Fatal("removed structural overlay node remained searchable")
	}

	Link(branch, added)
	Commit(root)
	Unlink(branch, added)
	if index := first(root).index; !validSearchIndex(first(root)) || index.activeByOrdinal[index.addedOrdinal[first(added)]] || !Empty(Search(root, ident("added"))) {
		t.Fatal("committed node removal did not use structural overlay")
	}
}

func TestAddedNodePostingsTrackAttributesAndReachability(t *testing.T) {
	root := New(kind("root"))
	Commit(root)
	branch := New(kind("branch"))
	added := make([]Graph, addedPostingThreshold+1)
	for i := range added {
		added[i] = New(kind("actor"), ident(string(rune('a'+i))))
		Link(branch, added[i])
	}
	Link(root, branch)

	index := first(root).index
	if !index.addedIndexed || Len(Search(root, kind("actor"))) != len(added) {
		t.Fatal("large added overlay was not indexed")
	}
	if Len(Search(root, Type[any]())) != len(added)+2 || Len(Search(root, Type[kind]())) != len(added)+2 {
		t.Fatal("added wildcard postings")
	}
	if Len(Search(root, And(kind("actor"), Or(ident("a"), ident("b"))))) != 2 {
		t.Fatal("added logical postings")
	}
	if Len(Search(root, Or(kind("branch"), ident("a")))) != 2 {
		t.Fatal("added union postings")
	}
	func() {
		defer expectPanic(t)
		index.matchAdded(predicateFromValue(Path(kind("actor"))))
	}()
	func() {
		defer expectPanic(t)
		index.matchAdded(&predicate{op: predicateOp(255)})
	}()
	Set(added[0], kind("changed"))
	if Len(Search(root, kind("actor"))) != len(added)-1 || Len(Search(root, kind("changed"))) != 1 {
		t.Fatal("added postings did not track Set")
	}
	Unset[kind](added[1])
	if Len(Search(root, Type[kind]())) != len(added)+1 {
		t.Fatal("added postings did not track Unset")
	}
	Unlink(root, branch)
	if !Empty(Search(root, kind("actor"))) {
		t.Fatal("inactive added postings remained searchable")
	}
	Link(root, branch)
	if Len(Search(root, kind("actor"))) != len(added)-2 {
		t.Fatal("reactivated added postings were not searchable")
	}
}

func TestAddedPathOverlayUsesCurrentReverseEdges(t *testing.T) {
	root := New(kind("root"))
	Commit(root)
	locations := make([]Graph, addedPostingThreshold+1)
	actors := make([]Graph, len(locations))
	for i := range locations {
		locations[i] = New(kind("location"), ident(string(rune('a'+i))))
		relation := New(kind("contains"))
		actors[i] = New(kind("actor"), name(string(rune('a'+i))))
		Link(locations[i], relation)
		Link(relation, actors[i])
		Link(root, locations[i])
	}
	expression := Path(kind("contains"), And(kind("actor"), name("z")))
	result := Search(root, kind("location"), expression)
	if Len(result) != 1 || value[ident](t, result) != "z" {
		t.Fatal("added path did not use current reverse edges")
	}
	relation := Follow(locations[25], Path(kind("contains")))
	Unlink(relation, actors[25])
	if !Empty(Search(root, kind("location"), expression)) {
		t.Fatal("removed added path edge remained indexed")
	}
	Link(relation, actors[25])
	if Len(Search(root, kind("location"), expression)) != 1 {
		t.Fatal("restored added path edge was not indexed")
	}
	Set(root, kind("location"))
	if Len(Search(root, Or(expression, Path(kind("contains"), And(kind("actor"), name("missing")))))) != 1 {
		t.Fatal("added path alternatives")
	}
	func() {
		defer expectPanic(t)
		first(root).index.matchOverlay(&predicate{op: predicateOp(255)})
	}()
}

func TestDirtyPostingIndexTracksLargeAttributeOverlay(t *testing.T) {
	root := New(kind("root"))
	nodes := make([]Graph, dirtyPostingThreshold)
	for i := range nodes {
		nodes[i] = New(kind("actor"), ident(fmt.Sprint(i)), benchmarkGroup(i%2))
		Link(root, nodes[i])
	}
	Commit(root)
	for _, node := range nodes {
		Set(node, benchmarkGroup(3))
	}
	index := first(root).index
	if !index.dirtyIndexed || Len(Search(root, benchmarkGroup(3))) != len(nodes) {
		t.Fatal("large dirty overlay was not indexed")
	}
	if Len(Search(root, And(benchmarkGroup(3), ident("5")))) != 1 || Len(Search(root, Or(benchmarkGroup(0), benchmarkGroup(3)))) != len(nodes) {
		t.Fatal("dirty logical postings")
	}
	Set(nodes[0], benchmarkGroup(0))
	Set(nodes[1], benchmarkGroup(3))
	Set(nodes[1], benchmarkVersion(7))
	if Len(Search(root, benchmarkGroup(3))) != len(nodes)-1 || Len(Search(root, benchmarkVersion(7))) != 1 {
		t.Fatal("dirty posting update")
	}
	Unset[benchmarkVersion](nodes[1])
	if !Empty(Search(root, benchmarkVersion(7))) || Len(Search(root, Type[benchmarkGroup]())) != len(nodes) {
		t.Fatal("dirty posting removal")
	}
	if Len(Search(root, Type[any]())) != len(nodes)+1 || index.matchDirty(predicateFromValue(Path(kind("actor")))) != nil {
		t.Fatal("dirty wildcard posting")
	}
	values := insertPostingOrdinal(nil, 2)
	values = insertPostingOrdinal(values, 2)
	values = insertPostingOrdinal(values, 1)
	values = removePostingOrdinal(values, 3)
	values = removePostingOrdinal(values, 1)
	if !reflect.DeepEqual(values, posting{2}) {
		t.Fatalf("posting helpers = %v", values)
	}
}

func TestDenseCommittedPathExpansion(t *testing.T) {
	root := New(kind("root"))
	for i := 0; i < 100; i++ {
		location := New(kind("location"))
		relation := New(kind("contains"))
		actor := New(kind("actor"))
		Link(root, location)
		Link(location, relation)
		Link(relation, actor)
	}
	Commit(root)
	if Len(Search(root, kind("location"), Path(kind("contains"), kind("actor")))) != 100 {
		t.Fatal("dense committed path expansion")
	}
}

func TestApplyAndPatchKeepCommittedSearchIndex(t *testing.T) {
	t.Run("apply", func(t *testing.T) {
		source := New(kind("root"), ident("root"))
		original := New(kind("actor"), ident("original"))
		Link(source, original)
		replica := Apply(Graph{}, Delta(source, Type[ident]()))
		Commit(source)
		Commit(replica)
		Unlink(source, original)
		Apply(replica, Delta(source, Type[ident]()))
		if !validSearchIndex(first(replica)) || !Empty(Search(replica, kind("actor"))) {
			t.Fatal("apply removal invalidated the committed search index")
		}
		Commit(source)
		child := New(kind("actor"), ident("child"))
		Link(source, child)
		Apply(replica, Delta(source, Type[ident]()))
		if !validSearchIndex(first(replica)) || Len(Search(replica, kind("actor"))) != 1 {
			t.Fatal("apply invalidated the committed search index")
		}
	})

	t.Run("patch", func(t *testing.T) {
		root := New(kind("root"), ident("root"))
		Commit(root)
		patch := New(ident("root"))
		Link(patch, New(kind("actor"), ident("child")))
		Patch(root, patch, Type[ident]())
		if !validSearchIndex(first(root)) || Len(Search(root, kind("actor"))) != 1 {
			t.Fatal("patch invalidated the committed search index")
		}
	})
}

func TestStructuralOverlayCountsReachableParents(t *testing.T) {
	root := New(kind("root"))
	left := New(kind("parent"), name("left"))
	right := New(kind("parent"), name("right"))
	shared := New(kind("shared"))
	Link(root, left, right)
	Link(left, shared)
	Link(right, shared)
	Commit(root)

	Unlink(left, shared)
	if Len(Search(root, kind("shared"))) != 1 || first(root).index.support[first(shared)] != 1 {
		t.Fatal("shared node was removed while it still had an active parent")
	}
	Unlink(right, shared)
	if !Empty(Search(root, kind("shared"))) || first(root).index.support[first(shared)] != 0 {
		t.Fatal("shared node remained after losing its last active parent")
	}
	Link(left, shared)
	if Len(Search(root, kind("shared"))) != 1 {
		t.Fatal("shared node was not reactivated")
	}
}

func TestSearchCombinesIndexedAndUncommittedRoots(t *testing.T) {
	committed := New(kind("root"))
	committedActor := New(kind("actor"), ident("committed"))
	Link(committed, committedActor)
	Commit(committed)

	uncommitted := New(kind("root"))
	uncommittedActor := New(kind("actor"), ident("uncommitted"))
	Link(uncommitted, uncommittedActor)
	shared := New(kind("actor"), ident("shared"))
	Link(committed, shared)
	Link(uncommitted, shared)
	Commit(committed)

	result := Search(Union(committed, uncommitted), kind("actor"))
	if Len(result) != 3 {
		t.Fatal("mixed indexed and traversal roots")
	}
}

func TestStructuralOverlayMatchesTraversalAcrossMutations(t *testing.T) {
	root := New(kind("root"))
	nodes := make([]Graph, 24)
	for i := range nodes {
		nodeKind := kind("other")
		if i%3 == 0 {
			nodeKind = "actor"
		}
		nodes[i] = New(nodeKind, ident(string(rune('a'+i))))
		if i < 6 {
			Link(root, nodes[i])
		}
	}
	for i := 0; i < 18; i++ {
		Link(nodes[i%6], nodes[i+6])
	}
	Commit(root)

	seed := uint32(1)
	for step := 0; step < 80; step++ {
		seed = seed*1664525 + 1013904223
		from := int(seed % uint32(len(nodes)-1))
		seed = seed*1664525 + 1013904223
		to := from + 1 + int(seed%uint32(len(nodes)-from-1))
		if step%2 == 0 {
			Link(nodes[from], nodes[to])
		} else {
			Unlink(nodes[from], nodes[to])
		}
		assertIndexedSearchEqualsTraversal(t, root, Type[any]())
		assertIndexedSearchEqualsTraversal(t, root, kind("actor"))
		assertIndexedSearchEqualsTraversal(t, root, Path(kind("actor")))
	}
}

func TestAddedPathOverlayMatchesTraversalAcrossMutations(t *testing.T) {
	root := New(kind("root"))
	Commit(root)
	locations := make([]Graph, 40)
	actors := make([]Graph, len(locations))
	linked := make([]bool, len(locations))
	for i := range locations {
		locations[i] = New(kind("location"), ident(string(rune('a'+i))))
		relation := New(kind("contains"))
		actors[i] = New(kind("actor"), name("other"))
		Link(locations[i], relation)
		Link(relation, actors[i])
	}

	seed := uint32(7)
	for step := 0; step < 160; step++ {
		seed = seed*1664525 + 1013904223
		position := int(seed % uint32(len(locations)))
		if step%3 == 0 {
			if linked[position] {
				Unlink(root, locations[position])
			} else {
				Link(root, locations[position])
			}
			linked[position] = !linked[position]
		} else if step%2 == 0 {
			Set(actors[position], name("target"))
		} else {
			Set(actors[position], name("other"))
		}
		assertIndexedSearchEqualsTraversal(t, root, Type[any]())
		assertIndexedSearchEqualsTraversal(t, root, kind("location"), Path(kind("contains"), And(kind("actor"), name("target"))))
		if step%11 == 0 {
			Commit(root)
			if !Empty(Delta(root)) {
				t.Fatalf("step %d: incremental commit left a delta", step)
			}
			assertIndexedSearchEqualsTraversal(t, root, kind("location"), Path(kind("contains"), And(kind("actor"), name("target"))))
		}
	}
}

func TestPatchCompositeIndexTracksKeyChanges(t *testing.T) {
	root := New(ident("root"))
	child := New(ident("old"), name("before"))
	Link(root, child)
	Commit(root)
	Patch(root, New(ident("root")), Type[ident]())
	Patch(root, New(ident("root")), Type[ident]())
	if !sameTypes([]reflect.Type{attributeType[ident]()}, []reflect.Type{attributeType[ident]()}) || sameTypes(nil, []reflect.Type{attributeType[ident]()}) || sameTypes([]reflect.Type{attributeType[ident]()}, []reflect.Type{attributeType[name]()}) {
		t.Fatal("composite index type identity")
	}
	Set(child, ident("new"))
	patch := New(ident("root"))
	Link(patch, New(ident("new"), name("after")))
	Patch(root, patch, Type[ident]())
	if Len(Search(root, ident("new"))) != 1 || value[name](t, child) != "after" {
		t.Fatal("cached composite index survived a key change")
	}
}

func TestApplyUsesCommittedInternalIdentityIndex(t *testing.T) {
	source := New(kind("root"))
	child := New(kind("actor"), name("before"))
	Link(source, child)
	replica := Apply(Graph{}, Delta(source))
	Commit(source)
	Commit(replica)
	Set(child, name("after"))
	Apply(replica, Delta(source))
	result := Search(replica, kind("actor"))
	if Len(result) != 1 || value[name](t, result) != "after" {
		t.Fatal("apply did not use committed internal identity")
	}
}

func TestIndexedApplyMatchesSourceAcrossMutations(t *testing.T) {
	source := New(kind("root"), ident("root"))
	nodes := make([]Graph, 24)
	linked := make([]bool, len(nodes))
	for i := range nodes {
		nodes[i] = New(kind("actor"), ident(string(rune('a'+i))), name("initial"))
		if i < 12 {
			Link(source, nodes[i])
			linked[i] = true
		}
	}
	replica := Apply(Graph{}, Delta(source, Type[ident]()))
	Commit(source)
	Commit(replica)
	seed := uint32(11)
	for step := 0; step < 100; step++ {
		seed = seed*1664525 + 1013904223
		position := int(seed % uint32(len(nodes)))
		if step%3 == 0 {
			if linked[position] {
				Unlink(source, nodes[position])
			} else {
				Link(source, nodes[position])
			}
			linked[position] = !linked[position]
		} else {
			Set(nodes[position], name(string(rune('a'+step%20))))
		}
		Apply(replica, Delta(source, Type[ident]()))
		Commit(source)
		expected := selected(Search(source, kind("actor")))
		actual := selected(Search(replica, kind("actor")))
		if len(expected) != len(actual) {
			t.Fatalf("step %d: replica length %d, want %d", step, len(actual), len(expected))
		}
		expectedNames := make(map[ident]name, len(expected))
		for _, n := range expected {
			expectedNames[n.attrs[attributeType[ident]()].(ident)] = n.attrs[attributeType[name]()].(name)
		}
		for _, n := range actual {
			id := n.attrs[attributeType[ident]()].(ident)
			if expectedNames[id] != n.attrs[attributeType[name]()].(name) {
				t.Fatalf("step %d: replica node %q differs", step, id)
			}
		}
		if !validSearchIndex(first(replica)) {
			t.Fatalf("step %d: apply invalidated search index", step)
		}
	}
}

func TestInternalIdentityIndexFiltersReachabilityAndDuplicates(t *testing.T) {
	root := New(kind("root"))
	firstChild := New(kind("first"))
	secondChild := New(kind("second"))
	first(secondChild).key = first(firstChild).key
	Link(root, firstChild, secondChild)
	Commit(root)
	Unlink(root, secondChild)
	if first(root).index.activeNodeByKey(first(firstChild).key) != first(firstChild) {
		t.Fatal("internal identity index did not skip an inactive duplicate")
	}
	Link(root, secondChild)
	func() {
		defer expectPanic(t)
		first(root).index.activeNodeByKey(first(firstChild).key)
	}()
}

func assertIndexedSearchEqualsTraversal(t *testing.T, root Graph, predicates ...any) {
	t.Helper()
	indexed := Search(root, predicates...)
	state.Lock()
	n := first(root)
	saved := n.index
	n.index = nil
	state.Unlock()
	traversed := Search(root, predicates...)
	state.Lock()
	n.index = saved
	state.Unlock()
	if Len(indexed) != Len(traversed) {
		t.Fatalf("indexed length %d differs from traversal %d", Len(indexed), Len(traversed))
	}
	indexedNodes, traversalNodes := selected(indexed), selected(traversed)
	for i := range indexedNodes {
		if indexedNodes[i] != traversalNodes[i] {
			t.Fatalf("indexed order differs from traversal at %d", i)
		}
	}
}
