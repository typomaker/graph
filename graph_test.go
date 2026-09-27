package graph

import "testing"

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
	if !validSearchIndex(first(root)) || first(root).index == index {
		t.Fatal("commit did not replace stale index")
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
	if !Empty(Search(world, kind("location"), expression)) || !Empty(Follow(locationB, expression)) {
		t.Fatal("stale structural index was used")
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
