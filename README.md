# graph

`graph` is a small, type-oriented directed acyclic graph for Go. Nodes and edges stay private; the public `Graph` value represents a node or an ordered selection.

## Install

```sh
go get github.com/typomaker/graph
```

## Nodes and attributes

Every attribute is a comparable, non-pointer value identified by its concrete Go type.

```go
type Name string
type Position struct{ X, Y int }

player := graph.New(Name("player"), Position{X: 10, Y: 20})
position, ok := graph.Get[Position](player)
changed := graph.Set(player, Position{X: 20, Y: 30})
previous, removed := graph.Unset[Position](player)
```

`Get` and `Unset` operate on the first selected node. `Set` returns true only when it inserts or changes the attribute. `Unset` returns the previous value and whether it existed.

## Relations

`Link` creates outgoing edges from the first node in its first argument to the first node in each following argument. `Unlink` removes them. Both return true when at least one edge changed.

```go
world := graph.New(Name("world"))
player := graph.New(Name("player"))
inventory := graph.New(Name("inventory"))

graph.Link(world, player)
graph.Link(player, inventory)
graph.Unlink(player, inventory)
```

The graph is acyclic. `Link` panics if an edge would introduce a cycle. A node may have several parents.

## Selecting nodes

Selectors are synchronous snapshots and do not require a closer:

- `Select` tests only the nodes already present in the input selection and returns matching starting nodes.
- `Search` tests the input nodes and every node reachable from them, returning matching starting nodes.
- `Follow` tests the input nodes like `Select`, but returns endpoints produced by structural expressions. Without `Path`, it is equivalent to `Select`.

Plain arguments use AND semantics. `And` and `Or` provide explicit grouping. A value matches the same type and value; `Type[T]()` matches any value of type `T`; `Type[any]()` matches any node.

```go
actors := graph.Search(world, graph.Type[Actor]())
visiblePlayers := graph.Search(
    world,
    graph.Type[Player](),
    graph.Or(Team("red"), Team("blue")),
)
```

Selections do not update after graph mutations. Run the selector again when a fresh result is needed.

## Structural paths

`Path` starts with the immediate children of each candidate. Every subsequent step follows one outgoing edge. Ordinary predicates never search descendants implicitly.

For a structure `Location -> Contains -> Actor`:

```go
locations := graph.Search(
    world,
    graph.Type[Location](),
    graph.Path(
        graph.Type[Contains](),
        graph.And(graph.Type[Actor](), ID("actor-1")),
    ),
)
```

The same expression can return terminal actors:

```go
actors := graph.Follow(
    locations,
    graph.Type[Location](),
    graph.Path(graph.Type[Contains](), graph.Type[Actor]()),
)
```

Use `graph.Follow(node, graph.Path(graph.Type[any]()))` to obtain immediate children.

A path step may contain `And` or `Or`, but not another `Path`. When an `And` expression contains several paths, every path must match and `Follow` returns the union of their endpoints. An `Or` expression returns endpoints from every matching alternative.

## Selection operations

`Union`, `Intersect`, and `Difference` combine selections while preserving stable order and removing duplicates. `Len` and `Empty` inspect them. `Each` iterates over singleton `Graph` values.

```go
for actor := range graph.Each(actors) {
    graph.Set(actor, Visible(true))
}
```

## Synchronization

`Commit` establishes a revision baseline for reachable nodes and publishes an immutable search index for each selected root. `Delta` creates a change graph since that baseline, and `Apply` applies such a delta. Optional type matchers define a composite identity for independent replicas.

```go
changes := graph.Delta(source, graph.Type[Kind](), graph.Type[ID]())
target = graph.Apply(target, changes)
graph.Commit(source)
```

`Patch` additively merges an ordinary graph by a required composite key:

```go
graph.Patch(target, patch, graph.Type[ID]())
```

`Apply` and `Patch` build temporary composite indexes for their operation and discard them on return.

## Performance

`Select`, `Follow`, and `Search` are snapshot operations. After `Commit`, `Search` can use immutable type and value postings and evaluate structural paths backwards through internal parent relations. Conjunctions start with their most selective posting. Results retain graph traversal order.

Mutations do not rebuild postings. `Set` and `Unset` add the changed node to a small per-index overlay; restoring its committed attributes removes it again. Ordinary predicates recheck that node, while structural predicates also recheck its indexed ancestors. Committed indexes count active parents for every reachable node. `Link` and `Unlink` update those support counts and activate or deactivate only the affected branch. Added and removed edges are normalized against the committed structure, so opposite operations cancel and immediately restore the clean fast path. Result order is rebuilt lazily by the first subsequent search and already ordered results avoid copying and sorting. Calling `Commit` publishes a replacement index and clears both overlays. Bulk `Apply` and `Patch` changes conservatively fall back to a current traversal until the next commit. Selections returned by an indexed search retain that index while its overlay revision remains current, allowing `Select` and `Follow` to use it for structural prefiltering.

```sh
go test -bench=. -benchmem
```

## Constraints

- Attributes must be comparable, non-pointer values.
- A node can hold only one attribute of each concrete type.
- Logical expressions and selectors require at least one predicate.
- `Path` requires at least one step and cannot be nested.
- Graph state is process-local and safe for concurrent access.
