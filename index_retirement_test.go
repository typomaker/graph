package graph

import "testing"

func TestExactSelectionAfterCommittedNodeRetirement(t *testing.T) {
	root := New(memoryIdentity(0))
	retired := New(memoryIdentity(1))
	live := New(memoryIdentity(2), memoryText("live"))
	neighbor := New(memoryIdentity(3))
	Link(root, retired, live, neighbor)
	Commit(root)
	selection := Search(root, memoryIdentity(2))
	Unlink(root, retired)
	Commit(root)
	for candidate := range Each(Select(Unnest(root), Type[memoryIdentity]())) {
		if value, _ := Get[memoryIdentity](candidate); value == 2 && Len(Select(candidate, value)) != 1 {
			t.Fatal("indexed candidate failed to match its own identity")
		}
	}
	if value, ok := Get[memoryIdentity](live); !ok || value != 2 {
		t.Fatal("live attribute changed")
	}
	for _, candidate := range []Graph{live, selection, Select(Unnest(root), memoryText("live"))} {
		if Len(Select(candidate, memoryIdentity(2))) != 1 {
			t.Fatal("live selection failed to match its own identity")
		}
	}
	if Len(Search(root, memoryIdentity(2))) != 1 || Len(Search(root, memoryIdentity(3))) != 1 {
		t.Fatal("retirement corrupted exact value postings")
	}
	if value, _ := Get[memoryIdentity](Search(root, memoryIdentity(2))); value != 2 {
		t.Fatal("exact value search returned a neighboring node")
	}
	Link(root, retired)
	Commit(root)
	if Len(Search(root, memoryIdentity(1))) != 1 || Len(Search(root, memoryIdentity(2))) != 1 {
		t.Fatal("relink corrupted exact value postings")
	}
}

func TestPreparedSelectionSelfMatchesAfterDetachAndRelink(t *testing.T) {
	root := New(memoryIdentity(0))
	child := New(memoryIdentity(1))
	Link(root, child)
	Commit(root)
	selection := Search(root, memoryIdentity(1))
	Unlink(root, child)
	Commit(root)
	if Len(Select(selection, memoryIdentity(1))) != 1 {
		t.Fatal("externally retained detached selection lost self-match")
	}
	Link(root, New(memoryIdentity(2)), child)
	Commit(root)
	if Len(Select(selection, memoryIdentity(1))) != 1 {
		t.Fatal("prepared selection lost self-match after ordinal changed")
	}
}
