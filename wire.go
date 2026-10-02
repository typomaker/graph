package graph

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

const (
	wireVersion    = uint16(1)
	wireRecordNode = byte(1)
	wireRecordRoot = byte(2)
)

var wireMagic = [4]byte{'G', 'R', 'W', 'U'}

// Definition associates a stable wire label with a graph attribute type.
// Definitions made independently with the same label and Go representation
// are compatible.
type Definition struct {
	label string
	typ   reflect.Type
}

// Define declares the stable wire label of graph attribute type T.
func Define[T any](label string) Definition {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	if typ.Kind() == reflect.Pointer {
		panic("graph: pointer wire definition")
	}
	if typ.Kind() == reflect.Interface || !typ.Comparable() {
		panic(fmt.Sprintf("graph: wire type %v is not a comparable value", typ))
	}
	if label == "" {
		panic("graph: empty wire label")
	}
	return Definition{label: label, typ: typ}
}

// Wire encodes and applies graph updates using stable definition labels.
type Wire struct {
	byType  map[reflect.Type]Definition
	byLabel map[string]Definition
}

// NewWire creates a wire codec. Invalid, duplicate, or conflicting
// definitions are programming errors and cause NewWire to panic.
func NewWire(definitions ...Definition) Wire {
	w := Wire{
		byType:  make(map[reflect.Type]Definition, len(definitions)),
		byLabel: make(map[string]Definition, len(definitions)),
	}
	for _, definition := range definitions {
		if definition.typ == nil || definition.label == "" {
			panic("graph: invalid zero wire definition")
		}
		if strings.IndexByte(definition.label, 0) >= 0 {
			panic("graph: wire label contains NUL")
		}
		if previous, ok := w.byType[definition.typ]; ok {
			panic(fmt.Sprintf("graph: wire type %v has labels %q and %q", definition.typ, previous.label, definition.label))
		}
		if previous, ok := w.byLabel[definition.label]; ok {
			panic(fmt.Sprintf("graph: wire label %q is used by %v and %v", definition.label, previous.typ, definition.typ))
		}
		w.byType[definition.typ] = definition
		w.byLabel[definition.label] = definition
	}
	return w
}

type wireAttribute struct {
	label string
	data  []byte
}

type wireRemovedChild struct {
	key   uint64
	attrs []wireAttribute
}

// ExportUpdate encodes changes in g since its Commit baseline. Exporting does
// not advance or otherwise modify that baseline.
func (w Wire) ExportUpdate(g Graph) ([]byte, error) {
	if w.byType == nil || w.byLabel == nil {
		return nil, errors.New("graph: zero Wire")
	}
	delta := g
	if delta.view == nil {
		delta = Delta(g)
	}

	state.RLock()
	defer state.RUnlock()

	var records [][]byte
	if delta.view != nil {
		nodes := reachableViewLocked(selected(delta), delta.view)
		for _, n := range nodes {
			change := delta.view.changes[n]
			record, err := w.encodeNode(n.key, change, delta.view.edges[n])
			if err != nil {
				return nil, err
			}
			records = append(records, frameWireRecord(wireRecordNode, record))
		}
		for _, root := range selected(delta) {
			var payload [8]byte
			binary.BigEndian.PutUint64(payload[:], root.key)
			records = append(records, frameWireRecord(wireRecordRoot, payload[:]))
		}
	}

	var out bytes.Buffer
	out.Write(wireMagic[:])
	_ = binary.Write(&out, binary.BigEndian, wireVersion)
	_ = binary.Write(&out, binary.BigEndian, uint32(len(records)))
	for _, record := range records {
		out.Write(record)
	}
	return out.Bytes(), nil
}

func (w Wire) encodeNode(key uint64, change nodeChange, children []*node) ([]byte, error) {
	attrs, err := w.encodeAttributes(change.attrs)
	if err != nil {
		return nil, err
	}
	snapshot, err := w.encodeAttributes(change.snapshot)
	if err != nil {
		return nil, err
	}
	removedAttrs := make([]string, 0, len(change.removedAttrs))
	for _, typ := range change.removedAttrs {
		definition, ok := w.byType[typ]
		if !ok {
			return nil, fmt.Errorf("graph: attribute type %v is not in Wire schema", typ)
		}
		removedAttrs = append(removedAttrs, definition.label)
	}
	sort.Strings(removedAttrs)
	linked := make([]uint64, 0, len(children))
	added := make([]uint64, 0, len(children))
	for _, child := range children {
		linked = append(linked, child.key)
		if _, ok := change.addedChildren[child.key]; ok {
			added = append(added, child.key)
		}
	}
	removed := make([]wireRemovedChild, 0, len(change.removedChildren))
	for _, child := range change.removedChildren {
		encoded, encodeErr := w.encodeAttributes(child.attrs)
		if encodeErr != nil {
			return nil, encodeErr
		}
		removed = append(removed, wireRemovedChild{key: child.key, attrs: encoded})
	}

	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, key)
	writeWireAttributes(&payload, attrs)
	writeWireStrings(&payload, removedAttrs)
	writeWireAttributes(&payload, snapshot)
	writeWireUint64s(&payload, linked)
	writeWireUint64s(&payload, added)
	writeWireRemovedChildren(&payload, removed)
	return payload.Bytes(), nil
}

func (w Wire) encodeAttributes(attrs map[reflect.Type]any) ([]wireAttribute, error) {
	encoded := make([]wireAttribute, 0, len(attrs))
	for typ, value := range attrs {
		definition, ok := w.byType[typ]
		if !ok {
			return nil, fmt.Errorf("graph: attribute type %v is not in Wire schema", typ)
		}
		data, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("graph: encode wire attribute %q: %w", definition.label, err)
		}
		encoded = append(encoded, wireAttribute{label: definition.label, data: data})
	}
	sort.Slice(encoded, func(i, j int) bool { return encoded[i].label < encoded[j].label })
	return encoded, nil
}

func frameWireRecord(kind byte, payload []byte) []byte {
	record := make([]byte, 5+len(payload))
	record[0] = kind
	binary.BigEndian.PutUint32(record[1:5], uint32(len(payload)))
	copy(record[5:], payload)
	return record
}

// ImportUpdate decodes and applies an update to g. Unknown definitions are
// skipped. Unknown data is not retained for a later re-export.
func (w Wire) ImportUpdate(g Graph, data []byte) (err error) {
	if w.byType == nil || w.byLabel == nil {
		return errors.New("graph: zero Wire")
	}
	decoded, roots, err := w.decodeUpdate(data)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return nil
	}
	if len(selected(g)) == 0 {
		return errors.New("graph: cannot import a non-empty update into an empty Graph value")
	}

	state.Lock()
	defer state.Unlock()
	keyMap, err := resolveWireTargetsLocked(g, decoded, roots)
	if err != nil {
		return err
	}
	for key := range decoded {
		if _, resolved := keyMap[key]; resolved {
			continue
		}
		state.nextID++
		keyMap[key] = state.nextID
	}
	deltaNodes := make(map[uint64]*node, len(decoded))
	view := &subgraph{edges: make(map[*node][]*node), changes: make(map[*node]nodeChange)}
	for key, decodedNode := range decoded {
		if len(decodedNode.snapshot) == 0 {
			continue
		}
		wireKey := key
		if targetKey, ok := keyMap[key]; ok {
			wireKey = targetKey
		}
		n := newNodeLocked(wireKey, cloneAttrs(decodedNode.snapshot), 0)
		n.key = wireKey
		deltaNodes[key] = n
		change := decodedNode.change
		change.addedChildren = make(map[uint64]struct{}, len(decodedNode.change.addedChildren))
		for childKey := range decodedNode.change.addedChildren {
			if targetKey, ok := keyMap[childKey]; ok {
				childKey = targetKey
			}
			change.addedChildren[childKey] = struct{}{}
		}
		for i := range change.removedChildren {
			if targetKey, ok := keyMap[change.removedChildren[i].key]; ok {
				change.removedChildren[i].key = targetKey
			}
		}
		view.changes[n] = change
	}
	for key, decodedNode := range decoded {
		parent := deltaNodes[key]
		if parent == nil {
			continue
		}
		for _, childKey := range decodedNode.children {
			if child := deltaNodes[childKey]; child != nil {
				view.edges[parent] = append(view.edges[parent], child)
			}
		}
	}
	deltaRoots := make([]*node, 0, len(roots))
	for _, key := range roots {
		if root := deltaNodes[key]; root != nil {
			deltaRoots = append(deltaRoots, root)
		}
	}
	if len(deltaRoots) == 0 {
		return nil
	}
	// Apply cannot return an error and validates cycles before changing the
	// target. Convert its invariant panics into an import error.
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("graph: cannot apply wire update: %v", recovered)
		}
	}()
	applyLocked(g, Graph{nodes: deltaRoots, view: view})
	return nil
}

func resolveWireTargetsLocked(g Graph, decoded map[uint64]decodedWireNode, roots []uint64) (map[uint64]uint64, error) {
	targetRoots := selected(g)
	if len(roots) > len(targetRoots) {
		return nil, errors.New("graph: update has more roots than the import scope")
	}
	resolved := make(map[uint64]uint64)
	for i, key := range roots {
		resolved[key] = targetRoots[i].key
	}
	targets := reachableLocked(targetRoots)
	for key, decodedNode := range decoded {
		if _, ok := resolved[key]; ok {
			continue
		}
		stable := cloneAttrs(decodedNode.snapshot)
		for typ := range decodedNode.change.attrs {
			delete(stable, typ)
		}
		for _, typ := range decodedNode.change.removedAttrs {
			delete(stable, typ)
		}
		if len(stable) == 0 {
			continue
		}
		var match *node
		for _, candidate := range targets {
			if !wireAttrsMatch(candidate.attrs, stable) {
				continue
			}
			if match != nil {
				match = nil
				break
			}
			match = candidate
		}
		if match != nil {
			resolved[key] = match.key
		}
	}
	for _, decodedNode := range decoded {
		for _, removed := range decodedNode.change.removedChildren {
			if _, ok := resolved[removed.key]; ok || len(removed.attrs) == 0 {
				continue
			}
			var match *node
			for _, candidate := range targets {
				if !wireAttrsMatch(candidate.attrs, removed.attrs) {
					continue
				}
				if match != nil {
					match = nil
					break
				}
				match = candidate
			}
			if match != nil {
				resolved[removed.key] = match.key
			}
		}
	}
	// A reference to an unchanged node is an external/baseline reference. It
	// must resolve; otherwise silently creating a partial substitute would
	// corrupt topology.
	for key, decodedNode := range decoded {
		for _, childKey := range decodedNode.children {
			if _, added := decodedNode.change.addedChildren[childKey]; !added {
				continue
			}
			child := decoded[childKey]
			if len(child.snapshot) > 0 && len(child.change.attrs) == 0 && len(child.change.removedAttrs) == 0 {
				if _, ok := resolved[childKey]; !ok {
					return nil, fmt.Errorf("graph: cannot resolve external wire reference %d from node %d", childKey, key)
				}
			}
		}
	}
	return resolved, nil
}

func wireAttrsMatch(candidate, stable map[reflect.Type]any) bool {
	for typ, value := range stable {
		if candidateValue, ok := candidate[typ]; !ok || candidateValue != value {
			return false
		}
	}
	return true
}

type decodedWireNode struct {
	snapshot map[reflect.Type]any
	change   nodeChange
	children []uint64
}

func (w Wire) decodeUpdate(data []byte) (map[uint64]decodedWireNode, []uint64, error) {
	r := bytes.NewReader(data)
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || magic != wireMagic {
		return nil, nil, errors.New("graph: invalid wire update header")
	}
	var version uint16
	var count uint32
	if binary.Read(r, binary.BigEndian, &version) != nil || binary.Read(r, binary.BigEndian, &count) != nil {
		return nil, nil, errors.New("graph: truncated wire update header")
	}
	if version != wireVersion {
		return nil, nil, fmt.Errorf("graph: unsupported wire format version %d", version)
	}
	nodes := make(map[uint64]decodedWireNode)
	var roots []uint64
	for range count {
		kind, err := r.ReadByte()
		if err != nil {
			return nil, nil, errors.New("graph: truncated wire record")
		}
		var length uint32
		if binary.Read(r, binary.BigEndian, &length) != nil || uint64(length) > uint64(r.Len()) {
			return nil, nil, errors.New("graph: truncated wire record")
		}
		payload := make([]byte, length)
		_, _ = io.ReadFull(r, payload)
		switch kind {
		case wireRecordNode:
			key, node, decodeErr := w.decodeNode(payload)
			if decodeErr != nil {
				return nil, nil, decodeErr
			}
			if _, duplicate := nodes[key]; duplicate {
				return nil, nil, fmt.Errorf("graph: duplicate wire node %d", key)
			}
			nodes[key] = node
		case wireRecordRoot:
			if len(payload) != 8 {
				return nil, nil, errors.New("graph: invalid wire root record")
			}
			roots = append(roots, binary.BigEndian.Uint64(payload))
		default:
			// Length framing makes protocol extensions safely skippable.
		}
	}
	if r.Len() != 0 {
		return nil, nil, errors.New("graph: trailing wire update data")
	}
	for _, root := range roots {
		if _, ok := nodes[root]; !ok {
			return nil, nil, fmt.Errorf("graph: wire root %d is missing", root)
		}
	}
	return nodes, roots, nil
}

func (w Wire) decodeNode(data []byte) (uint64, decodedWireNode, error) {
	r := bytes.NewReader(data)
	var key uint64
	if binary.Read(r, binary.BigEndian, &key) != nil {
		return 0, decodedWireNode{}, errors.New("graph: truncated wire node")
	}
	attrs, err := w.readWireAttributes(r)
	if err != nil {
		return 0, decodedWireNode{}, err
	}
	removedLabels, err := readWireStrings(r)
	if err != nil {
		return 0, decodedWireNode{}, err
	}
	snapshot, err := w.readWireAttributes(r)
	if err != nil {
		return 0, decodedWireNode{}, err
	}
	children, err := readWireUint64s(r)
	if err != nil {
		return 0, decodedWireNode{}, err
	}
	addedChildren, err := readWireUint64s(r)
	if err != nil {
		return 0, decodedWireNode{}, err
	}
	removedChildren, err := w.readWireRemovedChildren(r)
	if err != nil {
		return 0, decodedWireNode{}, err
	}
	if r.Len() != 0 {
		return 0, decodedWireNode{}, errors.New("graph: trailing wire node data")
	}
	removed := make([]reflect.Type, 0, len(removedLabels))
	for _, label := range removedLabels {
		if definition, ok := w.byLabel[label]; ok {
			removed = append(removed, definition.typ)
		}
	}
	added := make(map[uint64]struct{}, len(addedChildren))
	for _, child := range addedChildren {
		added[child] = struct{}{}
	}
	return key, decodedWireNode{
		snapshot: snapshot,
		change:   nodeChange{attrs: attrs, removedAttrs: removed, snapshot: snapshot, addedChildren: added, removedChildren: removedChildren},
		children: children,
	}, nil
}

func (w Wire) readWireAttributes(r *bytes.Reader) (map[reflect.Type]any, error) {
	count, err := readWireCount(r)
	if err != nil {
		return nil, err
	}
	attrs := make(map[reflect.Type]any)
	seen := make(map[string]struct{}, count)
	for range count {
		label, err := readWireBytes(r)
		if err != nil {
			return nil, err
		}
		data, err := readWireBytes(r)
		if err != nil {
			return nil, err
		}
		labelString := string(label)
		if _, duplicate := seen[labelString]; duplicate {
			return nil, fmt.Errorf("graph: duplicate wire attribute %q", labelString)
		}
		seen[labelString] = struct{}{}
		definition, known := w.byLabel[labelString]
		if !known {
			continue
		}
		pointer := reflect.New(definition.typ)
		if err := json.Unmarshal(data, pointer.Interface()); err != nil {
			return nil, fmt.Errorf("graph: decode wire attribute %q: %w", label, err)
		}
		attrs[definition.typ] = pointer.Elem().Interface()
	}
	return attrs, nil
}

func (w Wire) readWireRemovedChildren(r *bytes.Reader) ([]removedChildChange, error) {
	count, err := readWireCount(r)
	if err != nil {
		return nil, err
	}
	children := make([]removedChildChange, 0, count)
	for range count {
		var key uint64
		if binary.Read(r, binary.BigEndian, &key) != nil {
			return nil, errors.New("graph: truncated removed child")
		}
		attrs, readErr := w.readWireAttributes(r)
		if readErr != nil {
			return nil, readErr
		}
		children = append(children, removedChildChange{key: key, attrs: attrs})
	}
	return children, nil
}

func writeWireAttributes(w io.Writer, attrs []wireAttribute) {
	_ = binary.Write(w, binary.BigEndian, uint32(len(attrs)))
	for _, attr := range attrs {
		writeWireBytes(w, []byte(attr.label))
		writeWireBytes(w, attr.data)
	}
}

func writeWireStrings(w io.Writer, values []string) {
	_ = binary.Write(w, binary.BigEndian, uint32(len(values)))
	for _, value := range values {
		writeWireBytes(w, []byte(value))
	}
}

func writeWireUint64s(w io.Writer, values []uint64) {
	_ = binary.Write(w, binary.BigEndian, uint32(len(values)))
	for _, value := range values {
		_ = binary.Write(w, binary.BigEndian, value)
	}
}

func writeWireRemovedChildren(w io.Writer, children []wireRemovedChild) {
	_ = binary.Write(w, binary.BigEndian, uint32(len(children)))
	for _, child := range children {
		_ = binary.Write(w, binary.BigEndian, child.key)
		writeWireAttributes(w, child.attrs)
	}
}

func writeWireBytes(w io.Writer, data []byte) {
	_ = binary.Write(w, binary.BigEndian, uint32(len(data)))
	_, _ = w.Write(data)
}

func readWireCount(r *bytes.Reader) (uint32, error) {
	var count uint32
	if binary.Read(r, binary.BigEndian, &count) != nil || uint64(count) > uint64(r.Len()) {
		return 0, errors.New("graph: invalid wire field count")
	}
	return count, nil
}

func readWireBytes(r *bytes.Reader) ([]byte, error) {
	var length uint32
	if binary.Read(r, binary.BigEndian, &length) != nil || uint64(length) > uint64(r.Len()) {
		return nil, errors.New("graph: truncated wire field")
	}
	data := make([]byte, length)
	_, _ = io.ReadFull(r, data)
	return data, nil
}

func readWireStrings(r *bytes.Reader) ([]string, error) {
	count, err := readWireCount(r)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, count)
	for range count {
		value, readErr := readWireBytes(r)
		if readErr != nil {
			return nil, readErr
		}
		values = append(values, string(value))
	}
	return values, nil
}

func readWireUint64s(r *bytes.Reader) ([]uint64, error) {
	count, err := readWireCount(r)
	if err != nil || uint64(count)*8 > uint64(r.Len()) {
		return nil, errors.New("graph: truncated wire integer list")
	}
	values := make([]uint64, count)
	for i := range values {
		_ = binary.Read(r, binary.BigEndian, &values[i])
	}
	return values, nil
}
