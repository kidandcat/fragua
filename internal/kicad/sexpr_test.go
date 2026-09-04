package kicad

import (
	"fmt"
	"strings"
	"unicode"
)

// node is a parsed s-expression: either an atom or a list.
type node struct {
	atom string
	list []*node
}

func (n *node) tag() string {
	if n == nil || len(n.list) == 0 {
		return ""
	}
	return n.list[0].atom
}

// find returns every direct child list with the given tag.
func (n *node) find(tag string) []*node {
	var out []*node
	for _, c := range n.list {
		if c.tag() == tag {
			out = append(out, c)
		}
	}
	return out
}

// first returns the first direct child list with the given tag, or nil.
func (n *node) first(tag string) *node {
	if c := n.find(tag); len(c) > 0 {
		return c[0]
	}
	return nil
}

// arg returns the i-th element after the tag as a plain string.
func (n *node) arg(i int) string {
	if n == nil || len(n.list) <= i+1 {
		return ""
	}
	return n.list[i+1].atom
}

// findAll walks the whole tree collecting lists with the given tag.
func (n *node) findAll(tag string) []*node {
	var out []*node
	var walk func(*node)
	walk = func(x *node) {
		if x.tag() == tag {
			out = append(out, x)
		}
		for _, c := range x.list {
			walk(c)
		}
	}
	walk(n)
	return out
}

// parseSexpr is a minimal reader: enough to check the structure Fragua emits.
func parseSexpr(s string) (*node, error) {
	p := &parser{src: []rune(s)}
	p.skip()
	n, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("trailing input at %d", p.pos)
	}
	return n, nil
}

type parser struct {
	src []rune
	pos int
}

func (p *parser) skip() {
	for p.pos < len(p.src) && unicode.IsSpace(p.src[p.pos]) {
		p.pos++
	}
}

func (p *parser) value() (*node, error) {
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("unexpected end of input")
	}
	switch c := p.src[p.pos]; {
	case c == '(':
		p.pos++
		n := &node{}
		for {
			p.skip()
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("unterminated list")
			}
			if p.src[p.pos] == ')' {
				p.pos++
				return n, nil
			}
			child, err := p.value()
			if err != nil {
				return nil, err
			}
			n.list = append(n.list, child)
		}
	case c == '"':
		p.pos++
		var b strings.Builder
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if c == '\\' && p.pos+1 < len(p.src) {
				b.WriteRune(p.src[p.pos+1])
				p.pos += 2
				continue
			}
			p.pos++
			if c == '"' {
				return &node{atom: b.String()}, nil
			}
			b.WriteRune(c)
		}
		return nil, fmt.Errorf("unterminated string")
	case c == ')':
		return nil, fmt.Errorf("unexpected )")
	default:
		start := p.pos
		for p.pos < len(p.src) && !unicode.IsSpace(p.src[p.pos]) && p.src[p.pos] != '(' && p.src[p.pos] != ')' {
			p.pos++
		}
		return &node{atom: string(p.src[start:p.pos])}, nil
	}
}
