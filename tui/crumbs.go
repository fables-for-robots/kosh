package tui

import "strings"

// crumbs draws the Crumb of every view of the stack, the top one in the
// title style, in one line.
func (r *root) crumbs() string {
	parts := make([]string, len(r.stack))
	for i, v := range r.stack {
		c := "<" + strings.ReplaceAll(clean(v.Crumb()), "\n", " ") + ">"
		if i == len(r.stack)-1 {
			parts[i] = styleTitle.Render(c)
		} else {
			parts[i] = styleCrumb.Render(c)
		}
	}
	return fit(strings.Join(parts, " "), r.w)
}
