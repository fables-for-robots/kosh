package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// yamlPart says how a part of a line of YAML is drawn.
type yamlPart int

const (
	yamlPlain yamlPart = iota
	yamlKey
	yamlComment
	// yamlMatch is a match of a search, which a text view draws over
	// the other parts.
	yamlMatch
)

// yamlStyles are the styles of the parts that are not plain.
var yamlStyles = map[yamlPart]lipgloss.Style{
	yamlKey:     styleKey,
	yamlComment: styleDim,
	yamlMatch:   styleMatch,
}

// yamlSpan is the part of a line from byte from to byte to.
type yamlSpan struct {
	from, to int
	part     yamlPart
}

// yamlColor takes one cleaned line of YAML and returns it styled: the
// key in styleKey, the value after it in the default colour, a comment
// in styleDim. Stripped of its styles, the result is line. A line it
// does not understand is returned as it is.
func yamlColor(line string) string {
	return yamlPaint(line, yamlSpans(line))
}

// yamlPaint draws line in the styles of its spans.
func yamlPaint(line string, spans []yamlSpan) string {
	var b strings.Builder
	for _, s := range spans {
		if st, ok := yamlStyles[s.part]; ok {
			b.WriteString(st.Render(line[s.from:s.to]))
		} else {
			b.WriteString(line[s.from:s.to])
		}
	}
	return b.String()
}

// yamlSpans cuts a line of YAML into its parts: after the indentation
// and the dashes of list items, the key (what is before the first ": ",
// or a final ":", outside quotes), and a comment (from a "#" at the
// start or after a space, outside quotes). Everything else is plain, a
// line it does not understand all of it. The spans cover the line.
func yamlSpans(line string) []yamlSpan {
	whole := []yamlSpan{{0, len(line), yamlPlain}}
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	for strings.HasPrefix(line[i:], "- ") {
		i += 2
		for i < len(line) && line[i] == ' ' {
			i++
		}
	}
	rest := line[i:]
	keyEnd, comment := -1, -1
	start := 0 // where a scalar may start, which a quote opens only there
	var quote byte
scan:
	for j := 0; j < len(rest); j++ {
		c := rest[j]
		if quote != 0 {
			switch {
			case quote == '"' && c == '\\':
				j++
			case c == quote:
				quote = 0
			}
			continue
		}
		switch {
		case j == start && (c == '"' || c == '\''):
			quote = c
		case c == '#' && (j == 0 || rest[j-1] == ' '):
			comment = j
			break scan
		case keyEnd < 0 && c == ':' && (j+1 == len(rest) || rest[j+1] == ' '):
			keyEnd = j
			start = j + 1
			for start < len(rest) && rest[start] == ' ' {
				start++
			}
		}
	}
	if quote != 0 {
		return whole
	}
	if keyEnd == 0 || keyEnd > 0 && strings.ContainsAny(rest[:1], "{[|>&*!%@`") {
		return whole
	}
	var spans []yamlSpan
	add := func(from, to int, part yamlPart) {
		if to > from {
			spans = append(spans, yamlSpan{from, to, part})
		}
	}
	end := len(line)
	if comment >= 0 {
		end = i + comment
	}
	if keyEnd > 0 {
		add(0, i, yamlPlain)
		add(i, i+keyEnd, yamlKey)
		add(i+keyEnd, end, yamlPlain)
	} else {
		add(0, end, yamlPlain)
	}
	add(end, len(line), yamlComment)
	if len(spans) == 0 {
		return whole
	}
	return spans
}
