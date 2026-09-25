package graph

import (
	"fmt"
	"testing"
)

type benchmarkKind struct{}
type benchmarkID int
type benchmarkHealth int

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
