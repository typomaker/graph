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

`Query` accepts the same predicates as `Search` and returns a reusable iterator.
Creating it runs `Search` and materializes the result. Graph mutations only mark
that result as stale. The next iteration refreshes it once through the existing
indexed search, regardless of how many mutations occurred in between. Repeated
iterations without mutations reuse the materialized result without searching.
One iteration uses one stable snapshot; mutations during that iteration appear
on the next iteration, even before `Commit`. Call the returned close function
when the query is no longer needed; it releases the source, predicates, and
materialized result. Closing is idempotent.

```go
visibleActors, closeVisibleActors := graph.Query(
    world,
    graph.Type[Actor](),
    Visible(true),
)
defer closeVisibleActors()

for actor := range visibleActors {
    // Current matches.
}

graph.Set(player, Visible(true))

for actor := range visibleActors {
    // The updated matches.
}
```

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

### Flattening a view

`Unnest` transforms the current reachable view into a flat selection. It
includes the selected roots and all descendants visible through that view.
Shared nodes and nodes reached by several paths occur once. It does not mutate
the input graph or introduce a separate iterator API:

```go
flat := graph.Unnest(world)
for node := range graph.Each(flat) {
	// Inspect one node from the reachable view.
}
```

When the input is a restricted view, such as a delta or a materialized delta,
`Unnest` follows only links present in that view. It does not expand omitted
subtrees from the underlying graph.

## Synchronization

`Commit` establishes a revision baseline for reachable nodes and publishes an immutable search index for each selected root. `Delta` creates a change graph since that baseline, and `Apply` applies such a delta. Optional type matchers define a composite identity for independent replicas.

```go
changes := graph.Delta(source, graph.Type[Kind](), graph.Type[ID]())
target = graph.Apply(target, changes)
graph.Commit(source)
```

`Changed` selects only nodes with their own attribute or link changes. It
removes unchanged ancestors that `Delta` retains to preserve paths. Links in
the result are direct links whose two endpoints are both selected; paths are
never compressed through omitted nodes. `Detached` returns attribute snapshots
of nodes that were reachable at the previous commit but have since lost their
last reachable parent. A node that remains reachable through another parent is
not detached.

Both functions accept either the source graph or an already prepared delta.
Passing a source graph computes the equivalent delta internally, while passing
a delta reuses its recorded change view:

```go
changed := graph.Changed(source)
detached := graph.Detached(source)

delta := graph.Delta(source)
changed = graph.Changed(delta)
detached = graph.Detached(delta)
```

`Changed` creates a restricted view over the existing nodes rather than copying
them. `Materialize` also resolves a view to existing source nodes without
cloning those nodes. `Detached` is intentionally different: it records an
independent attribute snapshot when a baseline node becomes unreachable, so a
later mutation of the original node cannot change the detached result.

The source retains detached snapshots only until its next `Commit`:

```go
detached := graph.Detached(source)
graph.Commit(source)

graph.Empty(graph.Detached(source)) // true: the source has a new baseline
graph.Empty(detached)               // false: the returned snapshot is valid
```

### Materializing a sparse view

`Materialize` resolves a sparse view, such as a delta, back to the current
nodes in its source. The view determines which nodes and links remain visible;
the source supplies their current attributes. Nodes removed from the source
are omitted, and unchanged siblings or descendants are not added:

```go
delta := graph.Delta(source, graph.Type[ID]())
current := graph.Materialize(source, delta)
for node := range graph.Each(graph.Unnest(current)) {
	// Inspect current state.
}
```

`Materialize` automatically uses identity recorded by `Delta`. For another
view, pass the same optional identity matchers used by `Delta` and `Apply`.
An ordinary (non-structural) selection is materialized as a flat selection;
the operation never infers additional topology from `source`.

```go
current := graph.Materialize(
	source,
	independentView,
	graph.Type[ID](),
)
```

`Patch` additively merges an ordinary graph by a required composite key:

```go
graph.Patch(target, patch, graph.Type[ID]())
```

`Apply` uses the committed internal identity index when available. Requested composite-key indexes are cached on the root index and invalidated by relevant key or structural changes.

The first `Commit` of a root traverses its reachable graph and builds the search index. Later commits consume the propagated change frontier: they advance baselines only along changed paths and fold small search overlays into the existing index. Large or fragmented overlays trigger an automatic full compaction. Baselines belong to nodes rather than roots, so committing a shared node acknowledges that node for every root that reaches it; stale frontiers on other roots are pruned by their next `Delta` or `Commit`.

When a new edge points to an already committed node, `Delta` includes that node's identity but omits its unchanged descendants. `Apply` can therefore connect an existing replica node without retransmitting its committed subtree. Edges to new nodes still include their complete new subgraphs.

### Binary updates

`Wire` provides a versioned, process-independent binary representation of the
same changes tracked by `Commit`. Each definition associates a stable protocol
label with a Go attribute type:

```go
wire := graph.NewWire(
	graph.Define[Actor]("actor"),
	graph.Define[Health]("health"),
)

update, err := wire.ExportUpdate(source)
if err == nil {
	err = wire.ImportUpdate(replica, update)
}
```

The graph passed to `ExportUpdate` is the spatial scope; only changes reachable
from that selection are included. Export is non-consuming and never performs
an implicit `Commit`. A precomputed `Delta` may also be passed directly; Wire
recognizes its private change view and encodes it without computing another
delta. This is useful when the same delta is inspected locally before being
sent over the network. Existing roots are paired by selection order, and other
existing nodes are resolved from unchanged, uniquely matching defined
attributes. A link to an unchanged node outside the encoded fragment is applied
only when that node can be resolved in the replica scope; otherwise import
fails instead of creating incomplete topology.

Wire schemas evolve additively. An importer skips length-delimited attributes
whose labels it does not define and continues decoding known data. If an
unknown-only node is structural, its entire unreachable fragment is omitted
rather than flattening its known descendants into the surrounding topology.
Unknown data is not preserved for a later re-export by the older process.
Adding a definition does not change the binary format version; incompatible
changes to the binary protocol do.

## JSON

The `graphjson` package encodes a graph as a flat, versioned document. Declare
the stable JSON name of every attribute type in a schema:

```go
var schema = graph.Schema{
	graph.Attribute[ID]("id"),
	graph.Attribute[Name]("name"),
}

data, err := graphjson.Marshal(world, schema)

var restored graph.Graph
err = graphjson.Unmarshal(data, &restored, schema)
```

The document stores roots, nodes, and ordered links separately. Keys beginning
with `@` belong to the format; node keys exist only within one document and do
not expose the graph's internal identity. Shared children are encoded once and
remain shared after decoding. `Unmarshal` replaces its destination only after
the complete document has been decoded and validated.

```json
{
  "@version": 1,
  "@roots": ["1"],
  "@nodes": [
    {"@key": "1", "id": "world", "@links": ["2"]},
    {"@key": "2", "id": "player", "name": "Player"}
  ]
}
```

Attribute values use `encoding/json`, including implementations of
`json.Marshaler` and `json.Unmarshaler`. Unknown attributes, dangling links,
duplicate keys, cycles, unreachable nodes, and unsupported format versions are
rejected.

## Performance

`Select`, `Follow`, and `Search` are snapshot operations. After `Commit`, `Search` can use immutable type and value postings and evaluate structural paths backwards through internal parent relations. Conjunctions start with their most selective posting and choose merge or binary intersections according to posting density. Results retain graph traversal order.

Mutations do not rebuild committed postings. `Set` and `Unset` add the changed node to a small per-index overlay; restoring its committed attributes removes it again. Ordinary predicates recheck that node, while structural predicates also recheck its indexed ancestors. Committed indexes count active parents for every reachable node. `Link` and `Unlink` update those support counts and activate or deactivate only the affected branch. Added and removed edges are normalized against the committed structure, so opposite operations cancel and immediately restore the clean fast path.

Small added branches are checked directly. Once an added-node overlay becomes large, it receives its own type and value postings, which are updated by later attribute and reachability changes. Dense ordinal arrays track reachability and traversal order without per-result hash lookups. Result order is rebuilt lazily by the first subsequent search; an identity-order flag bypasses subsequent checks, and compatible searches stream nodes directly into the returned selection.

Structural paths choose between sparse parent expansion and dense bitmaps. Overlay paths use the same reverse evaluation across committed and added ordinals, including current edge additions and removals. Sorted mutation postings are prepared when the graph changes rather than on every search. Bulk `Apply` and `Patch` replay structural changes as one incremental batch. Committed indexes also retain internal identity and requested composite-key indexes so repeated synchronization does not rescan the graph. Relevant key-attribute or structural changes invalidate those caches.

Attribute and structural mutations register the immediate changed child at every reachable ancestor. `Delta` follows this frontier instead of scanning unchanged siblings. Calling `Commit` normally advances the same frontier and retains the layered index; automatic compaction publishes a replacement index only when accumulated overrides or tombstones become large. Selections returned by an indexed search retain the current root index while its overlay revision remains valid, allowing `Select` and `Follow` to use it for structural prefiltering.

```sh
go test -bench=. -benchmem
```

## Constraints

- Attributes must be comparable, non-pointer values.
- A node can hold only one attribute of each concrete type.
- Logical expressions and selectors require at least one predicate.
- `Path` requires at least one step and cannot be nested.
- Graph state is process-local and safe for concurrent access.
