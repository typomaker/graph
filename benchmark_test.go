package graph

import (
	"fmt"
	"testing"
)

type benchmarkKind struct{}
type benchmarkID int
type benchmarkHealth int
type benchmarkLocation struct{}
type benchmarkContains struct{}

func benchmarkWorld(size int) (Graph, []Graph) {
	root := New(name(fmt.Sprintf("benchmark-%d", size)))
	nodes := make([]Graph, size)
	for i := range nodes {
		nodes[i] = New(benchmarkKind{}, benchmarkID(i), benchmarkHealth(100))
		Link(root, nodes[i])
	}
	return root, nodes
}

func BenchmarkQueryBootstrap(b *testing.B) {
	for _, size := range []int{100, 1_000, 10_000} {
		root, _ := benchmarkWorld(size)
		b.Run(fmt.Sprintf("nodes=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				result, closeQuery := Query(root, Type[benchmarkKind](), benchmarkID(size/2))
				if Len(result) != 1 {
					b.Fatal("unexpected query result")
				}
				closeQuery()
			}
		})
	}
}

func BenchmarkQueryRead10K(b *testing.B) {
	root, _ := benchmarkWorld(10_000)
	result, closeQuery := Query(root, Type[benchmarkKind]())
	defer closeQuery()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if Len(result) != 10_000 {
			b.Fatal("unexpected query result")
		}
	}
}

func BenchmarkQueryUpdate10K(b *testing.B) {
	root, nodes := benchmarkWorld(10_000)
	result, closeQuery := Query(root, benchmarkID(5_000))
	defer closeQuery()
	target := nodes[5_000]
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		Set(target, benchmarkID(5_000+i%2))
	}
	if Len(result) > 1 {
		b.Fatal("unexpected query result")
	}
}

func BenchmarkQueryUnrelatedUpdate10K(b *testing.B) {
	root, nodes := benchmarkWorld(10_000)
	result, closeQuery := Query(root, benchmarkID(5_000))
	defer closeQuery()
	target := nodes[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		Set(target, benchmarkHealth(i))
	}
	if Len(result) != 1 {
		b.Fatal("unexpected query result")
	}
}

func BenchmarkQueryReuseActiveIndex10K(b *testing.B) {
	root, _ := benchmarkWorld(10_000)
	_, closeKeeper := Query(root, Type[benchmarkKind](), benchmarkID(5_000))
	defer closeKeeper()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, closeQuery := Query(root, Type[benchmarkKind](), benchmarkID(5_000))
		if Len(result) != 1 {
			b.Fatal("unexpected shared query result")
		}
		closeQuery()
	}
}

func benchmarkPathWorld(size int) (Graph, []Graph, []Graph, []Graph) {
	root := New(name(fmt.Sprintf("path-benchmark-%d", size)))
	locations := make([]Graph, size)
	contains := make([]Graph, size)
	actors := make([]Graph, size)
	for i := range size {
		locations[i] = New(benchmarkLocation{}, benchmarkID(i))
		contains[i] = New(benchmarkContains{})
		actors[i] = New(benchmarkKind{}, benchmarkID(i))
		Link(root, locations[i])
		Link(locations[i], contains[i])
		Link(contains[i], actors[i])
	}
	return root, locations, contains, actors
}

func BenchmarkPathQueryBootstrap10K(b *testing.B) {
	root, _, _, _ := benchmarkPathWorld(10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, closeQuery := Query(
			root,
			Path(Type[benchmarkLocation](), Type[benchmarkContains](), Match(Type[benchmarkKind](), benchmarkID(5_000))),
		)
		if Len(result) != 1 {
			b.Fatal("unexpected path query result")
		}
		closeQuery()
	}
}

func BenchmarkPathQueryAttributeUpdate10K(b *testing.B) {
	root, _, _, actors := benchmarkPathWorld(10_000)
	result, closeQuery := Query(
		root,
		Path(Type[benchmarkLocation](), Type[benchmarkContains](), Match(Type[benchmarkKind](), benchmarkID(5_000))),
	)
	defer closeQuery()
	target := actors[5_000]
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		Set(target, benchmarkID(5_000+i%2))
	}
	if Len(result) > 1 {
		b.Fatal("unexpected path query result")
	}
}

func BenchmarkPathQueryLinkUpdate10K(b *testing.B) {
	root, _, contains, actors := benchmarkPathWorld(10_000)
	result, closeQuery := Query(
		root,
		Path(Type[benchmarkLocation](), Type[benchmarkContains](), Match(Type[benchmarkKind](), benchmarkID(5_000))),
	)
	defer closeQuery()
	parent := contains[5_000]
	target := actors[5_000]
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		Unlink(parent, target)
		Link(parent, target)
	}
	if Len(result) != 1 {
		b.Fatal("unexpected path query result")
	}
}

func BenchmarkApplyDelta10K(b *testing.B) {
	const size = 10_000
	source := New(benchmarkKind{}, benchmarkID(-1), name("source"))
	target := New(benchmarkKind{}, benchmarkID(-1), name("target"))
	sourceNodes := make([]Graph, size)
	for i := range size {
		sourceNodes[i] = New(benchmarkKind{}, benchmarkID(i), benchmarkHealth(100))
		targetNode := New(benchmarkKind{}, benchmarkID(i), benchmarkHealth(100))
		Link(source, sourceNodes[i])
		Link(target, targetNode)
	}
	Commit(source)
	for i, sourceNode := range sourceNodes {
		Set(sourceNode, benchmarkHealth(100+i))
	}
	delta := Delta(source, Type[benchmarkKind](), Type[benchmarkID]())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		Apply(target, delta, Type[benchmarkKind](), Type[benchmarkID]())
	}
}
