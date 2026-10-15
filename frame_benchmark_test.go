package graph

import (
	"fmt"
	"testing"
)

type frameRoot struct{}
type frameContainer struct{}
type frameParameter struct{ Value int }
type frameItem struct{}
type frameHolding struct{}
type frameInventory struct{}
type frameTimeRange struct{}
type frameContains struct{}
type frameActor struct{}
type frameUUID int
type frameVersion int

type frameFixture struct {
	root       Graph
	containers Graph
	dirty      Graph
}

func benchmarkFrameFixture(size int) frameFixture {
	root := New(frameRoot{})
	containers := make([]Graph, 0, size)
	var dirty Graph
	for i := range size {
		container := New(frameContainer{}, frameUUID(i))
		parameter := New(frameParameter{Value: i})
		item := New(frameItem{})
		holding := New(frameHolding{})
		inventory := New(frameInventory{}, frameTimeRange{})
		contains := New(frameContains{})
		actor := New(frameActor{}, frameUUID(i+size))
		actorInventory := New(frameInventory{})
		actorContains := New(frameContains{}, frameUUID(i))
		Link(root, container, actor)
		Link(container, parameter, item, holding, inventory)
		Link(inventory, contains)
		Link(contains, item)
		Link(actor, actorInventory)
		Link(actorInventory, actorContains)
		containers = append(containers, container)
		dirty = parameter
	}
	Commit(root)
	return frameFixture{root: root, containers: Union(containers...), dirty: dirty}
}

func BenchmarkFrameTraversalPatterns(b *testing.B) {
	for _, size := range []int{0, 1, 1024} {
		b.Run(fmt.Sprintf("N%d", size), func(b *testing.B) {
			fixture := benchmarkFrameFixture(size)
			parameterPath := Path(Type[frameParameter]())
			itemPath := Path(Or(Type[frameItem](), Type[frameHolding]()))
			inventoryPath := Path(
				And(Type[frameInventory](), Type[frameTimeRange]()),
				Type[frameContains](),
				Or(Type[frameItem](), Type[frameHolding]()),
			)
			actorSearch := And(
				Type[frameActor](),
				Path(Type[frameInventory](), And(Type[frameContains](), frameUUID(max(size-1, 0)))),
			)

			b.Run("EachGet", func(b *testing.B) {
				for range b.N {
					count := 0
					for node := range Each(fixture.containers) {
						_, _ = Get[frameContainer](node)
						count++
					}
					if count != size {
						b.Fatal(count)
					}
				}
			})
			b.Run("FollowParameter", func(b *testing.B) {
				for range b.N {
					if result := Follow(fixture.containers, parameterPath); Len(result) != size {
						b.Fatal(Len(result))
					}
				}
			})
			b.Run("FollowItemOrHolding", func(b *testing.B) {
				for range b.N {
					if result := Follow(fixture.containers, itemPath); Len(result) != size*2 {
						b.Fatal(Len(result))
					}
				}
			})
			b.Run("FollowInventoryPath", func(b *testing.B) {
				for range b.N {
					if result := Follow(fixture.containers, inventoryPath); Len(result) != size {
						b.Fatal(Len(result))
					}
				}
			})
			b.Run("SearchActorPath", func(b *testing.B) {
				want := 0
				if size != 0 {
					want = 1
				}
				for range b.N {
					if result := Search(fixture.root, actorSearch); Len(result) != want {
						b.Fatal(Len(result))
					}
				}
			})
		})
	}
}

func BenchmarkFramePredicateLifecycle(b *testing.B) {
	fixture := benchmarkFrameFixture(1024)
	b.Run("Build", func(b *testing.B) {
		b.Run("TypePath", func(b *testing.B) {
			for range b.N {
				_ = Path(Type[frameParameter]())
			}
		})
		b.Run("OrPath", func(b *testing.B) {
			for range b.N {
				_ = Path(Or(Type[frameItem](), Type[frameHolding]()))
			}
		})
		b.Run("DeepPath", func(b *testing.B) {
			for range b.N {
				_ = Path(
					And(Type[frameInventory](), Type[frameTimeRange]()),
					Type[frameContains](),
					Or(Type[frameItem](), Type[frameHolding]()),
				)
			}
		})
	})

	parameterPath := Path(Type[frameParameter]())
	actorCondition := Compile(
		Type[frameActor](),
		Path(Type[frameInventory](), And(Type[frameContains](), frameUUID(1023))),
	)
	compiledDeep := Compile(Path(
		And(Type[frameInventory](), Type[frameTimeRange]()),
		Type[frameContains](),
		Or(Type[frameItem](), Type[frameHolding]()),
	))
	b.Run("Execute", func(b *testing.B) {
		b.Run("SearchPathClean", func(b *testing.B) {
			for range b.N {
				if result := Search(fixture.root, actorCondition); Len(result) != 1 {
					b.Fatal(Len(result))
				}
			}
		})
		b.Run("Clean", func(b *testing.B) {
			for range b.N {
				if result := Follow(fixture.containers, parameterPath); Len(result) != 1024 {
					b.Fatal(Len(result))
				}
			}
		})
		b.Run("Dirty", func(b *testing.B) {
			Set(fixture.dirty, frameVersion(1))
			b.ResetTimer()
			for range b.N {
				if result := Follow(fixture.containers, parameterPath); Len(result) != 1024 {
					b.Fatal(Len(result))
				}
			}
		})
		b.Run("SearchPathDirty", func(b *testing.B) {
			Set(fixture.dirty, frameVersion(2))
			b.ResetTimer()
			for range b.N {
				if result := Search(fixture.root, actorCondition); Len(result) != 1 {
					b.Fatal(Len(result))
				}
			}
		})
		b.Run("CompiledDeep", func(b *testing.B) {
			for range b.N {
				if result := Follow(fixture.containers, compiledDeep); Len(result) != 1024 {
					b.Fatal(Len(result))
				}
			}
		})
		b.Run("RebuiltDeep", func(b *testing.B) {
			for range b.N {
				condition := Path(
					And(Type[frameInventory](), Type[frameTimeRange]()),
					Type[frameContains](),
					Or(Type[frameItem](), Type[frameHolding]()),
				)
				if result := Follow(fixture.containers, condition); Len(result) != 1024 {
					b.Fatal(Len(result))
				}
			}
		})
	})
}
