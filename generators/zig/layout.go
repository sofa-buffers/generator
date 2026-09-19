package zig

import "strings"

// layoutZig gives the emitted source the block layout `zig fmt` produces, so a
// user's `zig fmt --check` over a tree that contains generated files passes.
//
// The emitters build many statements as one-line strings -- a guard, a store
// and a return nested in a switch arm -- because that is how they compose. zig
// fmt never keeps a block that holds statements, nor a non-empty switch body,
// on one line: it puts each statement (each switch prong) on a line of its own,
// four spaces deeper than the line the block opened on, and the closing brace
// back on that line's indent with whatever followed it (`} else {`, `},`,
// `}) orelse return null;`). This pass applies exactly that rule, line by line,
// and changes nothing else: every token stays, in order, and only whitespace
// between them moves. Everything else zig fmt cares about is emitted in its
// final form at the emitting site.
//
// Scope, on purpose: braces that open a BLOCK (after `)`, `else`, `catch`,
// a capture `|x|`, a label `blk:`, `=>`, a return type) and braces that open a
// SWITCH body. Initializer braces (`.{`, `T{`) and container declarations
// (`struct {`, `enum(u8) {`) are left as they are. Comments, string and char
// literals, and multiline string lines are never looked into.
func layoutZig(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, layoutLine(ln)...)
	}
	return strings.Join(out, "\n")
}

// layoutLine expands the first block or switch brace on the line that opens and
// closes on it with something inside, then lays out each resulting line the
// same way (a statement's own blocks, and the text after the closing brace).
func layoutLine(line string) []string {
	body := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(body)]
	open, close, isSwitch, ok := firstExpandable(body)
	if !ok {
		return []string{line}
	}
	inner := strings.TrimSpace(body[open+1 : close])
	var items []string
	if isSwitch {
		items = splitProngs(inner)
	} else {
		items = splitStatements(inner)
	}
	out := []string{indent + strings.TrimRight(body[:open+1], " ")}
	for _, it := range items {
		out = append(out, layoutLine(indent+"    "+it)...)
	}
	return append(out, layoutLine(indent+body[close:])...)
}

// pair is one matched bracket on a line: the byte offsets of its opener and
// closer, and the opener character.
type pair struct {
	open, close int
	ch          byte
}

// scanPairs matches the brackets on one line of Zig, skipping string and char
// literals and stopping at a comment or a multiline string line. A closer
// without an opener on the line (the `}` that ends a block opened above) is
// ignored. `visit`, when non-nil, sees every structural byte outside literals
// with the bracket depth BEFORE it.
func scanPairs(s string, visit func(i int, c byte, depth int)) []pair {
	var pairs []pair
	var stack []pair
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"', '\'':
			i = skipLiteral(s, i)
			continue
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				return pairs
			}
		case '\\':
			if i+1 < len(s) && s[i+1] == '\\' {
				return pairs
			}
		}
		if visit != nil {
			visit(i, c, len(stack))
		}
		switch c {
		case '(', '[', '{':
			stack = append(stack, pair{open: i, ch: c})
		case ')', ']', '}':
			if n := len(stack); n > 0 && stack[n-1].ch == opener(c) {
				p := stack[n-1]
				stack = stack[:n-1]
				p.close = i
				pairs = append(pairs, p)
			}
		}
	}
	return pairs
}

func opener(c byte) byte {
	switch c {
	case ')':
		return '('
	case ']':
		return '['
	}
	return '{'
}

// skipLiteral returns the offset of the quote that closes the string or char
// literal opened at s[i] (or the last byte, for an unterminated one).
func skipLiteral(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			return j
		}
	}
	return len(s) - 1
}

// firstExpandable finds the leftmost brace pair on the line that zig fmt would
// break: a block with anything inside, or a non-empty switch body.
func firstExpandable(s string) (open, close int, isSwitch, ok bool) {
	pairs := scanPairs(s, nil)
	parenOpen := map[int]int{}
	for _, p := range pairs {
		if p.ch == '(' {
			parenOpen[p.close] = p.open
		}
	}
	best := -1
	for i, p := range pairs {
		if p.ch != '{' || strings.TrimSpace(s[p.open+1:p.close]) == "" {
			continue
		}
		kind := braceKind(s, p.open, parenOpen)
		if kind == braceOther {
			continue
		}
		if best < 0 || p.open < pairs[best].open {
			best, isSwitch = i, kind == braceSwitch
		}
	}
	if best < 0 {
		return 0, 0, false, false
	}
	return pairs[best].open, pairs[best].close, isSwitch, true
}

const (
	braceBlock = iota
	braceSwitch
	braceOther // an initializer or a container declaration
)

// braceKind classifies the `{` at s[i] by what precedes it.
func braceKind(s string, i int, parenOpen map[int]int) int {
	if i > 0 && (s[i-1] == '.' || s[i-1] == ']' || s[i-1] == '}' || isIdentByte(s[i-1])) {
		return braceOther // `.{`, `[_]u8{`, `T{`, `struct { ... }{`: an initializer
	}
	before := strings.TrimRight(s[:i], " ")
	if before == "" {
		return braceBlock
	}
	if before[len(before)-1] == ')' {
		if po, ok := parenOpen[len(before)-1]; ok {
			switch trailingWord(s[:po]) {
			case "switch":
				return braceSwitch
			case "enum", "union", "struct", "opaque":
				return braceOther
			}
		}
		return braceBlock
	}
	switch trailingWord(before) {
	case "struct", "enum", "union", "opaque", "error":
		return braceOther
	}
	return braceBlock
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// trailingWord is the identifier s ends on, ignoring trailing spaces.
func trailingWord(s string) string {
	s = strings.TrimRight(s, " ")
	j := len(s)
	for j > 0 && isIdentByte(s[j-1]) {
		j--
	}
	return s[j:]
}

// leadingWord is the identifier s starts with.
func leadingWord(s string) string {
	j := 0
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	return s[:j]
}

// splitStatements splits a block's contents into its statements: each ends on
// a top-level `;`, or on a top-level `}` that the next statement follows
// directly (`if (c) { ... } x = y;`) -- not on one an `else`, `orelse`,
// `catch` or operator continues.
func splitStatements(s string) []string {
	var out []string
	start := 0
	scanPairs(s, func(i int, c byte, depth int) {
		if i < start {
			return
		}
		end := false
		switch {
		case c == ';' && depth == 0:
			end = true
		case c == '}' && depth == 1:
			rest := strings.TrimLeft(s[i+1:], " ")
			if rest != "" && (isIdentByte(rest[0]) || rest[0] == '@') {
				switch leadingWord(rest) {
				case "else", "orelse", "catch", "and", "or":
				default:
					end = true
				}
			}
		}
		if end {
			out = append(out, strings.TrimSpace(s[start:i+1]))
			start = i + 1
		}
	})
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

// splitProngs splits a switch body into its prongs, each ending on the comma zig
// fmt always gives it. The commas between the items of one prong (`0, 1 => x`)
// are not prong ends: a piece without a top-level `=>` joins the next.
func splitProngs(s string) []string {
	var pieces []string
	start := 0
	scanPairs(s, func(i int, c byte, depth int) {
		if c == ',' && depth == 0 {
			pieces = append(pieces, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	})
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		pieces = append(pieces, tail)
	}
	var out []string
	pending := ""
	for _, p := range pieces {
		if pending != "" {
			p = pending + ", " + p
		}
		if !hasTopLevelArrow(p) {
			pending = p
			continue
		}
		pending = ""
		out = append(out, p+",")
	}
	if pending != "" {
		out = append(out, pending+",")
	}
	return out
}

func hasTopLevelArrow(s string) bool {
	found := false
	scanPairs(s, func(i int, c byte, depth int) {
		if c == '=' && depth == 0 && i+1 < len(s) && s[i+1] == '>' {
			found = true
		}
	})
	return found
}
