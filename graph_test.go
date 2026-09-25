package graph

import "testing"

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
	close()
	Set(b, actor{})
	if Len(q) != 1 {
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
