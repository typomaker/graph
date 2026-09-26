package graph

import (
	"reflect"
	"testing"
)

type actor struct{}
type health struct{ Current, Max int }
type name string
type entityID string
type entityKind string

func TestAttributesAndSelections(t *testing.T) {
	a := New(actor{}, health{10, 10})
	b := New(actor{}, health{5, 10})
	if Len(Merge(a, b, a)) != 2 {
		t.Fatal("merge")
	}
	var h health
	if Empty(Get(a, &h)) || h.Current != 10 {
		t.Fatal("get")
	}
	if !Empty(Set(a, health{10, 10})) {
		t.Fatal("equal set changed")
	}
	if Empty(Set(a, health{9, 10})) {
		t.Fatal("set did not change")
	}
	if Empty(Unset(a, Type[health]())) || !Empty(Get(a, Type[health]())) {
		t.Fatal("unset")
	}
}

func TestDAGQueryAndCommit(t *testing.T) {
	root := New(name("root"))
	a := New(actor{})
	child := New(name("child"))
	Link(root, a)
	Link(a, child)
	q, close := Query(root, Type[actor]())
	if Len(q) != 1 {
		t.Fatal("bootstrap")
	}
	b := New(actor{})
	Link(root, b)
	if Len(q) != 2 {
		t.Fatal("link update")
	}
	Unset(b, Type[actor]())
	if Len(q) != 1 {
		t.Fatal("attribute update")
	}
	Set(b, actor{})
	if Len(q) != 2 {
		t.Fatal("attribute insertion update")
	}
	close()
	Unset(b, Type[actor]())
	if Len(q) != 2 {
		t.Fatal("closed query changed")
	}
	_ = Commit(root)
	Set(child, health{1, 2})
	delta := Commit(root)
	if Len(delta) != 1 || Len(At(delta)) != 1 || Len(At(At(delta))) != 1 {
		t.Fatal("pruned commit path")
	}
	if !Empty(Commit(root)) {
		t.Fatal("second commit not empty")
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

func TestQueryStructuralUpdatesPreserveMultipleParentReachability(t *testing.T) {
	root := New(name("query-root"))
	left := New(name("left"))
	right := New(name("right"))
	target := New(actor{})
	Link(root, left, right)
	Link(left, target)
	Link(right, target)
	result, closeResult := Query(root, Type[actor]())
	defer closeResult()
	if Len(result) != 1 {
		t.Fatalf("bootstrap length=%d", Len(result))
	}
	Unlink(left, target)
	if Len(result) != 1 {
		t.Fatal("node disappeared while still reachable through second parent")
	}
	Unlink(right, target)
	if !Empty(result) {
		t.Fatal("unreachable node remains in query")
	}
	Link(left, target)
	if Len(result) != 1 {
		t.Fatal("relinked subtree was not added incrementally")
	}
}

func TestQueryIndexLifecycleIsDemandDrivenAndShared(t *testing.T) {
	root := New(name("index-root"))
	first := New(actor{}, name("first"))
	second := New(actor{}, name("second"))
	Link(root, first, second)

	state.RLock()
	before := len(state.index)
	state.RUnlock()
	typeResult, closeType := Query(root, Type[actor]())
	otherTypeResult, closeOtherType := Query(root, Type[actor]())
	exactResult, closeExact := Query(root, Type[actor](), name("first"))
	if Len(typeResult) != 2 || Len(otherTypeResult) != 2 || Len(exactResult) != 1 {
		t.Fatal("indexed query bootstrap mismatch")
	}
	typeKey := indexKey{typ: reflect.TypeOf(actor{}), any: true}
	exactKey := indexKey{typ: reflect.TypeOf(name("")), value: name("first")}
	state.RLock()
	if state.index[typeKey] == nil || state.index[typeKey].users != 3 || state.index[exactKey] == nil || state.index[exactKey].users != 1 || len(state.index) != before+2 {
		t.Fatalf("indexes=%+v", state.index)
	}
	state.RUnlock()
	closeType()
	closeType()
	closeOtherType()
	state.RLock()
	if state.index[typeKey] == nil || state.index[typeKey].users != 1 {
		t.Fatal("shared type bucket was released early")
	}
	state.RUnlock()
	closeExact()
	state.RLock()
	if len(state.index) != before {
		t.Fatal("unused query buckets were retained")
	}
	state.RUnlock()
}

func TestApplyBootstrapsAndUpdatesReplica(t *testing.T) {
	source := New(name("world"), health{10, 10})
	actorNode := New(actor{}, name("actor"))
	item := New(name("item"))
	Link(source, actorNode)
	Link(actorNode, item)

	replica := Apply(Graph{}, Commit(source))
	if Len(replica) != 1 || Len(At(replica)) != 1 || Len(At(At(replica))) != 1 {
		t.Fatal("initial delta did not reproduce the graph")
	}
	var got health
	if Empty(Get(replica, &got)) || got != (health{10, 10}) {
		t.Fatal("initial attributes were not applied")
	}

	replicaActors, closeReplicaActors := Query(replica, Type[actor]())
	defer closeReplicaActors()
	if Len(replicaActors) != 1 {
		t.Fatal("replica query bootstrap")
	}

	Set(source, health{7, 10})
	Unset(actorNode, Type[actor]())
	newBranch := New(actor{}, name("new actor"))
	newItem := New(name("new item"))
	Link(newBranch, newItem)
	Link(source, newBranch)
	Unlink(actorNode, item)

	updated := Apply(replica, Commit(source))
	if Len(updated) != 1 {
		t.Fatal("updated roots")
	}
	if Empty(Get(replica, &got)) || got != (health{7, 10}) {
		t.Fatal("changed attribute was not applied")
	}
	children := At(replica)
	if Len(children) != 2 {
		t.Fatal("added branch was not applied")
	}
	var oldActor, replicatedNewActor Graph
	for child := range Each(children) {
		var childName name
		Get(child, &childName)
		switch childName {
		case "actor":
			oldActor = child
		case "new actor":
			replicatedNewActor = child
		}
	}
	if !Empty(Get(oldActor, Type[actor]())) || !Empty(At(oldActor)) {
		t.Fatal("attribute or edge removal was not applied")
	}
	if Empty(Get(replicatedNewActor, Type[actor]())) || Len(At(replicatedNewActor)) != 1 {
		t.Fatal("new subtree was not applied")
	}
	if Len(replicaActors) != 1 {
		t.Fatal("live query was not updated by apply")
	}
}

func TestApplyRejectsNonDelta(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	Apply(New(name("target")), New(name("not a delta")))
}

func TestSelectionOperationsAndNavigation(t *testing.T) {
	a := New(name("a"))
	b := New(name("b"))
	c := New(name("c"))
	Link(a, b, c)
	if Len(At(a, c, b, c, Graph{})) != 2 {
		t.Fatal("filtered children")
	}
	if Len(Include(Merge(a, b, c), Merge(c, a), Merge(a, c))) != 2 {
		t.Fatal("intersection")
	}
	if !Empty(Include()) || Len(Exclude(Merge(a, b, c), b, c)) != 1 || !Empty(Exclude()) {
		t.Fatal("empty or difference")
	}
	count := 0
	for range Each(Merge(a, b)) {
		count++
		break
	}
	if count != 1 {
		t.Fatal("early iteration stop")
	}
	if !Empty(At(Graph{})) || !Empty(Link(Graph{}, a)) || !Empty(Unlink(Graph{}, a)) {
		t.Fatal("empty graph operations")
	}
	if !Empty(Link(a, b)) || !Empty(Unlink(a, New(name("missing")))) {
		t.Fatal("relation no-op")
	}
}

func TestAttributeValidationPanics(t *testing.T) {
	tests := []struct {
		name string
		fn   func()
	}{
		{"new without attributes", func() { New(nil) }},
		{"nil attribute", func() { New(name("x"), nil) }},
		{"pointer attribute", func() { value := name("x"); New(&value) }},
		{"non-comparable attribute", func() { New([]int{1}) }},
		{"duplicate attribute", func() { New(name("x"), name("y")) }},
		{"get without requests", func() { Get(New(name("x"))) }},
		{"nil request", func() { Get(New(name("x")), nil) }},
		{"nil destination", func() { var value *name; Get(New(name("x")), value) }},
		{"non-comparable request", func() { Get(New(name("x")), []int{1}) }},
		{"duplicate request", func() { Get(New(name("x")), name("x"), Type[name]()) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			tt.fn()
		})
	}
}

func TestApplyEmptyDeltaIsNoOp(t *testing.T) {
	target := New(name("target"))
	if Empty(Apply(target, Graph{})) {
		t.Fatal("empty delta discarded target")
	}
}

func TestApplyDeepMergesOrdinaryGraphByCompositeKey(t *testing.T) {
	world := New(name("world"), health{100, 100})
	actorOne := New(entityKind("actor"), entityID("one"), name("old"), health{10, 10})
	itemOne := New(entityKind("item"), entityID("one"), name("sword"))
	Link(world, actorOne, itemOne)
	oldNames, closeOldNames := Query(world, name("old"))
	defer closeOldNames()

	patch := New(name("patched world"))
	actorPatch := New(entityKind("actor"), entityID("one"), name("new"))
	newActorPatch := New(entityKind("actor"), entityID("two"), health{5, 5})
	nestedItemPatch := New(entityKind("item"), entityID("nested"), name("shield"))
	Link(newActorPatch, nestedItemPatch)
	Link(patch, actorPatch, newActorPatch)

	result := Apply(world, patch, Type[entityKind](), Type[entityID]())
	if Len(result) != 1 || Len(At(world)) != 3 {
		t.Fatal("ordinary patch structure was not merged")
	}
	var worldName name
	var actorName name
	var retainedHealth health
	Get(world, &worldName)
	Get(actorOne, &actorName, &retainedHealth)
	if worldName != "patched world" || actorName != "new" || retainedHealth != (health{10, 10}) {
		t.Fatal("attributes were not deeply merged")
	}
	if !Empty(oldNames) {
		t.Fatal("live query index was not updated")
	}
	if Len(At(actorOne)) != 0 {
		t.Fatal("composite key matched the item with the same ID")
	}
	var added Graph
	for child := range Each(At(world)) {
		var kind entityKind
		var id entityID
		Get(child, &kind, &id)
		if kind == "actor" && id == "two" {
			added = child
		}
	}
	if Empty(added) || Len(At(added)) != 1 {
		t.Fatal("new nested branch was not cloned")
	}
}

func TestApplyOrdinaryGraphValidatesMatchKeys(t *testing.T) {
	tests := []struct {
		name  string
		world Graph
		patch Graph
	}{
		{
			name:  "missing key",
			world: New(name("world")),
			patch: func() Graph {
				root := New(name("patch"))
				Link(root, New(name("child")))
				return root
			}(),
		},
		{
			name:  "duplicate patch key",
			world: New(name("world")),
			patch: func() Graph {
				root := New(name("patch"))
				Link(root, New(entityID("same")), New(entityID("same")))
				return root
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			Apply(tt.world, tt.patch, Type[entityID]())
		})
	}
}
