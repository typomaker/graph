package graph

import (
	"fmt"
	"testing"
)

type benchmarkGroup int

func benchmarkIndexedWorld(size int) Graph {
	root := New(benchmarkKind("world"))
	for i := range size {
		Link(root, New(
			benchmarkKind("actor"),
			benchmarkID(fmt.Sprint(i)),
			benchmarkGroup(i%10),
		))
	}
	return root
}

// BenchmarkSearchIndexDiagnostics separates posting lookup, result ordering,
// and public Search materialization so profiles identify the expensive layer.
func BenchmarkSearchIndexDiagnostics(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("N%d", size), func(b *testing.B) {
			root := benchmarkIndexedWorld(size)
			Commit(root)
			index := first(root).index
			exactID := benchmarkID(fmt.Sprint(size / 2))
			exact := predicateFromValue(exactID)
			common := predicateFromValue(benchmarkGroup(5))

			b.Run("Posting/Exact", func(b *testing.B) {
				for range b.N {
					if matches := index.matchCurrent(exact); len(matches) != 1 {
						b.Fatal(len(matches))
					}
				}
			})
			b.Run("Posting/Common10Percent", func(b *testing.B) {
				for range b.N {
					if matches := index.matchCurrent(common); len(matches) != size/10 {
						b.Fatal(len(matches))
					}
				}
			})
			b.Run("Order/Exact", func(b *testing.B) {
				matches := index.matchCurrent(exact)
				for range b.N {
					if ordered := index.orderMatches(matches); len(ordered) != 1 {
						b.Fatal(len(ordered))
					}
				}
			})
			b.Run("Search/Exact", func(b *testing.B) {
				for range b.N {
					if result := Search(root, exactID); Len(result) != 1 {
						b.Fatal(Len(result))
					}
				}
			})
			b.Run("Search/Common10Percent", func(b *testing.B) {
				for range b.N {
					if result := Search(root, benchmarkGroup(5)); Len(result) != size/10 {
						b.Fatal(Len(result))
					}
				}
			})
			b.Run("Search/AllByType", func(b *testing.B) {
				for range b.N {
					if result := Search(root, Type[benchmarkKind]()); Len(result) != size+1 {
						b.Fatal(Len(result))
					}
				}
			})
		})
	}
}

// BenchmarkSearchPredicateDiagnostics exposes posting intersection costs for
// selective and non-selective logical expressions.
func BenchmarkSearchPredicateDiagnostics(b *testing.B) {
	root := benchmarkIndexedWorld(10000)
	Commit(root)
	cases := []struct {
		name      string
		condition any
		want      int
	}{
		{"Exact", benchmarkID("5000"), 1},
		{"Missing", benchmarkID("missing"), 0},
		{"AndCommonExact", And(benchmarkGroup(0), benchmarkID("5000")), 1},
		{"AndCommonMissing", And(benchmarkGroup(0), benchmarkID("missing")), 0},
		{"OrTwoCommon", Or(benchmarkGroup(0), benchmarkGroup(1)), 2000},
		{"TypeAll", Type[benchmarkKind](), 10001},
	}
	for _, benchmark := range cases {
		b.Run(benchmark.name, func(b *testing.B) {
			for range b.N {
				if result := Search(root, benchmark.condition); Len(result) != benchmark.want {
					b.Fatal(Len(result))
				}
			}
		})
	}
}

// BenchmarkSearchOverlayDiagnostics shows how dirty-node cardinality affects
// indexed searches without including mutation setup in the measurement.
func BenchmarkSearchOverlayDiagnostics(b *testing.B) {
	for _, dirty := range []int{0, 1, 100, 1000} {
		b.Run(fmt.Sprintf("Dirty%d", dirty), func(b *testing.B) {
			root := benchmarkIndexedWorld(10000)
			Commit(root)
			for i := range dirty {
				child := Graph{nodes: []*node{first(root).childOrder[i]}}
				Set(child, benchmarkGroup(99))
			}
			want := Len(Search(root, benchmarkGroup(0)))
			b.ResetTimer()
			for range b.N {
				if result := Search(root, benchmarkGroup(0)); Len(result) != want {
					b.Fatalf("got %d, want %d", Len(result), want)
				}
			}
		})
	}
}

// BenchmarkSearchPathScaling contrasts forward traversal with the committed
// reverse-path index at increasing graph sizes.
func BenchmarkSearchPathScaling(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		for _, committed := range []bool{false, true} {
			name := fmt.Sprintf("N%d/Uncommitted", size)
			if committed {
				name = fmt.Sprintf("N%d/Committed", size)
			}
			b.Run(name, func(b *testing.B) {
				root := benchmarkPathWorld(size)
				if committed {
					Commit(root)
				}
				expression := Path(benchmarkKind("contains"), And(
					benchmarkKind("actor"),
					benchmarkID(fmt.Sprint(size-1)),
				))
				b.ResetTimer()
				for range b.N {
					if result := Search(root, benchmarkKind("location"), expression); Len(result) != 1 {
						b.Fatal(Len(result))
					}
				}
			})
		}
	}
}

// BenchmarkSearchIndexBuild measures Commit separately from graph creation.
func BenchmarkSearchIndexBuild(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("N%d", size), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				root := benchmarkIndexedWorld(size)
				b.StartTimer()
				Commit(root)
			}
		})
	}
}
