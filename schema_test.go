package graph

import (
	"reflect"
	"testing"
)

type schemaID string
type schemaName string

func TestSchemaAttribute(t *testing.T) {
	definition := Attribute[schemaID]("id")
	if definition.Name() != "id" {
		t.Fatalf("Name() = %q", definition.Name())
	}
	pointer := definition.NewValue()
	reflect.ValueOf(pointer).Elem().SetString("42")
	if value := definition.Value(pointer); value != schemaID("42") {
		t.Fatalf("Value() = %#v", value)
	}
	if found, ok := SchemaDefinition(Schema{definition}, "id"); !ok || found.Name() != "id" {
		t.Fatalf("SchemaDefinition() = %#v, %v", found, ok)
	}
}

func TestSchemaAttributePanics(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{"empty name", func() { Attribute[schemaID]("") }},
		{"reserved name", func() { Attribute[schemaID]("@id") }},
		{"pointer", func() { Attribute[*schemaID]("id") }},
		{"interface", func() { Attribute[any]("value") }},
		{"non-comparable", func() { Attribute[[]int]("values") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("did not panic")
				}
			}()
			test.call()
		})
	}
}

func TestValidateSchema(t *testing.T) {
	tests := []Schema{
		{{}},
		{Attribute[schemaID]("id"), Attribute[schemaID]("other")},
		{Attribute[schemaID]("value"), Attribute[schemaName]("value")},
	}
	for _, schema := range tests {
		if ValidateSchema(schema) == nil {
			t.Fatalf("ValidateSchema(%#v) succeeded", schema)
		}
	}
	if ValidateSchema(Schema{Attribute[schemaID]("id")}) != nil {
		t.Fatal("valid schema was rejected")
	}
	if _, ok := SchemaDefinition(Schema{{}}, "id"); ok {
		t.Fatal("invalid schema returned a definition")
	}
}

func TestSchemaValuePanicsForWrongPointer(t *testing.T) {
	definition := Attribute[schemaID]("id")
	defer func() {
		if recover() == nil {
			t.Fatal("did not panic")
		}
	}()
	definition.Value(new(string))
}

func TestEncodeRejectsUnknownAttributeAndInvalidSchema(t *testing.T) {
	g := New(schemaID("1"), schemaName("root"))
	if _, err := Encode(g, Schema{Attribute[schemaID]("id")}); err == nil {
		t.Fatal("Encode accepted an unknown attribute")
	}
	if _, err := Encode(g, Schema{{}}); err == nil {
		t.Fatal("Encode accepted an invalid schema")
	}
}

func TestDecodeRejectsInvalidSnapshots(t *testing.T) {
	validNode := EncodedNode{Attributes: map[string]any{"id": schemaID("1")}}
	tests := []EncodedGraph{
		{Nodes: []EncodedNode{{}}},
		{Nodes: []EncodedNode{validNode}, Roots: []int{1}},
		{Nodes: []EncodedNode{{Attributes: validNode.Attributes, Links: []int{1}}}},
		{Nodes: []EncodedNode{{Attributes: validNode.Attributes, Links: []int{0}}}},
		{Nodes: []EncodedNode{{Attributes: validNode.Attributes, Links: []int{1}}, validNode}, Roots: []int{0}},
	}
	// Make the last graph cyclic instead of merely linked.
	tests[len(tests)-1].Nodes[1].Links = []int{0}
	for _, encoded := range tests {
		if _, err := Decode(encoded); err == nil {
			t.Fatalf("Decode(%#v) succeeded", encoded)
		}
	}
}
