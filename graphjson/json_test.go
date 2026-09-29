package graphjson_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/typomaker/graph"
	"github.com/typomaker/graph/graphjson"
)

type ID string
type Name string
type Number int

type Coded string

func (value Coded) MarshalJSON() ([]byte, error) {
	return json.Marshal("coded:" + string(value))
}

func (value *Coded) UnmarshalJSON(data []byte) error {
	var encoded string
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	*value = Coded(strings.TrimPrefix(encoded, "coded:"))
	return nil
}

var schema = graph.Schema{
	graph.Attribute[ID]("id"),
	graph.Attribute[Name]("name"),
	graph.Attribute[Number]("number"),
	graph.Attribute[Coded]("coded"),
}

func TestRoundTripFlatGraph(t *testing.T) {
	first := graph.New(ID("first"), Name("first node"), Coded("value"))
	second := graph.New(ID("second"), Number(2))
	shared := graph.New(ID("shared"), Name("shared node"))
	graph.Link(first, shared)
	graph.Link(second, shared)
	original := graph.Union(first, second)

	data, err := graphjson.Marshal(original, schema)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"@version":1,"@roots":["1","3"],"@nodes":[{"@key":"1","@links":["2"],"coded":"coded:value","id":"first","name":"first node"},{"@key":"2","id":"shared","name":"shared node"},{"@key":"3","@links":["2"],"id":"second","number":2}]}`
	if string(data) != want {
		t.Fatalf("Marshal() = %s\nwant      %s", data, want)
	}

	restored := graph.New(ID("old"))
	if err := graphjson.Unmarshal(data, &restored, schema); err != nil {
		t.Fatal(err)
	}
	if graph.Len(restored) != 2 {
		t.Fatalf("restored roots = %d", graph.Len(restored))
	}
	roots := make([]graph.Graph, 0, 2)
	for root := range graph.Each(restored) {
		roots = append(roots, root)
	}
	if id, _ := graph.Get[ID](roots[0]); id != "first" {
		t.Fatalf("first ID = %q", id)
	}
	if coded, _ := graph.Get[Coded](roots[0]); coded != "value" {
		t.Fatalf("coded = %q", coded)
	}
	firstChildren := graph.Follow(roots[0], graph.Path(graph.Type[any]()))
	secondChildren := graph.Follow(roots[1], graph.Path(graph.Type[any]()))
	if graph.Len(graph.Intersect(firstChildren, secondChildren)) != 1 {
		t.Fatal("shared child identity was not preserved")
	}
}

func TestEmptyGraph(t *testing.T) {
	data, err := graphjson.Marshal(graph.Graph{}, schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"@version":1,"@roots":[],"@nodes":[]}` {
		t.Fatalf("Marshal() = %s", data)
	}
	var restored graph.Graph
	if err := graphjson.Unmarshal(data, &restored, schema); err != nil {
		t.Fatal(err)
	}
	if !graph.Empty(restored) {
		t.Fatal("decoded empty graph is not empty")
	}
}

func TestUnmarshalErrorsLeaveDestinationUnchanged(t *testing.T) {
	tests := []string{
		``,
		`{} {}`,
		`{"@version":2,"@roots":[],"@nodes":[]}`,
		`{"@version":1,"@roots":null,"@nodes":[]}`,
		`{"@version":1,"@roots":[],"@nodes":null}`,
		`{"@version":1,"@roots":[],"@nodes":[],"extra":true}`,
		`{"@version":1,"@roots":["missing"],"@nodes":[]}`,
		`{"@version":1,"@roots":[],"@nodes":[{"id":"1"}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":1,"id":"1"}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","@links":1,"id":"1"}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","@unknown":true,"id":"1"}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","unknown":true}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","number":"bad"}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","id":"1","@links":["missing"]}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","id":"1"},{"@key":"1","id":"2"}]}`,
		`{"@version":1,"@roots":["1","1"],"@nodes":[{"@key":"1","id":"1"}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","id":"1","@links":["2","2"]},{"@key":"2","id":"2"}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","id":"1","@links":["2"]},{"@key":"2","id":"2","@links":["1"]}]}`,
		`{"@version":1,"@roots":["1"],"@nodes":[{"@key":"1","id":"1"},{"@key":"2","id":"2"}]}`,
	}
	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			destination := graph.New(ID("kept"))
			if err := graphjson.Unmarshal([]byte(source), &destination, schema); err == nil {
				t.Fatal("Unmarshal succeeded")
			}
			if id, _ := graph.Get[ID](destination); id != "kept" {
				t.Fatalf("destination changed to %q", id)
			}
		})
	}
	if err := graphjson.Unmarshal([]byte(`{}`), nil, schema); err == nil {
		t.Fatal("nil destination was accepted")
	}
	badSchema := graph.Schema{graph.Attribute[ID]("value"), graph.Attribute[Name]("value")}
	var destination graph.Graph
	if err := graphjson.Unmarshal([]byte(`{}`), &destination, badSchema); err == nil {
		t.Fatal("invalid schema was accepted")
	}
}

func TestMarshalErrors(t *testing.T) {
	if _, err := graphjson.Marshal(graph.New(ID("1"), struct{ Value int }{1}), schema); err == nil {
		t.Fatal("unknown attribute type was accepted")
	}
	badSchema := graph.Schema{graph.Attribute[ID]("value"), graph.Attribute[Name]("value")}
	if _, err := graphjson.Marshal(graph.New(ID("1")), badSchema); err == nil {
		t.Fatal("invalid schema was accepted")
	}
}
