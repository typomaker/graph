// Package graphjson encodes and decodes graphs as flat JSON documents.
package graphjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/typomaker/graph"
)

const formatVersion = 1

type document struct {
	Version int               `json:"@version"`
	Roots   []string          `json:"@roots"`
	Nodes   []json.RawMessage `json:"@nodes"`
}

// Marshal returns the flat JSON encoding of g according to schema.
func Marshal(g graph.Graph, schema graph.Schema) ([]byte, error) {
	snapshot, err := graph.Encode(g, schema)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(snapshot.Nodes))
	for i := range keys {
		keys[i] = strconv.Itoa(i + 1)
	}
	doc := document{Version: formatVersion, Roots: make([]string, len(snapshot.Roots)), Nodes: make([]json.RawMessage, len(snapshot.Nodes))}
	for i, ordinal := range snapshot.Roots {
		doc.Roots[i] = keys[ordinal]
	}
	for ordinal, node := range snapshot.Nodes {
		object := make(map[string]any, len(node.Attributes)+2)
		object["@key"] = keys[ordinal]
		if len(node.Links) > 0 {
			links := make([]string, len(node.Links))
			for i, child := range node.Links {
				links[i] = keys[child]
			}
			object["@links"] = links
		}
		for name, value := range node.Attributes {
			object[name] = value
		}
		encoded, marshalErr := json.Marshal(object)
		if marshalErr != nil {
			return nil, fmt.Errorf("graphjson: marshal node %q: %w", keys[ordinal], marshalErr)
		}
		doc.Nodes[ordinal] = encoded
	}
	return json.Marshal(doc)
}

// Unmarshal parses data according to schema and atomically replaces destination.
func Unmarshal(data []byte, destination *graph.Graph, schema graph.Schema) error {
	if destination == nil {
		return fmt.Errorf("graphjson: nil destination")
	}
	if err := graph.ValidateSchema(schema); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var doc document
	if err := decoder.Decode(&doc); err != nil {
		return fmt.Errorf("graphjson: decode document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("graphjson: trailing JSON value")
		}
		return fmt.Errorf("graphjson: decode trailing data: %w", err)
	}
	if doc.Version != formatVersion {
		return fmt.Errorf("graphjson: unsupported version %d", doc.Version)
	}
	if doc.Roots == nil || doc.Nodes == nil {
		return fmt.Errorf("graphjson: @roots and @nodes must be arrays")
	}

	keys := make(map[string]int, len(doc.Nodes))
	snapshot := graph.EncodedGraph{Roots: make([]int, len(doc.Roots)), Nodes: make([]graph.EncodedNode, len(doc.Nodes))}
	type pendingNode struct {
		links []string
	}
	pending := make([]pendingNode, len(doc.Nodes))
	for ordinal, raw := range doc.Nodes {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return fmt.Errorf("graphjson: decode node %d: %w", ordinal+1, err)
		}
		var key string
		if rawKey, ok := object["@key"]; !ok {
			return fmt.Errorf("graphjson: node %d has no @key", ordinal+1)
		} else if err := json.Unmarshal(rawKey, &key); err != nil || key == "" {
			return fmt.Errorf("graphjson: node %d has invalid @key", ordinal+1)
		}
		if _, exists := keys[key]; exists {
			return fmt.Errorf("graphjson: duplicate @key %q", key)
		}
		keys[key] = ordinal
		delete(object, "@key")
		if rawLinks, ok := object["@links"]; ok {
			if err := json.Unmarshal(rawLinks, &pending[ordinal].links); err != nil {
				return fmt.Errorf("graphjson: node %q has invalid @links", key)
			}
			delete(object, "@links")
		}
		attributes := make(map[string]any, len(object))
		for name, rawValue := range object {
			if strings.HasPrefix(name, "@") {
				return fmt.Errorf("graphjson: unknown reserved field %q", name)
			}
			definition, ok := graph.SchemaDefinition(schema, name)
			if !ok {
				return fmt.Errorf("graphjson: attribute %q is not in schema", name)
			}
			pointer := definition.NewValue()
			if err := json.Unmarshal(rawValue, pointer); err != nil {
				return fmt.Errorf("graphjson: decode attribute %q of node %q: %w", name, key, err)
			}
			attributes[name] = definition.Value(pointer)
		}
		snapshot.Nodes[ordinal].Attributes = attributes
	}
	for ordinal := range snapshot.Nodes {
		links := pending[ordinal].links
		snapshot.Nodes[ordinal].Links = make([]int, len(links))
		for i, key := range links {
			child, ok := keys[key]
			if !ok {
				return fmt.Errorf("graphjson: link references unknown @key %q", key)
			}
			snapshot.Nodes[ordinal].Links[i] = child
		}
	}
	for i, key := range doc.Roots {
		ordinal, ok := keys[key]
		if !ok {
			return fmt.Errorf("graphjson: root references unknown @key %q", key)
		}
		snapshot.Roots[i] = ordinal
	}
	if err := validateReachability(snapshot); err != nil {
		return err
	}
	restored, err := graph.Decode(snapshot)
	if err != nil {
		return fmt.Errorf("graphjson: %w", err)
	}
	*destination = restored
	return nil
}

func validateReachability(snapshot graph.EncodedGraph) error {
	seen := make([]bool, len(snapshot.Nodes))
	visiting := make([]bool, len(snapshot.Nodes))
	rootSet := make(map[int]struct{}, len(snapshot.Roots))
	var visit func(int) error
	visit = func(node int) error {
		if visiting[node] {
			return fmt.Errorf("graphjson: graph contains a cycle")
		}
		if seen[node] {
			return nil
		}
		visiting[node] = true
		seen[node] = true
		children := make(map[int]struct{}, len(snapshot.Nodes[node].Links))
		for _, child := range snapshot.Nodes[node].Links {
			if _, duplicate := children[child]; duplicate {
				return fmt.Errorf("graphjson: node %d contains duplicate link to node %d", node+1, child+1)
			}
			children[child] = struct{}{}
			if err := visit(child); err != nil {
				return err
			}
		}
		visiting[node] = false
		return nil
	}
	for _, root := range snapshot.Roots {
		if _, duplicate := rootSet[root]; duplicate {
			return fmt.Errorf("graphjson: duplicate root node %d", root+1)
		}
		rootSet[root] = struct{}{}
		if err := visit(root); err != nil {
			return err
		}
	}
	for node, reachable := range seen {
		if !reachable {
			return fmt.Errorf("graphjson: node %d is not reachable from @roots", node+1)
		}
	}
	return nil
}
