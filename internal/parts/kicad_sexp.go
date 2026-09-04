package parts

import (
	"fmt"
	"strconv"
	"strings"
)

// sexp is a KiCad s-expression node: either an atom or a list whose first
// element is conventionally the node name. Hand-rolled so the tool keeps its
// pure-Go, zero-dependency promise (VISION.md).
type sexp struct {
	atom string
	list []sexp
	isL  bool
}

func (s sexp) name() string {
	if !s.isL || len(s.list) == 0 || s.list[0].isL {
		return ""
	}
	return s.list[0].atom
}

// children returns every child list named name.
func (s sexp) children(name string) []sexp {
	if !s.isL {
		return nil
	}
	var out []sexp
	for _, c := range s.list {
		if c.isL && c.name() == name {
			out = append(out, c)
		}
	}
	return out
}

// child returns the first child list named name.
func (s sexp) child(name string) (sexp, bool) {
	for _, c := range s.children(name) {
		return c, true
	}
	return sexp{}, false
}

// args returns the atoms after the node name (nested lists are skipped).
func (s sexp) args() []string {
	if !s.isL {
		return nil
	}
	var out []string
	for _, c := range s.list[1:] {
		if !c.isL {
			out = append(out, c.atom)
		}
	}
	return out
}

// arg returns the i-th atom after the node name.
func (s sexp) arg(i int) string {
	a := s.args()
	if i < 0 || i >= len(a) {
		return ""
	}
	return a[i]
}

// argF returns the i-th atom after the node name as a float.
func (s sexp) argF(i int) float64 { return parseFloat(s.arg(i)) }

// hasAtom reports whether one of the node's bare atoms equals want.
func (s sexp) hasAtom(want string) bool {
	for _, a := range s.args() {
		if a == want {
			return true
		}
	}
	return false
}

// xy reads a (name x y [rot]) child, e.g. (at 1 -2 90) or (start 0 0).
func (s sexp) xy(name string) (x, y float64, ok bool) {
	c, ok := s.child(name)
	if !ok || len(c.args()) < 2 {
		return 0, 0, false
	}
	return c.argF(0), c.argF(1), true
}

// parseSexp parses one or more top-level s-expressions from src.
func parseSexp(src string) ([]sexp, error) {
	p := &sexpParser{src: src}
	var out []sexp
	for {
		p.skipSpace()
		if p.i >= len(p.src) {
			return out, nil
		}
		n, err := p.node()
		if err != nil {
			return out, err
		}
		out = append(out, n)
	}
}

type sexpParser struct {
	src string
	i   int
}

func (p *sexpParser) skipSpace() {
	for p.i < len(p.src) {
		c := p.src[p.i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			p.i++
			continue
		}
		// KiCad files carry no comments, but tolerate `;` line comments.
		if c == ';' {
			for p.i < len(p.src) && p.src[p.i] != '\n' {
				p.i++
			}
			continue
		}
		return
	}
}

func (p *sexpParser) node() (sexp, error) {
	p.skipSpace()
	if p.i >= len(p.src) {
		return sexp{}, fmt.Errorf("sexp: unexpected end of input")
	}
	switch p.src[p.i] {
	case '(':
		p.i++
		n := sexp{isL: true}
		for {
			p.skipSpace()
			if p.i >= len(p.src) {
				return sexp{}, fmt.Errorf("sexp: unclosed list")
			}
			if p.src[p.i] == ')' {
				p.i++
				return n, nil
			}
			c, err := p.node()
			if err != nil {
				return sexp{}, err
			}
			n.list = append(n.list, c)
		}
	case ')':
		return sexp{}, fmt.Errorf("sexp: unexpected )")
	case '"':
		return sexp{atom: p.quoted()}, nil
	default:
		start := p.i
		for p.i < len(p.src) && !strings.ContainsRune(" \t\r\n()", rune(p.src[p.i])) {
			p.i++
		}
		return sexp{atom: p.src[start:p.i]}, nil
	}
}

// quoted reads a "…" atom, honouring KiCad's backslash escapes.
func (p *sexpParser) quoted() string {
	p.i++ // opening quote
	var b strings.Builder
	for p.i < len(p.src) {
		c := p.src[p.i]
		if c == '\\' && p.i+1 < len(p.src) {
			p.i++
			switch p.src[p.i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(p.src[p.i])
			}
			p.i++
			continue
		}
		if c == '"' {
			p.i++
			return b.String()
		}
		b.WriteByte(c)
		p.i++
	}
	return b.String()
}

// unquote strips the quotes KiCad puts around names in v6+ files.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if v, err := strconv.Unquote(s); err == nil {
			return v
		}
		return s[1 : len(s)-1]
	}
	return s
}
