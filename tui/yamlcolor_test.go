package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestYAMLColorKeepsTheText(t *testing.T) {
	for _, tt := range []struct {
		what    string
		line    string
		key     string // the part drawn in styleKey, or "" for none
		comment string // the part drawn in styleDim, or "" for none
	}{
		{what: "a key and value", line: "kind: Pod", key: "kind"},
		{what: "a list item with a key", line: "  - name: web", key: "name"},
		{what: "a nested key", line: "    containerPort: 8080", key: "containerPort"},
		{what: "a key without a value", line: "metadata:", key: "metadata"},
		{what: "a comment", line: "  # a comment: with a colon", comment: "# a comment: with a colon"},
		{what: "a value and a comment", line: "replicas: 3 # three", key: "replicas", comment: "# three"},
		{what: "a quoted string with a colon", line: `  message: "error: bad # not a comment"`, key: "message"},
		{what: "a quoted key", line: `  "app.kubernetes.io/name: x": web`, key: `"app.kubernetes.io/name: x"`},
		{what: "a single-quoted value", line: `  note: 'it''s: fine'`, key: "note"},
		{what: "a URL in a value", line: "  url: http://example.com:8080/x", key: "url"},
		{what: "a line of a block scalar", line: "    some text of a script"},
		{what: "a list item without a key", line: "  - --verbose"},
		{what: "an empty line", line: ""},
		{what: "a document start", line: "---"},
		{what: "an unterminated quote", line: `  "a: b`},
		{what: "a flow mapping", line: "  {a: b}"},
		{what: "a control picture", line: "  name: a␀b", key: "name"},
		{what: "wide letters", line: "  名前: 値", key: "名前"},
	} {
		got := yamlColor(tt.line)
		if plain := ansi.Strip(got); plain != tt.line {
			t.Errorf("%s: yamlColor(%q) stripped is %q", tt.what, tt.line, plain)
		}
		if tt.key != "" {
			if !strings.Contains(got, styleKey.Render(tt.key)) {
				t.Errorf("%s: the key %q of %q is not in the key's style: %q", tt.what, tt.key, tt.line, got)
			}
		} else if strings.Contains(got, sgr(styleKey)) {
			t.Errorf("%s: %q has a key: %q", tt.what, tt.line, got)
		}
		if tt.comment != "" {
			if !strings.Contains(got, styleDim.Render(tt.comment)) {
				t.Errorf("%s: the comment %q of %q is not dim: %q", tt.what, tt.comment, tt.line, got)
			}
		} else if strings.Contains(got, sgr(styleDim)) {
			t.Errorf("%s: %q has a comment: %q", tt.what, tt.line, got)
		}
		if tt.key == "" && tt.comment == "" && got != tt.line {
			t.Errorf("%s: %q is not returned as it is: %q", tt.what, tt.line, got)
		}
	}
}
