package graph

import "testing"

type actor struct{}
type health struct{ Current, Max int }
type name string
type entityID string
type entityKind string

type location struct{}
type contains struct{}

func TestAttributesAndSelections(t *testing.T) {
	a := New(actor{}, health{10, 10})
	b := New(actor{}, health{5, 10})
	if Len(Merge(a, b, a)) != 2 {
		t.Fatal("merge")
	}
	var h health
	if Empty(Get(a, &h)) || h.Current != 10 {
		t.Fatal("get")
	}
	if !Empty(Set(a, health{10, 10})) {
		t.Fatal("equal set changed")
	}
	if Empty(Set(a, health{9, 10})) {
		t.Fatal("set did not change")
	}
	if Empty(Unset(a, Type[health]())) || !Empty(Get(a, Type[health]())) {
		t.Fatal("unset")
	}
}

func TestDAGQueryAndCommit(t *testing.T) {
	root := New(name("root"))
	a := New(actor{})
	child := New(name("child"))
	Link(root, a)
	Link(a, child)
	q, close := Query(root, Type[actor]())
	if Len(q) != 1 {
		t.Fatal("bootstrap")
	}
	b := New(actor{})
	Link(root, b)
	if Len(q) != 2 {
		t.Fatal("link update")
	}
	Unset(b, Type[actor]())
	if Len(q) != 1 {
		t.Fatal("attribute update")
	}
	Set(b, actor{})
	if Len(q) != 2 {
		t.Fatal("attribute insertion update")
	}
	close()
	Unset(b, Type[actor]())
	if Len(q) != 2 {
		t.Fatal("closed query changed")
	}
	Commit(root)
	Set(child, health{1, 2})
	delta := Delta(root)
	if Len(delta) != 1 || Len(At(delta)) != 1 || Len(At(At(delta))) != 1 {
		t.Fatal("pruned delta path")
	}
	if Empty(Delta(root)) {
		t.Fatal("delta advanced the baseline")
	}
	Commit(root)
	if !Empty(Delta(root)) {
		t.Fatal("delta after commit not empty")
	}
}

func TestCyclePanics(t *testing.T) {
	a := New(name("a"))
	b := New(name("b"))
	Link(a, b)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	Link(b, a)
}

func TestQueryStructuralUpdatesPreserveMultipleParentReachability(t *testing.T) {
	root := New(name("query-root"))
	left := New(name("left"))
	right := New(name("right"))
	target := New(actor{})
	Link(root, left, right)
	Link(left, target)
	Link(right, target)
	result, closeResult := Query(root, Type[actor]())
	defer closeResult()
	if Len(result) != 1 {
		t.Fatalf("bootstrap length=%d", Len(result))
	}
	Unlink(left, target)
	if Len(result) != 1 {
		t.Fatal("node disappeared while still reachable through second parent")
	}
	Unlink(right, target)
	if !Empty(result) {
		t.Fatal("unreachable node remains in query")
	}
	Link(left, target)
	if Len(result) != 1 {
		t.Fatal("relinked subtree was not added incrementally")
	}
}

func TestPathQueryMatchesImmediateStructuralChain(t *testing.T) {
	world := New(name("world"))
	firstLocation := New(location{}, name("first"))
	secondLocation := New(location{}, name("second"))
	firstContains := New(contains{})
	secondContains := New(contains{})
	target := New(actor{}, entityID("actor-1"))
	other := New(actor{}, entityID("actor-2"))
	Link(world, firstLocation, secondLocation)
	Link(firstLocation, firstContains)
	Link(secondLocation, secondContains)
	Link(firstContains, target)
	Link(secondContains, other)

	locations, closeLocations := Query(
		world,
		Path(
			Type[location](),
			Type[contains](),
			Match(Type[actor](), entityID("actor-1")),
		),
	)
	defer closeLocations()

	if Len(locations) != 1 || first(locations) != first(firstLocation) {
		t.Fatal("path query did not return the matching path start")
	}

	intermediate := New(name("intermediate"))
	Unlink(firstContains, target)
	Link(firstContains, intermediate)
	Link(intermediate, target)
	if !Empty(locations) {
		t.Fatal("path query skipped a non-matching intermediate node")
	}
}

func TestPathQueryTracksAttributeAndRelationChanges(t *testing.T) {
	world := New(name("world"))
	place := New(location{})
	relation := New(contains{})
	target := New(actor{}, entityID("other"))
	Link(world, place)
	Link(place, relation)
	Link(relation, target)

	locations, closeLocations := Query(
		world,
		Path(Type[location](), Type[contains](), Match(Type[actor](), entityID("actor-1"))),
	)
	if !Empty(locations) {
		t.Fatal("path query matched the wrong endpoint")
	}

	Set(target, entityID("actor-1"))
	if Len(locations) != 1 {
		t.Fatal("endpoint attribute update did not add the path start")
	}
	Set(target, health{Current: 1, Max: 1})
	if Len(locations) != 1 {
		t.Fatal("unrelated attribute update changed the path result")
	}
	Unset(relation, Type[contains]())
	if !Empty(locations) {
		t.Fatal("intermediate attribute removal did not remove the path start")
	}
	Set(relation, contains{})
	if Len(locations) != 1 {
		t.Fatal("intermediate attribute insertion did not restore the path start")
	}
	Unlink(place, relation)
	if !Empty(locations) {
		t.Fatal("relation removal did not remove the path start")
	}
	Link(place, relation)
	if Len(locations) != 1 {
		t.Fatal("relation insertion did not restore the path start")
	}

	closeLocations()
	Unset(target, Type[actor]())
	if Len(locations) != 1 {
		t.Fatal("closed path query changed")
	}
}

func TestPathAndMatchValidationPanics(t *testing.T) {
	tests := []struct {
		name string
		fn   func()
	}{
		{"empty match", func() { Match() }},
		{"empty path", func() { Path() }},
		{"nested path", func() { Path(Path(Type[actor]())) }},
		{"mixed query expression", func() { Query(New(actor{}), Path(Type[actor]()), Type[actor]()) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			tt.fn()
		})
	}
}

func TestMatchCanGroupAWholeQuery(t *testing.T) {
	root := New(name("root"))
	target := New(actor{}, entityID("actor-1"))
	Link(root, target)

	result, closeResult := Query(root, Match(Type[actor](), entityID("actor-1")))
	defer closeResult()
	if Len(result) != 1 || first(result) != first(target) {
		t.Fatal("top-level match did not preserve query semantics")
	}

	Set(target, health{Current: 1, Max: 1})
	if Len(result) != 1 {
		t.Fatal("unrelated attribute update changed a grouped query")
	}
}

func TestPathQueryCountsMultipleSupportingChildren(t *testing.T) {
	world := New(name("world"))
	place := New(location{})
	firstRelation := New(contains{})
	secondRelation := New(contains{})
	target := New(actor{}, entityID("actor-1"))
	Link(world, place)
	Link(place, firstRelation, secondRelation)
	Link(firstRelation, target)
	Link(secondRelation, target)

	locations, closeLocations := Query(
		world,
		Path(Type[location](), Type[contains](), Match(Type[actor](), entityID("actor-1"))),
	)
	defer closeLocations()
	if Len(locations) != 1 {
		t.Fatal("multiple supporting paths did not produce one result")
	}

	Unlink(firstRelation, target)
	if Len(locations) != 1 {
		t.Fatal("removing one supporting path removed the result")
	}
	Unlink(secondRelation, target)
	if !Empty(locations) {
		t.Fatal("result remained after removing every supporting path")
	}
}

func TestPathQueryIndexesNewlyReachableSubtree(t *testing.T) {
	world := New(name("world"))
	place := New(location{})
	relation := New(contains{})
	target := New(actor{}, entityID("actor-1"))
	Link(place, relation)
	Link(relation, target)

	locations, closeLocations := Query(
		world,
		Path(Type[location](), Type[contains](), Match(Type[actor](), entityID("actor-1"))),
	)
	defer closeLocations()
	if !Empty(locations) {
		t.Fatal("detached subtree unexpectedly matched")
	}

	Link(world, place)
	if Len(locations) != 1 {
		t.Fatal("newly reachable subtree was not indexed")
	}
	Unlink(world, place)
	if !Empty(locations) {
		t.Fatal("detached subtree remained in the path index")
	}
}

func TestPathQueryLinksAlreadyReachableNodes(t *testing.T) {
	world := New(name("world"))
	place := New(location{})
	relation := New(contains{})
	target := New(actor{}, entityID("actor-1"))
	Link(world, place, relation, target)

	locations, closeLocations := Query(
		world,
		Path(Type[location](), Type[contains](), Match(Type[actor](), entityID("actor-1"))),
	)
	defer closeLocations()

	Link(place, relation)
	Link(relation, target)
	if Len(locations) != 1 {
		t.Fatal("links between reachable nodes did not update the path index")
	}
	Unlink(place, relation)
	if !Empty(locations) {
		t.Fatal("unlink between reachable nodes did not update the path index")
	}
}

func TestDetachedNodeRemainsUsableAndCanBeRelinked(t *testing.T) {
	root := New(name("root"))
	child := New(actor{}, name("child"))
	Link(root, child)
	result, closeResult := Query(root, Type[actor]())
	defer closeResult()

	Unlink(root, child)
	if !Empty(result) {
		t.Fatal("detached node remained in parent query")
	}
	if Empty(Get(child, Type[actor]())) {
		t.Fatal("user-held detached node became invalid")
	}
	detachedResult, closeDetached := Query(child, Type[actor]())
	if Len(detachedResult) != 1 {
		t.Fatal("detached node cannot be queried")
	}
	closeDetached()

	Link(root, child)
	if Len(result) != 1 {
		t.Fatal("relinked node did not return to parent query")
	}
}

func TestApplyBootstrapsAndUpdatesReplica(t *testing.T) {
	source := New(name("world"), health{10, 10})
	actorNode := New(actor{}, name("actor"))
	item := New(name("item"))
	Link(source, actorNode)
	Link(actorNode, item)

	replica := Apply(Graph{}, Delta(source))
	Commit(source)
	if Len(replica) != 1 || Len(At(replica)) != 1 || Len(At(At(replica))) != 1 {
		t.Fatal("initial delta did not reproduce the graph")
	}
	var got health
	if Empty(Get(replica, &got)) || got != (health{10, 10}) {
		t.Fatal("initial attributes were not applied")
	}

	replicaActors, closeReplicaActors := Query(replica, Type[actor]())
	defer closeReplicaActors()
	if Len(replicaActors) != 1 {
		t.Fatal("replica query bootstrap")
	}

	Set(source, health{7, 10})
	Unset(actorNode, Type[actor]())
	newBranch := New(actor{}, name("new actor"))
	newItem := New(name("new item"))
	Link(newBranch, newItem)
	Link(source, newBranch)
	Unlink(actorNode, item)

	updated := Apply(replica, Delta(source))
	Commit(source)
	if Len(updated) != 1 {
		t.Fatal("updated roots")
	}
	if Empty(Get(replica, &got)) || got != (health{7, 10}) {
		t.Fatal("changed attribute was not applied")
	}
	children := At(replica)
	if Len(children) != 2 {
		t.Fatal("added branch was not applied")
	}
	var oldActor, replicatedNewActor Graph
	for child := range Each(children) {
		var childName name
		Get(child, &childName)
		switch childName {
		case "actor":
			oldActor = child
		case "new actor":
			replicatedNewActor = child
		}
	}
	if !Empty(Get(oldActor, Type[actor]())) || !Empty(At(oldActor)) {
		t.Fatal("attribute or edge removal was not applied")
	}
	if Empty(Get(replicatedNewActor, Type[actor]())) || Len(At(replicatedNewActor)) != 1 {
		t.Fatal("new subtree was not applied")
	}
	if Len(replicaActors) != 1 {
		t.Fatal("live query was not updated by apply")
	}
}

func TestApplyRejectsNonDelta(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	Apply(New(name("target")), New(name("not a delta")))
}

func TestSelectionOperationsAndNavigation(t *testing.T) {
	a := New(name("a"))
	b := New(name("b"))
	c := New(name("c"))
	Link(a, b, c)
	if Len(At(a, c, b, c, Graph{})) != 2 {
		t.Fatal("filtered children")
	}
	if Len(Include(Merge(a, b, c), Merge(c, a), Merge(a, c))) != 2 {
		t.Fatal("intersection")
	}
	if !Empty(Include()) || Len(Exclude(Merge(a, b, c), b, c)) != 1 || !Empty(Exclude()) {
		t.Fatal("empty or difference")
	}
	count := 0
	for range Each(Merge(a, b)) {
		count++
		break
	}
	if count != 1 {
		t.Fatal("early iteration stop")
	}
	if !Empty(At(Graph{})) || !Empty(Link(Graph{}, a)) || !Empty(Unlink(Graph{}, a)) {
		t.Fatal("empty graph operations")
	}
	if !Empty(Link(a, b)) || !Empty(Unlink(a, New(name("missing")))) {
		t.Fatal("relation no-op")
	}
}

func TestAttributeValidationPanics(t *testing.T) {
	tests := []struct {
		name string
		fn   func()
	}{
		{"new without attributes", func() { New(nil) }},
		{"nil attribute", func() { New(name("x"), nil) }},
		{"pointer attribute", func() { value := name("x"); New(&value) }},
		{"non-comparable attribute", func() { New([]int{1}) }},
		{"duplicate attribute", func() { New(name("x"), name("y")) }},
		{"get without requests", func() { Get(New(name("x"))) }},
		{"nil request", func() { Get(New(name("x")), nil) }},
		{"nil destination", func() { var value *name; Get(New(name("x")), value) }},
		{"non-comparable request", func() { Get(New(name("x")), []int{1}) }},
		{"duplicate request", func() { Get(New(name("x")), name("x"), Type[name]()) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			tt.fn()
		})
	}
}

func TestApplyEmptyDeltaIsNoOp(t *testing.T) {
	target := New(name("target"))
	if Empty(Apply(target, Graph{})) {
		t.Fatal("empty delta discarded target")
	}
}

func TestApplyDeepMergesOrdinaryGraphByCompositeKey(t *testing.T) {
	world := New(name("world"), health{100, 100})
	actorOne := New(entityKind("actor"), entityID("one"), name("old"), health{10, 10})
	itemOne := New(entityKind("item"), entityID("one"), name("sword"))
	Link(world, actorOne, itemOne)
	oldNames, closeOldNames := Query(world, name("old"))
	defer closeOldNames()

	patch := New(name("patched world"))
	actorPatch := New(entityKind("actor"), entityID("one"), name("new"))
	newActorPatch := New(entityKind("actor"), entityID("two"), health{5, 5})
	nestedItemPatch := New(entityKind("item"), entityID("nested"), name("shield"))
	Link(newActorPatch, nestedItemPatch)
	Link(patch, actorPatch, newActorPatch)

	result := Apply(world, patch, Type[entityKind](), Type[entityID]())
	if Len(result) != 1 || Len(At(world)) != 3 {
		t.Fatal("ordinary patch structure was not merged")
	}
	var worldName name
	var actorName name
	var retainedHealth health
	Get(world, &worldName)
	Get(actorOne, &actorName, &retainedHealth)
	if worldName != "patched world" || actorName != "new" || retainedHealth != (health{10, 10}) {
		t.Fatal("attributes were not deeply merged")
	}
	if !Empty(oldNames) {
		t.Fatal("live query index was not updated")
	}
	if Len(At(actorOne)) != 0 {
		t.Fatal("composite key matched the item with the same ID")
	}
	var added Graph
	for child := range Each(At(world)) {
		var kind entityKind
		var id entityID
		Get(child, &kind, &id)
		if kind == "actor" && id == "two" {
			added = child
		}
	}
	if Empty(added) || Len(At(added)) != 1 {
		t.Fatal("new nested branch was not cloned")
	}
}

func TestApplyOrdinaryGraphMatchesNodesGlobally(t *testing.T) {
	world := New(entityID("world"))
	actor := New(entityID("actor"), entityKind("actor"))
	skill := New(entityID("sprint"), entityKind("skill"), name("old"))
	Link(world, actor)
	Link(actor, skill)
	Commit(world)

	patchActor := New(entityID("actor"), entityKind("actor"))
	perform := New(entityID("perform"), entityKind("perform"))
	skillReference := New(entityID("sprint"), entityKind("skill"), name("updated"))
	Link(patchActor, perform)
	Link(patchActor, skillReference)
	Link(perform, skillReference)

	updatedSkills, closeUpdatedSkills := Query(world, name("updated"))
	defer closeUpdatedSkills()

	result := Apply(world, patchActor, Type[entityID]())
	if first(result) != first(actor) {
		t.Fatal("patch root was not matched globally")
	}
	var targetPerform Graph
	for child := range Each(At(actor)) {
		var id entityID
		Get(child, &id)
		if id == "perform" {
			targetPerform = child
		}
	}
	if Len(At(actor)) != 2 || Len(At(targetPerform)) != 1 || Empty(At(targetPerform, skill)) {
		t.Fatal("existing node was not linked into the new branch")
	}
	if first(At(targetPerform)) != first(skill) {
		t.Fatal("patch created a duplicate node")
	}
	var skillName name
	Get(skill, &skillName)
	if skillName != "updated" || Len(updatedSkills) != 1 {
		t.Fatal("matched node attributes or live query were not updated")
	}

	Commit(world)
	Apply(world, patchActor, Type[entityID]())
	if !Empty(Delta(world)) || Len(At(actor)) != 2 || Len(At(targetPerform)) != 1 {
		t.Fatal("reapplying the same patch was not a no-op")
	}
}

func TestApplyOrdinaryGraphValidatesMatchKeys(t *testing.T) {
	tests := []struct {
		name  string
		world Graph
		patch Graph
	}{
		{
			name:  "missing key",
			world: New(name("world")),
			patch: func() Graph {
				root := New(name("patch"))
				Link(root, New(name("child")))
				return root
			}(),
		},
		{
			name:  "duplicate patch key",
			world: New(name("world")),
			patch: func() Graph {
				root := New(name("patch"))
				Link(root, New(entityID("same")), New(entityID("same")))
				return root
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			Apply(tt.world, tt.patch, Type[entityID]())
		})
	}
}

func TestCommitAndApplyUseCompositeIdentityAcrossIndependentReplicas(t *testing.T) {
	keys := []any{Type[entityKind](), Type[entityID]()}
	source := New(entityKind("world"), entityID("main"), name("source"))
	sourceActor := New(entityKind("actor"), entityID("one"), health{10, 10})
	sourceRemoved := New(entityKind("item"), entityID("old"), name("old item"))
	Link(source, sourceActor, sourceRemoved)
	Commit(source)

	target := New(entityKind("world"), entityID("main"), name("target"))
	targetActor := New(entityKind("actor"), entityID("one"), health{1, 10})
	targetRemoved := New(entityKind("item"), entityID("old"), name("old item"))
	Link(target, targetActor, targetRemoved)

	Set(sourceActor, health{8, 10})
	Unlink(source, sourceRemoved)
	sourceAdded := New(entityKind("item"), entityID("new"), name("new item"))
	Link(source, sourceAdded)
	delta := Delta(source, keys...)
	Apply(target, delta, keys...)

	var got health
	Get(targetActor, &got)
	if got != (health{8, 10}) {
		t.Fatal("independent replica node was not matched")
	}
	if !Empty(At(target, targetRemoved)) || Len(At(target)) != 2 {
		t.Fatal("keyed structural changes were not applied")
	}
	var foundAdded bool
	for child := range Each(At(target)) {
		var id entityID
		Get(child, &id)
		foundAdded = foundAdded || id == "new"
	}
	if !foundAdded {
		t.Fatal("keyed added node was not applied")
	}
}

func TestApplyRejectsDeltaIdentityMismatch(t *testing.T) {
	source := New(entityID("root"))
	delta := Delta(source, Type[entityID]())
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	Apply(New(entityID("root")), delta)
}

func TestCommitRecursivelyAdvancesNodeBaselines(t *testing.T) {
	root := New(name("root"))
	branch := New(name("branch"))
	shared := New(name("shared"), health{10, 10})
	otherRoot := New(name("other root"))
	Link(root, branch)
	Link(branch, shared)
	Link(otherRoot, shared)
	Commit(Merge(root, otherRoot))

	Set(shared, health{9, 10})
	if Empty(Delta(root)) || Empty(Delta(branch)) || Empty(Delta(otherRoot)) {
		t.Fatal("changed shared node was not visible through every owner")
	}

	Commit(branch)
	if !Empty(Delta(branch)) || !Empty(Delta(root)) || !Empty(Delta(otherRoot)) {
		t.Fatal("subgraph commit did not globally commit its reachable nodes")
	}

	Set(shared, health{8, 10})
	Commit(root)
	if !Empty(Delta(otherRoot)) {
		t.Fatal("root commit did not commit a node shared with another graph")
	}
}
