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
	root := New(benchmarkKind("world"))
	for i := 0; i < 10000; i++ {
		location := New(benchmarkKind("location"))
		relation := New(benchmarkKind("contains"))
		actor := New(benchmarkKind("actor"), benchmarkID(fmt.Sprint(i)))
		Link(root, location)
		Link(location, relation)
		Link(relation, actor)
	}
	expression := Path(benchmarkKind("contains"), And(benchmarkKind("actor"), benchmarkID("9999")))
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("location"), expression)) != 1 {
			b.Fatal()
		}
	}
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
