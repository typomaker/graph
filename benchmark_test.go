package graph

import (
	"fmt"
	"testing"
)

type benchmarkKind string
type benchmarkID string
type benchmarkVersion int

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

func BenchmarkSearchAlternatingLargeCommitted10K(b *testing.B) {
	root := New(benchmarkKind("world"))
	for i := range 10000 {
		Link(root, New(benchmarkGroup(i%2)))
	}
	Commit(root)
	b.ResetTimer()
	for i := range b.N {
		if got := Len(Search(root, benchmarkGroup(i&1))); got != 5000 {
			b.Fatal(got)
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

func BenchmarkQueryBatchedMutations(b *testing.B) {
	for _, mutations := range []int{1000, 10000} {
		b.Run(fmt.Sprint(mutations), func(b *testing.B) {
			root := benchmarkWorld(10000)
			child := New(benchmarkKind("item"))
			Link(root, child)
			Commit(root)
			query, closeQuery := Query(root, Type[benchmarkVersion]())
			defer closeQuery()
			b.ResetTimer()
			for range b.N {
				for i := 0; i < mutations; i++ {
					Set(child, benchmarkVersion(i))
					Unset[benchmarkVersion](child)
				}
				Set(child, benchmarkVersion(mutations))
				count := 0
				for range query {
					count++
				}
				if count != 1 {
					b.Fatalf("query length = %d", count)
				}
			}
		})
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

func BenchmarkSelectCommitted10K(b *testing.B) {
	root := benchmarkWorld(10000)
	Commit(root)
	all := Search(root, Type[any]())
	b.ResetTimer()
	for range b.N {
		if Len(Select(all, benchmarkKind("actor"))) != 10000 {
			b.Fatal()
		}
	}
}

func BenchmarkSelectCommittedTraversal10K(b *testing.B) {
	root := benchmarkWorld(10000)
	Commit(root)
	all := Search(root, Type[any]())
	all.index = nil
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

func BenchmarkPathSearchAdded10K(b *testing.B) {
	root := New(benchmarkKind("root"))
	Commit(root)
	branch := benchmarkPathWorld(10000)
	Link(root, branch)
	expression := Path(benchmarkKind("contains"), And(benchmarkKind("actor"), benchmarkID("9999")))
	Search(root, benchmarkKind("location"), expression)
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

func BenchmarkLocalPathCommitted10K(b *testing.B) {
	root := New(benchmarkKind("root"))
	for i := 0; i < 10000; i++ {
		parent := New(benchmarkKind("parent"), benchmarkID(fmt.Sprint(i)))
		Link(root, parent)
		Link(parent, New(benchmarkKind("child")))
	}
	Commit(root)
	parent := Search(root, benchmarkID("9999"))
	path := Path(benchmarkKind("child"))
	b.Run("FollowOne", func(b *testing.B) {
		for range b.N {
			if Len(Follow(parent, path)) != 1 {
				b.Fatal()
			}
		}
	})
	b.Run("SelectOne", func(b *testing.B) {
		for range b.N {
			if Len(Select(parent, path)) != 1 {
				b.Fatal()
			}
		}
	})
	b.Run("FollowWide", func(b *testing.B) {
		parents := Search(root, benchmarkKind("parent"))
		b.ResetTimer()
		for range b.N {
			if Len(Follow(parents, path)) != 10000 {
				b.Fatal()
			}
		}
	})
}

func BenchmarkCommit10K(b *testing.B) {
	root := benchmarkWorld(10000)
	b.ResetTimer()
	for range b.N {
		Commit(root)
	}
}

func BenchmarkSetCommitted10K(b *testing.B) {
	root := benchmarkWorld(10000)
	child := Graph{nodes: []*node{first(root).childOrder[5000]}}
	Commit(root)
	b.ResetTimer()
	for i := range b.N {
		Set(child, benchmarkVersion(i&1))
	}
}

func BenchmarkBatchSetSearch10K(b *testing.B) {
	for _, changed := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("Changed%d", changed), func(b *testing.B) {
			root := benchmarkWorld(10000)
			children := make([]Graph, changed)
			for i, child := range first(root).childOrder[:changed] {
				children[i] = Graph{nodes: []*node{child}}
			}
			Commit(root)
			b.ResetTimer()
			for i := range b.N {
				value := benchmarkVersion(i & 1)
				for _, child := range children {
					Set(child, value)
				}
				if got := Len(Search(root, value)); got != changed {
					b.Fatalf("got %d, want %d", got, changed)
				}
			}
		})
	}
}

func BenchmarkDeltaOneOf10K(b *testing.B) {
	root := benchmarkWorld(10000)
	child := Graph{nodes: []*node{first(root).childOrder[5000]}}
	Commit(root)
	b.ResetTimer()
	for i := range b.N {
		Set(child, benchmarkVersion(i&1))
		if Empty(Delta(root)) {
			b.Fatal()
		}
	}
}

func BenchmarkDeltaUpdates10K(b *testing.B) {
	for _, changed := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("Changed%d", changed), func(b *testing.B) {
			root := benchmarkWorld(10000)
			children := first(root).childOrder[:changed]
			Commit(root)
			b.ResetTimer()
			for i := range b.N {
				for _, child := range children {
					Set(Graph{nodes: []*node{child}}, benchmarkVersion(i&1))
				}
				if Empty(Delta(root)) {
					b.Fatal()
				}
			}
		})
	}
}

func BenchmarkCommitUpdates10K(b *testing.B) {
	for _, changed := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("Changed%d", changed), func(b *testing.B) {
			root := benchmarkWorld(10000)
			children := first(root).childOrder[:changed]
			Commit(root)
			b.ResetTimer()
			for i := range b.N {
				for _, child := range children {
					Set(Graph{nodes: []*node{child}}, benchmarkVersion(i&1))
				}
				Commit(root)
			}
		})
	}
}

func BenchmarkCommitStructuralLeaf10K(b *testing.B) {
	root := benchmarkWorld(10000)
	leaf := New(benchmarkKind("leaf"))
	Commit(root)
	linked := false
	b.ResetTimer()
	for range b.N {
		if linked {
			Unlink(root, leaf)
		} else {
			Link(root, leaf)
		}
		Commit(root)
		linked = !linked
	}
}

func BenchmarkDeltaDeep10K(b *testing.B) {
	nodes := make([]Graph, 10000)
	for i := range nodes {
		nodes[i] = New(benchmarkKind("node"))
	}
	for i := len(nodes) - 2; i >= 0; i-- {
		Link(nodes[i], nodes[i+1])
	}
	root := nodes[0]
	leaf := nodes[len(nodes)-1]
	Commit(root)
	b.ResetTimer()
	for i := range b.N {
		Set(leaf, benchmarkVersion(i&1))
		if Empty(Delta(root)) {
			b.Fatal()
		}
	}
}

func BenchmarkDeltaShared100(b *testing.B) {
	root := New(benchmarkKind("root"))
	shared := New(benchmarkKind("shared"))
	for range 100 {
		parent := New(benchmarkKind("parent"))
		Link(root, parent)
		Link(parent, shared)
	}
	Commit(root)
	b.ResetTimer()
	for i := range b.N {
		Set(shared, benchmarkVersion(i&1))
		if Empty(Delta(root)) {
			b.Fatal()
		}
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
	root, branch := benchmarkStructuralBranch100Of10K()
	b.ResetTimer()
	for range b.N {
		Link(root, branch)
		Unlink(root, branch)
	}
}

func benchmarkStructuralBranch100Of10K() (Graph, Graph) {
	root := benchmarkWorld(10000)
	branch := New(benchmarkKind("branch"))
	current := branch
	for range 99 {
		next := New(benchmarkKind("branch"))
		Link(current, next)
		current = next
	}
	Commit(root)
	return root, branch
}

func BenchmarkStructuralLinkBranch100Of10K(b *testing.B) {
	root, branch := benchmarkStructuralBranch100Of10K()
	b.StopTimer()
	for range b.N {
		b.StartTimer()
		Link(root, branch)
		b.StopTimer()
		Unlink(root, branch)
	}
}

func BenchmarkStructuralUnlinkBranch100Of10K(b *testing.B) {
	root, branch := benchmarkStructuralBranch100Of10K()
	b.StopTimer()
	for range b.N {
		Link(root, branch)
		b.StartTimer()
		Unlink(root, branch)
		b.StopTimer()
	}
}

func BenchmarkSearchAddedBranch100Of10K(b *testing.B) {
	root := benchmarkWorld(10000)
	branch := New(benchmarkKind("branch"))
	for i := 0; i < 99; i++ {
		Link(branch, New(benchmarkKind("branch"), benchmarkID(fmt.Sprint(i))))
	}
	Commit(root)
	Link(root, branch)
	Search(root, benchmarkKind("actor")) // Rebuild traversal order outside timing.
	b.ResetTimer()
	for range b.N {
		Search(root, benchmarkKind("branch"))
	}
}

func BenchmarkSearchAddedBranch10KOf10K(b *testing.B) {
	root := benchmarkWorld(10000)
	branch := New(benchmarkKind("branch"))
	for i := 0; i < 9999; i++ {
		Link(branch, New(benchmarkKind("branch"), benchmarkID(fmt.Sprint(i))))
	}
	Commit(root)
	Link(root, branch)
	Search(root, benchmarkKind("actor"))
	b.ResetTimer()
	for range b.N {
		Search(root, benchmarkKind("branch"))
	}
}

func BenchmarkSearchAfterApply10K(b *testing.B) {
	source := benchmarkWorld(10000)
	replica := Apply(Graph{}, Delta(source))
	Commit(source)
	Commit(replica)
	Link(source, New(benchmarkKind("actor"), benchmarkID("added")))
	Apply(replica, Delta(source))
	Search(replica, benchmarkKind("actor"))
	b.ResetTimer()
	for range b.N {
		if Len(Search(replica, benchmarkKind("actor"))) != 10001 {
			b.Fatal()
		}
	}
}

func BenchmarkSearchAfterPatch10K(b *testing.B) {
	root := benchmarkWorld(10000)
	Set(root, benchmarkID("root"))
	Commit(root)
	patch := New(benchmarkID("root"))
	Link(patch, New(benchmarkKind("actor"), benchmarkID("added")))
	Patch(root, patch, Type[benchmarkID]())
	Search(root, benchmarkKind("actor"))
	b.ResetTimer()
	for range b.N {
		if Len(Search(root, benchmarkKind("actor"))) != 10001 {
			b.Fatal()
		}
	}
}

func BenchmarkApplyDeltaToCommitted10K(b *testing.B) {
	source := benchmarkWorld(10000)
	replica := Apply(Graph{}, Delta(source))
	Commit(source)
	Commit(replica)
	leaf := New(benchmarkKind("actor"), benchmarkID("toggle"))
	linked := false
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		if linked {
			Unlink(source, leaf)
		} else {
			Link(source, leaf)
		}
		changes := Delta(source)
		Commit(source)
		linked = !linked
		b.StartTimer()
		Apply(replica, changes)
	}
}

func BenchmarkWireExport100Of10K(b *testing.B) {
	wire := NewWire(Define[benchmarkKind]("kind"), Define[benchmarkID]("id"))
	root := New(benchmarkKind("root"), benchmarkID("root"))
	nodes := make([]Graph, 10_000)
	for i := range nodes {
		nodes[i] = New(benchmarkKind("node"), benchmarkID(fmt.Sprint(i)))
		Link(root, nodes[i])
	}
	Commit(root)
	for i := range 100 {
		Set(nodes[i], benchmarkKind("changed"))
	}
	delta := Delta(root)
	b.Run("Graph", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := wire.ExportUpdate(root); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("PrecomputedDelta", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := wire.ExportUpdate(delta); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkWireImport100Of10K(b *testing.B) {
	wire := NewWire(Define[benchmarkKind]("kind"), Define[benchmarkID]("id"))
	source := New(benchmarkKind("root"), benchmarkID("root"))
	nodes := make([]Graph, 10_000)
	for i := range nodes {
		nodes[i] = New(benchmarkKind("node"), benchmarkID(fmt.Sprint(i)))
		Link(source, nodes[i])
	}
	replica := Apply(Graph{}, Delta(source))
	Commit(source)
	Commit(replica)
	for i := range 100 {
		Set(nodes[i], benchmarkKind("changed"))
	}
	update, err := wire.ExportUpdate(source)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := wire.ImportUpdate(replica, update); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPatchAttributeCommitted10K(b *testing.B) {
	root := benchmarkWorld(10000)
	Set(root, benchmarkID("root"))
	Commit(root)
	patches := [2]Graph{
		New(benchmarkID("root"), benchmarkKind("first")),
		New(benchmarkID("root"), benchmarkKind("second")),
	}
	b.ResetTimer()
	for i := range b.N {
		Patch(root, patches[i%2], Type[benchmarkID]())
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
