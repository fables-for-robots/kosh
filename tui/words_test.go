package tui

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCommandWords(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want []string
	}{
		{`sh`, []string{"sh"}},
		{`sh -c "command -v bash >/dev/null && exec bash || exec sh"`, []string{"sh", "-c", "command -v bash >/dev/null && exec bash || exec sh"}},
		{`/bin/bash -l`, []string{"/bin/bash", "-l"}},
		{"a  b\tc", []string{"a", "b", "c"}},
		{"  \t a b \t ", []string{"a", "b"}},
		{`'a b' c`, []string{"a b", "c"}},
		{`'a \" $b'`, []string{`a \" $b`}},
		{`"a \"b\" \\ \$c"`, []string{`a "b" \ $c`}},
		{`"a\nb"`, []string{`a\nb`}},
		{`"it's"`, []string{`it's`}},
		{`'say "hi"'`, []string{`say "hi"`}},
		{`a\ b`, []string{"a b"}},
		{`a\'b`, []string{"a'b"}},
		{`a\\b`, []string{`a\b`}},
		{`a\`, []string{`a\`}},
		{`a"b c"d`, []string{"ab cd"}},
		{`a'b c'"d e"f`, []string{"ab cd ef"}},
		{`''`, []string{""}},
		{`""`, []string{""}},
		{`a '' b`, []string{"a", "", "b"}},
		{`ls *.txt`, []string{"ls", "*.txt"}},
		{`echo $HOME; ls`, []string{"echo", "$HOME;", "ls"}},
		{`a|b && c # d`, []string{"a|b", "&&", "c", "#", "d"}},
		{"名前 ü", []string{"名前", "ü"}},
	} {
		got, err := commandWords(tt.in)
		if err != nil {
			t.Errorf("commandWords(%q): %v", tt.in, err)
			continue
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("commandWords(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	for _, tt := range []struct {
		in   string
		want string
	}{
		{``, commandTextEmpty},
		{`   `, commandTextEmpty},
		{"\t \t", commandTextEmpty},
		{`"abc`, commandTextUnclosed},
		{`'abc`, commandTextUnclosed},
		{`a "b\"`, commandTextUnclosed},
		{`'a'"`, commandTextUnclosed},
		{"a\nb", commandTextControl},
		{"a\rb", commandTextControl},
		{"sh\n", commandTextControl},
		{"'a\nb'", commandTextControl},
		{"a\x1b[2Jb", commandTextControl},
		{"a\x00b", commandTextControl},
		{"a\x7fb", commandTextControl},
		{"a\u009bb", commandTextControl},
	} {
		got, err := commandWords(tt.in)
		if err == nil {
			t.Errorf("commandWords(%q) = %q, want the error %q", tt.in, got, tt.want)
			continue
		}
		if err.Error() != tt.want {
			t.Errorf("commandWords(%q): %q, want %q", tt.in, err, tt.want)
		}
	}
}

// Random strings of what the cutting looks at: an answer or an error,
// never a panic, and a megabyte of them in linear time.
func TestCommandWordsNeverPanics(t *testing.T) {
	alphabet := []string{`'`, `"`, `\`, " ", "\t", "a", "b", "$", "ü"}
	rng := rand.New(rand.NewPCG(1, 2))
	random := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		return b.String()
	}
	for range 10000 {
		in := random(rng.IntN(40))
		got, err := commandWords(in)
		if err == nil && len(got) == 0 {
			t.Fatalf("commandWords(%q): no words and no error", in)
		}
		if err != nil && got != nil {
			t.Fatalf("commandWords(%q): words %q with the error %v", in, got, err)
		}
	}
	const size = 1 << 20
	for _, in := range []string{
		random(size),
		strings.Repeat(`\`, size),
		strings.Repeat(`"\"`, size/3),
		strings.Repeat(`a'' `, size/4),
		strings.Repeat(" ", size) + "a",
	} {
		start := time.Now()
		if _, err := commandWords(in); err != nil && err.Error() == "" {
			t.Fatal("an error without words")
		}
		if d := time.Since(start); d > slowdown*time.Second {
			t.Errorf("cutting a megabyte took %v", d)
		}
	}
}
