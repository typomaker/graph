package graph

import (
	"reflect"
	"testing"
)

type actor struct{}
type health struct{ Current, Max int }
type name string

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
