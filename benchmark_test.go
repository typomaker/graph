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

func BenchmarkSearchStructuralFallback10K(b *testing.B) {
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
