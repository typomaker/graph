# graph

`graph` is a compact Go library for storing typed data in a directed acyclic graph (DAG).

It addresses a recurring problem in game worlds, configuration systems, dependency graphs, scenes, and domain models: data needs to be composed from independent components, connected through relationships, queried by type or value, and tracked for branch-level changes.

There are no public `Node`, `Edge`, or internal identifier types. The entire public API operates on `Graph`, which can represent one node, a selection of nodes, or an empty result. Attributes are ordinary comparable Go values.

## Features

- Typed attributes without schema registration
- Directed relations, multiple parents, and cycle prevention
- Selection and set operations
- Reactive queries updated by `Set`, `Unset`, `Link`, and `Unlink`
- Revision propagation and pruned change graphs through `Delta`
- Explicit delta baselines through `Commit`
- Replica synchronization through `Delta` and `Apply`
- Thread-safe operations and a small package-level API

## Installation

```bash
go get github.com/typomaker/graph
```

The library requires Go 1.23 or newer.

## Quick start

```go
package main

import (
	"fmt"

	"github.com/typomaker/graph"
)

type World struct{}
type Actor struct{}
type ID string
type Health struct {
	Current int
	Max     int
}

func main() {
	world := graph.New(World{})
	player := graph.New(
		Actor{},
		ID("player-1"),
		Health{Current: 100, Max: 100},
	)

	graph.Link(world, player)

	var health Health
	if !graph.Empty(graph.Get(player, &health)) {
		fmt.Println(health.Current) // 100
	}
}
```

## Attributes

An attribute type is its key. A node can contain at most one value of each type.

```go
type Name string
type Alive bool

actor := graph.New(
	Actor{},
	Name("Ada"),
	Alive(true),
	Health{Current: 80, Max: 100},
)
```

Attributes must be comparable Go values, such as numbers, strings, named types, or structs containing comparable fields. Slices, maps, functions, interface values, and pointers cannot be stored. Pointers are used only as destinations when reading or removing attributes.

### Check and read

`Type[T]()` matches any value of type `T`. A value of type `T` performs an exact comparison. A `*T` argument reads the stored value.

```go
hasHealth := graph.Get(actor, graph.Type[Health]())
isAlive := graph.Get(actor, Alive(true))

var health Health
if !graph.Empty(graph.Get(actor, &health)) {
	health.Current -= 10
}
```

With multiple arguments, `Get` returns the node if at least one argument matches. A destination remains unchanged when its attribute is absent.

### Set and remove

`Set` and `Unset` return the node only when data actually changes. This makes no-op detection inexpensive.

```go
changed := graph.Set(actor, Health{Current: 70, Max: 100})
if !graph.Empty(changed) {
	// Health changed.
}

// Read the old value and remove the attribute.
var old Health
removed := graph.Unset(actor, &old)

// Remove without reading the value.
graph.Unset(actor, graph.Type[Alive]())
```

## Relations and navigation

Relations are directed from parent to child and carry no data of their own.

```go
type Inventory struct{}
type Item struct{}
type Damage int

world := graph.New(World{})
player := graph.New(Actor{}, ID("player-1"))
inventory := graph.New(Inventory{})
rifle := graph.New(Item{}, Damage(20))

graph.Link(world, player)
graph.Link(player, inventory)
graph.Link(inventory, rifle)

children := graph.At(player) // Immediate children: inventory.
fmt.Println(graph.Len(children))

if !graph.Empty(graph.At(inventory, rifle)) {
	fmt.Println("rifle is in inventory")
}

graph.Unlink(inventory, rifle)
```

Linking an existing pair and unlinking a missing pair are safe no-ops. Creating a cycle panics because it violates the DAG invariant.

`Unlink` removes only the relation. A detached node remains usable while a
user-held `Graph`, an active query, a delta, or another edge references it, and
it can be linked again later. The package does not keep a global registry of
all nodes, so a detached component with no remaining references can be
reclaimed by Go's garbage collector.

A child may have multiple parents:

```go
shared := graph.New(Item{}, ID("shared-map"))
alice := graph.New(Actor{}, ID("alice"))
bob := graph.New(Actor{}, ID("bob"))

graph.Link(alice, shared)
graph.Link(bob, shared)
```

If a relationship needs state, represent it as a node. For example, model travel progress as `Actor -> Travel -> City` and store progress on `Travel`.

## Selections

A `Graph` may contain any number of nodes. `Each` iterates over it as single-node `Graph` values.

```go
actors, closeActors := graph.Query(world, graph.Type[Actor]())
defer closeActors()

for actor := range graph.Each(actors) {
	graph.Set(actor, Health{Current: 100, Max: 100})
}

fmt.Println(graph.Len(actors))
fmt.Println(graph.Empty(actors))
```

Set operations remove duplicates and preserve first appearance:

```go
all := graph.Merge(players, enemies)        // union
both := graph.Include(visible, selectable)  // intersection
active := graph.Exclude(all, disconnected)  // difference
```

Attribute and structural operations use only the first node of their `Graph` arguments. Use `Each` for explicit bulk updates.

## Reactive queries

`Query` searches the selected roots and every node reachable from them. Matchers use AND semantics. The bootstrap result is available immediately and remains current as the graph changes.

```go
type Faction string

pirates, closePirates := graph.Query(
	world,
	graph.Type[Actor](),
	Faction("pirates"),
	Alive(true),
)
defer closePirates()

// The matching node automatically appears in pirates.
jack := graph.New(Actor{}, Faction("pirates"), Alive(true))
graph.Link(world, jack)

// It automatically leaves the result after this update.
graph.Set(jack, Alive(false))
```

Call the returned close function when the live result is no longer needed. It is idempotent. After closing, the returned `Graph` remains available as a snapshot but no longer updates.

Search by a user-defined identifier in the same way:

```go
player, closePlayer := graph.Query(
	world,
	graph.Type[Actor](),
	ID("player-1"),
)
defer closePlayer()
```

## Track changed branches with Delta and Commit

`Delta` returns the smallest current subgraph connecting the supplied roots to
nodes changed since the baseline. Reading a delta does not modify the baseline.
`Commit` explicitly advances the baseline of every reachable node to the
current revision.

```go
world := graph.New(World{})
player := graph.New(Actor{})
inventory := graph.New(Inventory{})
rifle := graph.New(Item{}, Damage(20))

graph.Link(world, player)
graph.Link(player, inventory)
graph.Link(inventory, rifle)

graph.Commit(world) // Record the initial baseline.

graph.Set(rifle, Damage(25))
changed := graph.Delta(world)

// changed contains only world -> player -> inventory -> rifle.
for root := range graph.Each(changed) {
	for child := range graph.Each(graph.At(root)) {
		// Process the changed branch.
		_ = child
	}
}

fmt.Println(graph.Empty(graph.Delta(world))) // false: reading is repeatable
graph.Commit(world)
fmt.Println(graph.Empty(graph.Delta(world))) // true
```

Real attribute or relation changes update revisions and propagate dirty information through every parent. No-op operations do not change revisions.

A commit is global for the selected subgraph. `Commit(world)` commits every
node reachable from `world`, while `Commit(player)` commits only `player` and
its descendants. If a node is shared by multiple roots, committing it through
one root also removes its changes from deltas read through the other roots.
Callers are responsible for committing only after every intended consumer has
processed the changes.

## Synchronize replicas

`Apply` merges a delta into another in-memory replica. Bootstrap the replica by
applying the source's first delta to an empty graph. Commit the source after the
delta has been accepted, then repeat the same sequence for later changes:

```go
source := graph.New(World{})
player := graph.New(Actor{}, Health{Current: 100, Max: 100})
graph.Link(source, player)

initial := graph.Delta(source)
replica := graph.Apply(graph.Graph{}, initial)
graph.Commit(source)

graph.Set(player, Health{Current: 90, Max: 100})
delta := graph.Delta(source)
graph.Apply(replica, delta)
graph.Commit(source)
```

Applying a delta reproduces attribute additions, replacements, and removals,
as well as linked, unlinked, and newly created branches. Live queries on the
replica are updated.

The initial delta establishes the internal node identities shared by the two
replicas. Deltas must be applied and committed in order. `Graph` and
its deltas are in-memory values; encoding and transport across processes are
outside this package's current API.

Two independently constructed replicas can instead use the same stable
application attributes as a composite identity. Pass the attributes to both
operations in the same order:

```go
delta := graph.Delta(
	source,
	graph.Type[EntityKind](),
	graph.Type[ID](),
)

graph.Apply(
	replica,
	delta,
	graph.Type[EntityKind](),
	graph.Type[ID](),
)
```

Every node in this mode must contain every identity attribute. Their values
must remain stable and uniquely identify a node within the replica. `Apply`
panics if its identity attributes differ from those used by `Delta`.

When no identity attributes are supplied, `Delta` and `Apply` use an
immutable internal node ID. This is suitable for a replica bootstrapped from
the source's initial delta because that operation transfers the identity. It
cannot match independently constructed graphs. A creation timestamp is not
used: timestamps can collide, depend on clock behavior, and do not establish
that two separately created nodes represent the same entity.

### Merge a programmatic patch

`Apply` can also deeply merge an ordinary graph that was built without
revision metadata. Supply one or more attribute types that form the composite
identity of every non-root node:

```go
patch := graph.New(WorldName("changed"))
changedPlayer := graph.New(EntityKind("actor"), ID("player-1"), Health{Current: 80, Max: 100})
newItem := graph.New(EntityKind("item"), ID("shield"), Armor(20))
graph.Link(changedPlayer, newItem)
graph.Link(patch, changedPlayer)

graph.Apply(
	world,
	patch,
	graph.Type[EntityKind](),
	graph.Type[ID](),
)
```

Patch roots correspond to target roots by selection order. Descendants match
only when all supplied attributes are equal. Matching nodes receive every
attribute present in the patch, and missing branches are copied recursively.
Attributes and branches absent from an ordinary patch remain unchanged; use a
`Delta` when removals must be represented. Match keys must be present on
every patch descendant and unique among siblings.

## Programmer errors

The library panics when an invariant is violated:

- A `nil`, pointer, or non-comparable stored attribute
- Duplicate attribute types in one call
- A relation that creates a cycle

`T` and `*T` refer to the same attribute type. An empty `Graph` is valid; reads and mutations on it return an empty result.

## Performance

A reactive result is stored as a ready-to-use selection, so `Len` does not repeat the search. A relevant attribute mutation updates only the affected node, while an unrelated attribute type does not recompute the query.

Run the benchmarks with:

```bash
go test -run '^$' -bench '^BenchmarkQuery' -benchmem
```

Indicative results for a 10,000-node graph on an Apple M1 Max:

| Operation | Time | Allocations |
|---|---:|---:|
| Exact-query bootstrap | ~2.1 ms | 144 |
| Reactive result `Len` | ~14 ns | 0 |
| Relevant attribute update | ~0.41 us | 4 |
| Unrelated attribute update | ~0.39 us | 3 |

Actual results depend on the processor, graph shape, number of active queries, and result size.
