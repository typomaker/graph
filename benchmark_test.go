package graph

import (
	"fmt"
	"testing"
)

type benchmarkKind string
type benchmarkID string

func benchmarkWorld(size int) Graph {
	root := New(benchmarkKind("world"))
	for i := 0; i < size; i++ {
		child := New(benchmarkKind("actor"), benchmarkID(fmt.Sprint(i)))
		Link(root, child)
	}
	return root
}

func BenchmarkSearch10K(b *testing.B) {
	root := benchmarkWorld(10000)
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("actor"))) != 10000 {
			b.Fatal()
		}
	}
}

func BenchmarkSearchCommitted10K(b *testing.B) {
	root := benchmarkWorld(10000)
	Commit(root)
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("actor"))) != 10000 {
			b.Fatal()
		}
	}
}

func BenchmarkSearchAttributeOverlay10K(b *testing.B) {
	root := benchmarkWorld(10000)
	Commit(root)
	Set(root, benchmarkID("dirty"))
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("actor"))) != 10000 {
			b.Fatal()
		}
	}
}

func BenchmarkSearchRestoredAttribute10K(b *testing.B) {
	root := benchmarkWorld(10000)
	Commit(root)
	Set(root, benchmarkID("dirty"))
	Unset[benchmarkID](root)
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("actor"))) != 10000 {
			b.Fatal()
		}
	}
}

func BenchmarkSearchStructuralOverlay10K(b *testing.B) {
	root := benchmarkWorld(10000)
	Commit(root)
	Link(root, New(benchmarkKind("other")))
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("actor"))) != 10000 {
			b.Fatal()
		}
	}
}

func BenchmarkSearchCancelledEdge10K(b *testing.B) {
	root := benchmarkWorld(10000)
	leaf := New(benchmarkKind("leaf"))
	Commit(root)
	Link(root, leaf)
	Unlink(root, leaf)
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("actor"))) != 10000 {
			b.Fatal()
		}
	}
}

func BenchmarkSelect10K(b *testing.B) {
	root := benchmarkWorld(10000)
	all := Search(root, Type[any]())
	b.ResetTimer()
	for range b.N {
		if Len(Select(all, benchmarkKind("actor"))) != 10000 {
			b.Fatal()
		}
	}
}

func BenchmarkPathSearch10K(b *testing.B) {
	root := benchmarkPathWorld(10000)
	expression := Path(benchmarkKind("contains"), And(benchmarkKind("actor"), benchmarkID("9999")))
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("location"), expression)) != 1 {
			b.Fatal()
		}
	}
}

func BenchmarkPathSearchCommitted10K(b *testing.B) {
	root := benchmarkPathWorld(10000)
	Commit(root)
	expression := Path(benchmarkKind("contains"), And(benchmarkKind("actor"), benchmarkID("9999")))
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("location"), expression)) != 1 {
			b.Fatal()
		}
	}
}

func BenchmarkPathSearchAttributeOverlay10K(b *testing.B) {
	root := benchmarkPathWorld(10000)
	Commit(root)
	Set(root, benchmarkID("dirty"))
	expression := Path(benchmarkKind("contains"), And(benchmarkKind("actor"), benchmarkID("9999")))
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("location"), expression)) != 1 {
			b.Fatal()
		}
	}
}

func BenchmarkPathFollowCommitted10K(b *testing.B) {
	root := benchmarkPathWorld(10000)
	Commit(root)
	locations := Search(root, benchmarkKind("location"))
	expression := Path(benchmarkKind("contains"), And(benchmarkKind("actor"), benchmarkID("9999")))
	b.ResetTimer()
	for range b.N {
		if Len(Follow(locations, benchmarkKind("location"), expression)) != 1 {
			b.Fatal()
		}
	}
}

func BenchmarkCommit10K(b *testing.B) {
	root := benchmarkWorld(10000)
	b.ResetTimer()
	for range b.N {
		Commit(root)
	}
}

func BenchmarkStructuralToggleLeaf10K(b *testing.B) {
	root := benchmarkWorld(10000)
	leaf := New(benchmarkKind("leaf"))
	Commit(root)
	b.ResetTimer()
	for range b.N {
		Link(root, leaf)
		Unlink(root, leaf)
	}
}

func BenchmarkStructuralToggleShared10K(b *testing.B) {
	root := benchmarkWorld(10000)
	parents := Follow(root, Path(Type[any]()))
	firstParent := Graph{nodes: []*node{selected(parents)[0]}}
	secondParent := Graph{nodes: []*node{selected(parents)[1]}}
	shared := New(benchmarkKind("shared"))
	Link(firstParent, shared)
	Commit(root)
	b.ResetTimer()
	for range b.N {
		Link(secondParent, shared)
		Unlink(secondParent, shared)
	}
}

func BenchmarkStructuralToggleBranch100Of10K(b *testing.B) {
	root := benchmarkWorld(10000)
	branch := New(benchmarkKind("branch"))
	current := branch
	for range 99 {
		next := New(benchmarkKind("branch"))
		Link(current, next)
		current = next
	}
	Commit(root)
	b.ResetTimer()
	for range b.N {
		Link(root, branch)
		Unlink(root, branch)
	}
}

func BenchmarkSearchFirstAfterStructuralChange10K(b *testing.B) {
	root := benchmarkWorld(10000)
	leaf := New(benchmarkKind("leaf"))
	Commit(root)
	linked := false
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		if linked {
			Unlink(root, leaf)
		} else {
			Link(root, leaf)
		}
		linked = !linked
		b.StartTimer()
		Search(root, benchmarkKind("actor"))
	}
}

func benchmarkPathWorld(size int) Graph {
	root := New(benchmarkKind("world"))
	for i := 0; i < size; i++ {
		location := New(benchmarkKind("location"))
		relation := New(benchmarkKind("contains"))
		actor := New(benchmarkKind("actor"), benchmarkID(fmt.Sprint(i)))
		Link(root, location)
		Link(location, relation)
		Link(relation, actor)
	}
	return root
}

func BenchmarkFollowChildren10K(b *testing.B) {
	root := benchmarkWorld(10000)
	expression := Path(Type[any]())
	b.ResetTimer()
	for range b.N {
		if Len(Follow(root, expression)) != 10000 {
			b.Fatal()
		}
	}
}
