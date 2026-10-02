package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// node is a JSON value that keeps object member order, which generated
// structs and types follow.
type node struct {
	members []member // object
	items   []*node  // array
	value   any      // scalar: string, json.Number, bool or nil
	isObj   bool
	isArr   bool
}

type member struct {
	key string
	val *node
}

func parseNode(raw []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := readNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data")
	}
	return n, nil
}

func readNode(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &node{isObj: true}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.members = append(n.members, member{k.(string), v})
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := &node{isArr: true}
			for dec.More() {
				v, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, v)
			}
			_, err := dec.Token()
			return n, err
		}
	}
	return &node{value: tok}, nil
}

func (n *node) get(key string) *node {
	if n == nil {
		return nil
	}
	for _, m := range n.members {
		if m.key == key {
			return m.val
		}
	}
	return nil
}

func (n *node) str(key string) string {
	if v := n.get(key); v != nil {
		if s, ok := v.value.(string); ok {
			return s
		}
	}
	return ""
}

func (n *node) flag(key string) bool {
	v := n.get(key)
	return v != nil && v.value == true
}

func (n *node) num(key string) (json.Number, bool) {
	v := n.get(key)
	if v == nil {
		return "", false
	}
	num, ok := v.value.(json.Number)
	return num, ok
}

func (n *node) strings(key string) []string {
	var out []string
	for _, item := range n.get(key).itemsOrNil() {
		if s, ok := item.value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (n *node) itemsOrNil() []*node {
	if n == nil {
		return nil
	}
	return n.items
}

// refName is the definition a "$ref": "#/$defs/Name" points at.
func (n *node) refName() string {
	return strings.TrimPrefix(n.str("$ref"), "#/$defs/")
}

// nonNull is the non-null branch of a {"anyOf": [X, {"type": "null"}]}
// schema, or nil when n is not one.
func (n *node) nonNull() *node {
	any := n.get("anyOf")
	if any == nil || len(any.items) != 2 {
		return nil
	}
	for i, b := range any.items {
		if b.str("type") == "null" {
			return any.items[1-i]
		}
	}
	return nil
}
