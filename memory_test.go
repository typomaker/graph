package graph

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"weak"
)

type memoryIdentity int
type memoryText string

func TestCommitReleasesDetachedAddedNodes(t *testing.T) {
	root := New(memoryIdentity(0))
	Commit(root)
	rootNode := first(root)
	index := rootNode.index
	for i := range 320 {
		child := New(memoryIdentity(i+1), memoryText(strings.Repeat("payload", 4096)+strconv.Itoa(i)))
		Link(root, child)
		Commit(root)
		if !Unlink(root, child) {
			t.Fatal("failed to unlink child")
		}
		if len(rootNode.removedChildren) != 1 {
			t.Fatal("missing pre-commit removal snapshot")
		}
		Commit(root)
		if len(rootNode.removedChildren) != 0 || len(rootNode.dirtyChildOrder) != 0 || len(index.added) != 0 || len(index.addedOrdinal) != 0 || len(index.byKey) != 1 {
			t.Fatalf("commit retained removed node at iteration %d", i)
		}
		if rootNode.dirtyChildOrder != nil && rootNode.dirtyChildOrder[:cap(rootNode.dirtyChildOrder)][0] != nil {
			t.Fatal("dirty child backing array retained removed node")
		}
	}
}

func TestCommitRetainsPreparedDetachedDelta(t *testing.T) {
	root := New(memoryIdentity(0))
	child := New(memoryIdentity(1), memoryText("payload"))
	Link(root, child)
	Commit(root)
	Unlink(root, child)
	prepared := Delta(root)
	if Empty(Detached(prepared)) {
		t.Fatal("missing detached snapshot before commit")
	}
	Commit(root)
	if Empty(Detached(prepared)) || !Empty(Detached(root)) {
		t.Fatal("prepared snapshot or committed state is wrong")
	}
}

func TestCommitRetiresOriginalIndexNodeAndRelinks(t *testing.T) {
	root := New(memoryIdentity(0))
	child := New(memoryIdentity(1), memoryText("payload"))
	Link(root, child)
	Commit(root)
	index := first(root).index
	Set(child, memoryText("changed"))
	Unlink(root, child)
	Commit(root)
	if len(index.nodes) != 2 || index.nodes[1] != nil || len(index.byValue[attributeType[memoryText]()]) != 0 {
		t.Fatal("original index retained detached payload")
	}
	if !Empty(Search(root, memoryText("payload"))) {
		t.Fatal("detached child remained searchable")
	}
	Link(root, child)
	Commit(root)
	if Empty(Search(root, memoryText("changed"))) {
		t.Fatal("relinked child was not searchable")
	}
}

func TestRemoveKeyNodeCollisionChain(t *testing.T) {
	a := first(New(memoryIdentity(1)))
	b := first(New(memoryIdentity(2)))
	c := first(New(memoryIdentity(3)))
	b.key, c.key = a.key, a.key
	index := &searchIndex{byKey: make(map[uint64]*node), sameKey: make(map[*node]*node)}
	index.addKeyNode(a)
	index.addKeyNode(b)
	index.addKeyNode(c)
	index.removeKeyNode(b)
	if index.byKey[a.key] != c || index.sameKey[c] != a {
		t.Fatal("middle collision removal broke chain")
	}
	index.removeKeyNode(c)
	if index.byKey[a.key] != a {
		t.Fatal("head collision removal broke chain")
	}
	index.removeKeyNode(a)
	if index.byKey[a.key] != nil {
		t.Fatal("last collision removal retained node")
	}
}

func TestCommitDetachedSharedChild(t *testing.T) {
	root := New(memoryIdentity(0))
	left := New(memoryIdentity(1))
	right := New(memoryIdentity(2))
	shared := New(memoryIdentity(3), memoryText("shared"))
	Link(root, left, right)
	Link(left, shared)
	Link(right, shared)
	Commit(root)
	Unlink(left, shared)
	Commit(root)
	if Empty(Search(root, memoryText("shared"))) || first(root).index.nodes[first(root).index.ordinal[first(shared)]] == nil {
		t.Fatal("shared child was retired while still reachable")
	}
	Unlink(right, shared)
	Commit(root)
	if !Empty(Search(root, memoryText("shared"))) {
		t.Fatal("detached shared child remained searchable")
	}
}

func TestCommitDetachedAcrossRoots(t *testing.T) {
	left := New(memoryIdentity(0))
	right := New(memoryIdentity(1))
	child := New(memoryIdentity(2), memoryText("shared"))
	Link(left, child)
	Link(right, child)
	Commit(left)
	Commit(right)
	Unlink(left, child)
	Commit(left)
	if !Empty(Search(left, memoryText("shared"))) || Empty(Search(right, memoryText("shared"))) {
		t.Fatal("commit changed reachability under another root")
	}
}

func TestCommitReleasesAddedHole(t *testing.T) {
	root := New(memoryIdentity(0))
	Commit(root)
	retired := New(memoryIdentity(1), memoryText("retired"))
	live := New(memoryIdentity(2), memoryText("live"))
	Link(root, retired, live)
	Commit(root)
	index := first(root).index
	Unlink(root, retired)
	Commit(root)
	if index.added[0] != nil || len(index.added) != 2 || len(index.addedOrdinal) != 1 {
		t.Fatal("inactive interior added slot retained node")
	}
	if !Empty(Search(root, memoryText("retired"))) || Empty(Search(root, memoryText("live"))) {
		t.Fatal("search failed with retired interior slot")
	}
	Link(root, retired)
	Commit(root)
	if Empty(Search(root, memoryText("retired"))) {
		t.Fatal("relinked interior node is missing")
	}
}

func TestCommitRetiredParentRemovesOriginalParentPosting(t *testing.T) {
	root := New(memoryIdentity(0))
	oldParent := New(memoryIdentity(1))
	newParent := New(memoryIdentity(2))
	child := New(memoryIdentity(3), memoryText("child"))
	Link(root, oldParent, newParent)
	Link(oldParent, child)
	Commit(root)
	index := first(root).index
	oldOrdinal := index.ordinal[first(oldParent)]
	Link(newParent, child)
	Unlink(oldParent, child)
	Unlink(root, oldParent)
	Commit(root)
	childOrdinal := index.ordinal[first(child)]
	if index.nodes[oldOrdinal] != nil || postingHas(index.parents[childOrdinal], oldOrdinal) {
		t.Fatal("retired parent remained in original parent posting")
	}
	if Empty(Search(root, memoryText("child"))) {
		t.Fatal("child moved to another parent was lost")
	}
}

func TestCommitReleasesIndexedAddedHoleAndRefreshesQuery(t *testing.T) {
	root := New(memoryIdentity(0))
	Commit(root)
	children := make([]Graph, 40)
	for i := range children {
		children[i] = New(memoryIdentity(i+1), memoryText(strconv.Itoa(i)))
		Link(root, children[i])
	}
	Commit(root)
	index := first(root).index
	if !index.addedIndexed {
		t.Fatal("expected indexed added nodes")
	}
	query, closeQuery := Query(root, memoryText("0"))
	defer closeQuery()
	if countQuery(query) != 1 {
		t.Fatal("query missed initial node")
	}
	Unlink(root, children[0])
	Set(children[0], memoryText("changed after detach"))
	Commit(root)
	if index.added[0] != nil || countQuery(query) != 0 || !Empty(Search(root, memoryText("0"))) {
		t.Fatal("retired indexed node remained in query or search")
	}
	if Empty(Search(root, memoryText("39"))) {
		t.Fatal("live indexed neighbor disappeared")
	}
	closeQuery()
	if countQuery(query) != 0 {
		t.Fatal("closed query returned nodes")
	}
}

func TestCommitReleasesAddedParentWithSharedChild(t *testing.T) {
	root := New(memoryIdentity(0))
	shared := New(memoryIdentity(1), memoryText("shared"))
	Link(root, shared)
	Commit(root)
	parent := New(memoryIdentity(2), memoryText("retired parent"))
	Link(parent, shared)
	Link(root, parent)
	Commit(root)
	index := first(root).index
	Unlink(root, parent)
	Commit(root)
	if len(index.committedAdded) != 0 || len(index.committedRemoved) != 0 || len(index.addedOrdinal) != 0 {
		t.Fatal("edge overlay retained detached added parent")
	}
	if Empty(Search(root, memoryText("shared"))) {
		t.Fatal("shared child was lost")
	}
}

func countQuery(sequence func(func(Graph) bool)) int {
	count := 0
	for range sequence {
		count++
	}
	return count
}

func BenchmarkCommitDetachedChurn(b *testing.B) {
	for _, size := range []int{10, 10000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			root := New(memoryIdentity(0))
			for i := range size {
				Link(root, New(memoryIdentity(i+1)))
			}
			Commit(root)
			b.ResetTimer()
			for i := range b.N {
				child := New(memoryIdentity(size+i+1), memoryText("payload"))
				Link(root, child)
				Commit(root)
				Unlink(root, child)
				Commit(root)
			}
		})
	}
}

func TestCommitDetachedHeapBound(t *testing.T) {
	root := New(memoryIdentity(0))
	Commit(root)
	detach := func(i int) {
		child := New(memoryIdentity(i+1), memoryText(strings.Repeat("detached-node-", 1<<14)+strconv.Itoa(i)))
		Link(root, child)
		Commit(root)
		Unlink(root, child)
		Commit(root)
	}
	for i := range 8 {
		detach(i)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range 64 {
		detach(i + 8)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(root)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 8<<20 {
		t.Fatalf("detached payloads retained %d bytes", retained)
	}
}

func TestCommitReleasesDetachedParentsOfLiveNode(t *testing.T) {
	root := New(memoryIdentity(0))
	shared := New(memoryIdentity(1))
	Link(root, shared)
	Commit(root)
	detach := func(i int) weak.Pointer[node] {
		parent := New(memoryIdentity(i+2), memoryText(strings.Repeat("detached-node-", 1<<14)+strconv.Itoa(i)))
		Link(parent, shared)
		Link(root, parent)
		Commit(root)
		Unlink(root, parent)
		Commit(root)
		return weak.Make(first(parent))
	}
	for i := range 8 {
		detach(i)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var last weak.Pointer[node]
	for i := range 320 {
		last = detach(i + 8)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if last.Value() != nil {
		t.Fatal("shared child retained detached parent")
	}
	runtime.KeepAlive(root)
	runtime.KeepAlive(shared)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 8<<20 {
		t.Fatalf("detached parents retained %d bytes", retained)
	}
}

func TestExternallyHeldDetachedParentReceivesChildChanges(t *testing.T) {
	root := New(memoryIdentity(0))
	shared := New(memoryIdentity(1), memoryText("before"))
	parent := New(memoryIdentity(2))
	Link(root, shared, parent)
	Link(parent, shared)
	Commit(root)
	Unlink(root, parent)
	Commit(root)
	Set(shared, memoryText("after"))
	if Empty(Delta(parent)) || Empty(Search(parent, memoryText("after"))) {
		t.Fatal("externally held parent missed a child change")
	}
	Commit(parent)
	Set(shared, memoryText("again"))
	if Empty(Delta(parent)) || Empty(Search(parent, memoryText("again"))) {
		t.Fatal("externally held parent missed a change after its own commit")
	}
	runtime.KeepAlive(parent)
}
