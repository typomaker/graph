package graph

import (
	"bytes"
	"encoding/binary"
	"testing"
)

type wireEnergy int

func testWire() Wire {
	return NewWire(
		Define[kind]("kind"),
		Define[ident]("id"),
		Define[name]("name"),
		Define[health]("health"),
		Define[wireEnergy]("energy"),
	)
}

func wireReplica(source Graph) Graph {
	return Apply(Graph{}, Delta(source))
}

func TestWireAttributeRoundTripAndNonConsumingExport(t *testing.T) {
	wire := testWire()
	source := New(kind("root"), ident("root"), name("before"), health{10, 10})
	replica := wireReplica(source)
	Commit(source)
	Commit(replica)

	Set(source, name("after"))
	Set(source, health{7, 10})
	first, err := wire.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := wire.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("repeated export changed the update")
	}
	if err := wire.ImportUpdate(replica, first); err != nil {
		t.Fatal(err)
	}
	if value[name](t, replica) != "after" || value[health](t, replica) != (health{7, 10}) {
		t.Fatal("replacement was not imported")
	}

	Commit(source)
	Commit(replica)
	Unset[name](source)
	update, err := wire.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.ImportUpdate(replica, update); err != nil {
		t.Fatal(err)
	}
	if _, ok := Get[name](replica); ok {
		t.Fatal("unset was not imported")
	}
}

func TestWireStructuralRoundTrip(t *testing.T) {
	wire := testWire()
	source := New(kind("root"), ident("root"))
	old := New(kind("old"), ident("old"))
	Link(source, old)
	replica := wireReplica(source)
	Commit(source)
	Commit(replica)

	Unlink(source, old)
	branch := New(kind("branch"), ident("branch"))
	leaf := New(kind("leaf"), ident("leaf"), name("one"))
	Link(branch, leaf)
	Link(source, branch)
	Set(leaf, name("two"))
	update, err := wire.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.ImportUpdate(replica, update); err != nil {
		t.Fatal(err)
	}
	if !Empty(Search(replica, ident("old"))) || Len(Search(replica, ident("branch"), Path(ident("leaf")))) != 1 {
		t.Fatal("link, unlink, or new subtree was not imported")
	}
	if value[name](t, Search(replica, ident("leaf"))) != "two" {
		t.Fatal("multiple changes to a new node were not imported")
	}
}

func TestWireSpatialScope(t *testing.T) {
	wire := testWire()
	source := New(kind("root"), ident("root"))
	a := New(kind("branch"), ident("a"), name("old-a"))
	b := New(kind("branch"), ident("b"), name("old-b"))
	Link(source, a, b)
	replica := wireReplica(source)
	Commit(source)
	Commit(replica)
	Set(a, name("new-a"))
	Set(b, name("new-b"))

	aUpdate, err := wire.ExportUpdate(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.ImportUpdate(Search(replica, ident("a")), aUpdate); err != nil {
		t.Fatal(err)
	}
	if value[name](t, Search(replica, ident("a"))) != "new-a" || value[name](t, Search(replica, ident("b"))) != "old-b" {
		t.Fatal("subtree export escaped its scope")
	}

	rootUpdate, err := wire.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.ImportUpdate(replica, rootUpdate); err != nil {
		t.Fatal(err)
	}
	if value[name](t, Search(replica, ident("a"))) != "new-a" || value[name](t, Search(replica, ident("b"))) != "new-b" {
		t.Fatal("root export did not retain both branches")
	}
}

func TestWireIndependentlyCreatedReplicaIdentities(t *testing.T) {
	wire := testWire()
	source := New(kind("root"), ident("root"))
	sourceChild := New(kind("actor"), ident("actor"), health{10, 10})
	Link(source, sourceChild)
	_ = New(kind("shifts-process-local-identities"))
	replica := New(kind("root"), ident("root"))
	replicaChild := New(kind("actor"), ident("actor"), health{10, 10})
	Link(replica, replicaChild)
	Commit(source)
	Commit(replica)
	Set(sourceChild, health{5, 10})
	update, err := wire.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.ImportUpdate(replica, update); err != nil {
		t.Fatal(err)
	}
	if value[health](t, replicaChild) != (health{5, 10}) {
		t.Fatal("update depended on process-local node identities")
	}
}

func TestWireAdditiveSchemaEvolution(t *testing.T) {
	oldWire := NewWire(Define[kind]("kind"), Define[ident]("id"), Define[name]("name"))
	newWire := NewWire(Define[kind]("kind"), Define[ident]("id"), Define[name]("name"), Define[wireEnergy]("energy"))
	source := New(kind("root"), ident("root"), name("old"))
	replica := wireReplica(source)
	Commit(source)
	Commit(replica)
	Set(source, wireEnergy(42))
	Set(source, name("new"))
	update, err := newWire.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldWire.ImportUpdate(replica, update); err != nil {
		t.Fatal(err)
	}
	if value[name](t, replica) != "new" {
		t.Fatal("known record after unknown definition was not imported")
	}
	if _, ok := Get[wireEnergy](replica); ok {
		t.Fatal("old schema imported an unknown definition")
	}

	oldSource := New(kind("root"), ident("old-root"), name("before"))
	newReplica := wireReplica(oldSource)
	Commit(oldSource)
	Commit(newReplica)
	Set(oldSource, name("newer"))
	update, err = oldWire.ExportUpdate(oldSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := newWire.ImportUpdate(newReplica, update); err != nil {
		t.Fatal(err)
	}
	if value[name](t, newReplica) != "newer" {
		t.Fatal("new schema did not import an old-schema update")
	}
}

func TestWireErrorsAndSchemaValidation(t *testing.T) {
	assertPanic := func(name string, f func()) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			f()
		})
	}
	assertPanic("duplicate label", func() { NewWire(Define[kind]("x"), Define[name]("x")) })
	assertPanic("conflicting type", func() { NewWire(Define[kind]("x"), Define[kind]("y")) })
	assertPanic("zero definition", func() { NewWire(Definition{}) })

	wire := testWire()
	source := New(kind("root"), ident("root"))
	Commit(source)
	Set(source, name("changed"))
	update, err := wire.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	replica := New(kind("root"), ident("replica"))
	for _, data := range [][]byte{nil, update[:len(update)-1], append(append([]byte(nil), update...), 1)} {
		if err := wire.ImportUpdate(replica, data); err == nil {
			t.Fatal("corrupt update was accepted")
		}
	}
	unsupported := append([]byte(nil), update...)
	binary.BigEndian.PutUint16(unsupported[4:6], wireVersion+1)
	if err := wire.ImportUpdate(replica, unsupported); err == nil {
		t.Fatal("unsupported version was accepted")
	}
	if _, err := wire.ExportUpdate(Delta(source)); err == nil {
		t.Fatal("delta scope was accepted")
	}
	if _, err := NewWire(Define[kind]("kind")).ExportUpdate(source); err == nil {
		t.Fatal("incomplete schema was accepted")
	}
}

func TestWireUnknownStructuralNodeDoesNotFlattenTopology(t *testing.T) {
	type jetpack struct{}
	producer := NewWire(Define[kind]("kind"), Define[ident]("id"), Define[jetpack]("jetpack"))
	consumer := NewWire(Define[kind]("kind"), Define[ident]("id"))
	source := New(kind("root"), ident("root"))
	replica := wireReplica(source)
	Commit(source)
	Commit(replica)
	middle := New(jetpack{})
	leaf := New(kind("fuel"), ident("fuel"))
	Link(middle, leaf)
	Link(source, middle)
	update, err := producer.ExportUpdate(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.ImportUpdate(replica, update); err != nil {
		t.Fatal(err)
	}
	if !Empty(Search(replica, ident("fuel"))) {
		t.Fatal("known descendant of an unknown structural node was flattened")
	}
}
