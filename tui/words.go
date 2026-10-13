package tui

import (
	"errors"
	"strings"
)

// The Command field's text is cut into words as a shell cuts a simple
// command, and nothing else of a shell's language is done to it (spec
// §2): no variables, no globbing, no ;, |, && or comments. Whoever wants
// those writes sh -c '…', as the default does.

// Why a command's text has no words to run, as the dialog says it.
const (
	commandTextEmpty    = "the command is empty"
	commandTextUnclosed = "a quote is not closed"
	commandTextControl  = "the command holds a control character"
)

// commandWords cuts text into the words of a command: the command and
// its arguments, as the container's runtime gets them.
//
// The text is trimmed of spaces and tabs at its ends; words are
// separated by spaces and tabs. '…' makes one word of what it holds, in
// which nothing is special. "…" makes one word of what it holds, in which
// a backslash before ", \ or $ takes that character literally and any
// other backslash is itself. Outside quotes a backslash takes the next
// character literally, and one at the end is itself. Quoted and unquoted
// pieces that touch are one word: a"b c"d is "ab cd". Quotes with
// nothing between them are an empty word.
//
// A text with a control character in it (a line feed, a carriage return,
// an escape, DEL or a C1 control; the tab is a space) is refused, so that
// a paste cannot bring a second line; so is a text with no words, and
// one with a quote that is not closed. It runs in time linear in the
// text.
func commandWords(text string) ([]string, error) {
	if strings.ContainsFunc(text, formControl) {
		return nil, errors.New(commandTextControl)
	}
	text = strings.Trim(text, " \t")
	var (
		out  []string
		word strings.Builder
		in   bool // a word has begun, perhaps an empty one
	)
	end := func() {
		if in {
			out = append(out, word.String())
			word.Reset()
			in = false
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch c {
		case ' ', '\t':
			end()
		case '\'':
			in = true
			j := strings.IndexByte(text[i+1:], '\'')
			if j < 0 {
				return nil, errors.New(commandTextUnclosed)
			}
			word.WriteString(text[i+1 : i+1+j])
			i += j + 1
		case '"':
			in = true
			closed := false
			for i++; i < len(text); i++ {
				c := text[i]
				if c == '"' {
					closed = true
					break
				}
				if c == '\\' && i+1 < len(text) && strings.IndexByte(`"\$`, text[i+1]) >= 0 {
					i++
					c = text[i]
				}
				word.WriteByte(c)
			}
			if !closed {
				return nil, errors.New(commandTextUnclosed)
			}
		case '\\':
			in = true
			if i+1 < len(text) {
				i++
			}
			word.WriteByte(text[i])
		default:
			in = true
			word.WriteByte(c)
		}
	}
	end()
	if len(out) == 0 {
		return nil, errors.New(commandTextEmpty)
	}
	return out, nil
}
