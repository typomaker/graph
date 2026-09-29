package graph

import (
	"fmt"
	"reflect"
	"strings"
)

// Schema describes the stable external names of graph attribute types.
//
// A schema is normally declared as a package variable:
//
//	var schema = graph.Schema{
//		graph.Attribute[ID]("id"),
//		graph.Attribute[Name]("name"),
//	}
type Schema []AttributeDefinition

// AttributeDefinition associates one external name with one graph attribute
// type. Use Attribute to create definitions.
type AttributeDefinition struct {
	name string
	typ  reflect.Type
}

// Attribute declares the external name of an attribute type in a Schema.
func Attribute[T any](name string) AttributeDefinition {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	if typ.Kind() == reflect.Pointer {
		panic("graph: pointer schema attribute")
	}
	if typ.Kind() == reflect.Interface || !typ.Comparable() {
		panic(fmt.Sprintf("graph: schema attribute %v is not a comparable value", typ))
	}
	if strings.HasPrefix(name, "@") {
		panic("graph: schema attribute names starting with @ are reserved")
	}
	if name == "" {
		panic("graph: empty schema attribute name")
	}
	return AttributeDefinition{name: name, typ: typ}
}

// Name returns the external attribute name.
func (definition AttributeDefinition) Name() string { return definition.name }

// NewValue returns a pointer to a new value of the declared attribute type.
// Encoding packages can decode into the returned value.
func (definition AttributeDefinition) NewValue() any {
	return reflect.New(definition.typ).Interface()
}

// Value returns the decoded value held by a pointer returned by NewValue.
func (definition AttributeDefinition) Value(pointer any) any {
	value := reflect.ValueOf(pointer)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Type() != definition.typ {
		panic("graph: invalid schema attribute value")
	}
	return value.Elem().Interface()
}

func (schema Schema) definitions() (map[reflect.Type]AttributeDefinition, map[string]AttributeDefinition, error) {
	byType := make(map[reflect.Type]AttributeDefinition, len(schema))
	byName := make(map[string]AttributeDefinition, len(schema))
	for _, definition := range schema {
		if definition.typ == nil || definition.name == "" {
			return nil, nil, fmt.Errorf("graph: invalid zero schema attribute definition")
		}
		if strings.HasPrefix(definition.name, "@") {
			return nil, nil, fmt.Errorf("graph: schema attribute name %q is reserved", definition.name)
		}
		if previous, exists := byType[definition.typ]; exists {
			return nil, nil, fmt.Errorf("graph: attribute type %v has names %q and %q", definition.typ, previous.name, definition.name)
		}
		if previous, exists := byName[definition.name]; exists {
			return nil, nil, fmt.Errorf("graph: schema name %q is used by %v and %v", definition.name, previous.typ, definition.typ)
		}
		byType[definition.typ] = definition
		byName[definition.name] = definition
	}
	return byType, byName, nil
}

// ValidateSchema reports an invalid or ambiguous schema definition.
func ValidateSchema(schema Schema) error {
	_, _, err := schema.definitions()
	return err
}

// SchemaDefinition returns the attribute definition with the given external
// name. It reports false for an invalid schema or an unknown name.
func SchemaDefinition(schema Schema, name string) (AttributeDefinition, bool) {
	_, byName, err := schema.definitions()
	if err != nil {
		return AttributeDefinition{}, false
	}
	definition, ok := byName[name]
	return definition, ok
}

// EncodedGraph is a format-neutral snapshot used by graph encoding packages.
// Node and link indexes refer to positions in Nodes.
type EncodedGraph struct {
	Roots []int
	Nodes []EncodedNode
}

// EncodedNode is one node in an EncodedGraph snapshot.
type EncodedNode struct {
	Attributes map[string]any
	Links      []int
}

// Encode creates a consistent snapshot of g using schema attribute names.
func Encode(g Graph, schema Schema) (EncodedGraph, error) {
	byType, _, err := schema.definitions()
	if err != nil {
		return EncodedGraph{}, err
	}

	state.RLock()
	defer state.RUnlock()

	roots := append([]*node(nil), selected(g)...)
	nodes := make([]*node, 0)
	seen := make(map[*node]struct{})
	var visit func(*node)
	visit = func(n *node) {
		if _, exists := seen[n]; exists {
			return
		}
		seen[n] = struct{}{}
		nodes = append(nodes, n)
		for _, child := range selectedChildrenLocked(n, g.view) {
			visit(child)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	ordinals := make(map[*node]int, len(nodes))
	for ordinal, n := range nodes {
		ordinals[n] = ordinal
	}

	encoded := EncodedGraph{
		Roots: make([]int, len(roots)),
		Nodes: make([]EncodedNode, len(nodes)),
	}
	for i, root := range roots {
		encoded.Roots[i] = ordinals[root]
	}
	for ordinal, n := range nodes {
		attributes := make(map[string]any, len(n.attrs))
		for typ, value := range n.attrs {
			definition, ok := byType[typ]
			if !ok {
				return EncodedGraph{}, fmt.Errorf("graph: attribute type %v is not in schema", typ)
			}
			attributes[definition.name] = value
		}
		children := selectedChildrenLocked(n, g.view)
		links := make([]int, len(children))
		for i, child := range children {
			links[i] = ordinals[child]
		}
		encoded.Nodes[ordinal] = EncodedNode{Attributes: attributes, Links: links}
	}
	return encoded, nil
}

// Decode builds an independent graph from a validated format-neutral snapshot.
func Decode(encoded EncodedGraph) (Graph, error) {
	state.Lock()
	defer state.Unlock()

	nodes := make([]*node, len(encoded.Nodes))
	for ordinal, encodedNode := range encoded.Nodes {
		values := make([]any, 0, len(encodedNode.Attributes))
		for _, value := range encodedNode.Attributes {
			values = append(values, value)
		}
		if len(values) == 0 {
			return Graph{}, fmt.Errorf("graph: node %d has no attributes", ordinal)
		}
		attrs := storedAttributes(values)
		state.nextID++
		revision := nextRevisionLocked()
		nodes[ordinal] = newNodeLocked(state.nextID, attrs, revision)
	}
	for ordinal, encodedNode := range encoded.Nodes {
		parent := nodes[ordinal]
		for _, childOrdinal := range encodedNode.Links {
			if childOrdinal < 0 || childOrdinal >= len(nodes) {
				return Graph{}, fmt.Errorf("graph: node %d links to invalid node %d", ordinal, childOrdinal)
			}
			child := nodes[childOrdinal]
			if child == parent || reachesLocked(child, parent) {
				return Graph{}, fmt.Errorf("graph: encoded graph contains a cycle")
			}
			if _, exists := parent.children[child]; exists {
				return Graph{}, fmt.Errorf("graph: node %d contains duplicate link %d", ordinal, childOrdinal)
			}
			revision := nextRevisionLocked()
			addChildLocked(parent, child, revision)
			child.parents[parent] = struct{}{}
		}
	}
	roots := make([]*node, len(encoded.Roots))
	for i, ordinal := range encoded.Roots {
		if ordinal < 0 || ordinal >= len(nodes) {
			return Graph{}, fmt.Errorf("graph: invalid root %d", ordinal)
		}
		roots[i] = nodes[ordinal]
	}
	return Graph{nodes: roots}, nil
}
