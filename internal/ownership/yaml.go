package ownership

import (
	"fmt"
	"strings"
)

// This file is a small YAML subset parser, so the module stays standard-library only. It covers
// what config files like projects.yaml and gh's hosts.yml use: block mappings, block sequences
// (including "- key: value" items), flow sequences of scalars ("[a, b]"), single- and
// double-quoted strings, "~"/"null"/empty values, and "#" comments. Every scalar is a string.
// Anchors, aliases, tags, flow mappings, multi-line strings, and multiple documents are rejected
// with a line number rather than misread.

// Map is a YAML mapping that remembers key order.
type Map struct {
	Keys []string
	Vals map[string]any
}

// Get returns a key's value, or nil.
func (m *Map) Get(key string) any {
	if m == nil {
		return nil
	}
	return m.Vals[key]
}

type yline struct {
	n      int // 1-based line number
	indent int
	text   string
}

type yparser struct {
	lines []yline
	pos   int
}

// ParseYAML parses a document into *Map, []any, string, or nil.
func ParseYAML(src string) (any, error) {
	var p yparser
	for i, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		body := strings.TrimRight(stripComment(raw), " \t")
		trimmed := strings.TrimLeft(body, " ")
		if trimmed == "" {
			continue
		}
		if trimmed == "---" && len(p.lines) == 0 {
			continue
		}
		if strings.HasPrefix(trimmed, "\t") || strings.HasPrefix(body, "\t") {
			return nil, fmt.Errorf("line %d: tabs can't indent YAML; use spaces", i+1)
		}
		if trimmed == "---" || trimmed == "..." {
			return nil, fmt.Errorf("line %d: only one YAML document is supported", i+1)
		}
		p.lines = append(p.lines, yline{i + 1, len(body) - len(trimmed), trimmed})
	}
	if len(p.lines) == 0 {
		return nil, nil
	}
	v, err := p.node(0)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.lines[p.pos].n)
	}
	return v, nil
}

// stripComment drops a "#" comment that starts the line or follows whitespace, outside quotes.
func stripComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if (c == '\\' && q == '"') || (c == '\'' && q == '\'' && i+1 < len(s) && s[i+1] == '\'') {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '[' || s[i-1] == ',' || s[i-1] == ':' || s[i-1] == '-' {
				q = c
			}
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

func isDash(t string) bool { return t == "-" || strings.HasPrefix(t, "- ") }

func (p *yparser) node(minIndent int) (any, error) {
	if p.pos >= len(p.lines) || p.lines[p.pos].indent < minIndent {
		return nil, nil
	}
	l := p.lines[p.pos]
	if isDash(l.text) {
		return p.seq(l.indent)
	}
	if _, _, ok := splitKey(l.text); ok {
		return p.mapping(l.indent)
	}
	p.pos++
	return scalar(l.text, l.n)
}

func (p *yparser) seq(indent int) ([]any, error) {
	out := []any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent || !isDash(l.text) {
			return nil, fmt.Errorf("line %d: expected a \"- \" list item at column %d", l.n, indent+1)
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		switch {
		case rest == "":
			p.pos++
			v, err := p.node(indent + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		case isDash(rest):
			return nil, fmt.Errorf("line %d: nested inline lists aren't supported; put the inner list on its own lines", l.n)
		default:
			if _, _, ok := splitKey(rest); ok {
				// "- key: value" starts a mapping indented to where "key" begins.
				p.lines[p.pos] = yline{l.n, l.indent + len(l.text) - len(rest), rest}
				v, err := p.mapping(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
				continue
			}
			p.pos++
			v, err := scalar(rest, l.n)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return out, nil
}

func (p *yparser) mapping(indent int) (*Map, error) {
	m := &Map{Vals: map[string]any{}}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.n)
		}
		if isDash(l.text) {
			break // a sequence item belonging to the parent
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", l.n)
		}
		if _, dup := m.Vals[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.n, key)
		}
		p.pos++
		var v any
		var err error
		switch {
		case rest != "":
			v, err = scalar(rest, l.n)
		case p.pos < len(p.lines) && p.lines[p.pos].indent > indent:
			v, err = p.node(indent + 1)
		case p.pos < len(p.lines) && p.lines[p.pos].indent == indent && isDash(p.lines[p.pos].text):
			v, err = p.seq(indent) // "key:\n- item" with the list at the key's indent
		}
		if err != nil {
			return nil, err
		}
		m.Keys = append(m.Keys, key)
		m.Vals[key] = v
	}
	return m, nil
}

// splitKey splits "key: value" or "key:" outside quotes.
func splitKey(t string) (key, rest string, ok bool) {
	if t == "" || t[0] == '[' || t[0] == '{' {
		return "", "", false
	}
	end := -1
	if t[0] == '"' || t[0] == '\'' {
		if i := strings.IndexByte(t[1:], t[0]); i >= 0 {
			end = i + 2
		} else {
			return "", "", false
		}
	}
	from := max(end, 0)
	i := strings.Index(t[from:], ":")
	for i >= 0 {
		j := from + i
		if j+1 == len(t) || t[j+1] == ' ' {
			raw := strings.TrimSpace(t[:j])
			if end > 0 {
				k, err := scalar(raw, 0)
				if err != nil {
					return "", "", false
				}
				raw, _ = k.(string)
			}
			if raw == "" {
				return "", "", false
			}
			return raw, strings.TrimSpace(t[j+1:]), true
		}
		from = j + 1
		i = strings.Index(t[from:], ":")
	}
	return "", "", false
}

func scalar(t string, n int) (any, error) {
	switch {
	case t == "~" || t == "null" || t == "Null" || t == "NULL":
		return nil, nil
	case t[0] == '[':
		if !strings.HasSuffix(t, "]") {
			return nil, fmt.Errorf("line %d: a flow list must end with \"]\" on the same line", n)
		}
		return flowList(t[1:len(t)-1], n)
	case t[0] == '"':
		return doubleQuoted(t, n)
	case t[0] == '\'':
		if len(t) < 2 || t[len(t)-1] != '\'' {
			return nil, fmt.Errorf("line %d: unterminated single-quoted string", n)
		}
		return strings.ReplaceAll(t[1:len(t)-1], "''", "'"), nil
	case strings.ContainsRune("{&*!|>%@`", rune(t[0])):
		return nil, fmt.Errorf("line %d: %q isn't supported in this config; quote the value", n, t[:1])
	}
	return t, nil
}

func doubleQuoted(t string, n int) (string, error) {
	var b strings.Builder
	for i := 1; i < len(t); i++ {
		c := t[i]
		switch c {
		case '"':
			if i != len(t)-1 {
				return "", fmt.Errorf("line %d: text after a closing quote", n)
			}
			return b.String(), nil
		case '\\':
			i++
			if i >= len(t) {
				break
			}
			switch t[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(t[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("line %d: unterminated double-quoted string", n)
}

func flowList(body string, n int) ([]any, error) {
	out := []any{}
	var cur strings.Builder
	var q byte
	flush := func() error {
		item := strings.TrimSpace(cur.String())
		cur.Reset()
		if item == "" {
			return nil
		}
		if item[0] == '[' {
			return fmt.Errorf("line %d: nested flow lists aren't supported", n)
		}
		v, err := scalar(item, n)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' && i+1 < len(body) {
				cur.WriteByte(c)
				i++
				c = body[i]
			} else if c == q {
				q = 0
			}
			cur.WriteByte(c)
		case c == '"' || c == '\'':
			q = c
			cur.WriteByte(c)
		case c == ',':
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			cur.WriteByte(c)
		}
	}
	if q != 0 {
		return nil, fmt.Errorf("line %d: unterminated string in a flow list", n)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}
