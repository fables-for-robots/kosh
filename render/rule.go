// Follows k9s's internal/render/policy.go and rbac.go (Apache-2.0).

package render

import (
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rbacv1 "k8s.io/api/rbac/v1"
)

// The marks of a verb that a rule allows and one that it does not.
const (
	ruleAllowed = "✓"
	ruleDenied  = "×"
)

// ruleExtrasWidth is the most runes the last column holds.
const ruleExtrasWidth = 30

// ruleVerbs are the verbs that have a column of their own, in the order
// of the columns.
var ruleVerbs = []string{"get", "list", "watch", "create", "patch", "update", "delete", "deletecollection"}

// ruleVerbAliases are the verbs of HTTP that a rule may use for those of
// the API.
var ruleVerbAliases = map[string]string{"post": "create", "put": "update"}

// RuleColumns returns the columns of the table of a role's rules.
func RuleColumns() []Column {
	return []Column{
		{Name: "NAME"},
		{Name: "API-GROUP"},
		{Name: "GET"},
		{Name: "LIST"},
		{Name: "WATCH"},
		{Name: "CREATE"},
		{Name: "PATCH"},
		{Name: "UPDATE"},
		{Name: "DELETE"},
		{Name: "DEL-LIST"},
		{Name: "EXTRAS", Wide: true},
	}
}

// ruleEntry is what the rules allow on one resource or one URL, limited
// to the named objects when names is not empty.
type ruleEntry struct {
	resource, group string
	names           []string
	verbs           []string
}

// label is the NAME cell: the resource, and its object names in brackets
// when the rule is limited to them.
func (e *ruleEntry) label() string {
	if len(e.names) == 0 {
		return e.resource
	}
	return e.resource + "[" + strings.Join(e.names, ",") + "]"
}

// key is the Name of the row: the NAME cell, followed by "." and the
// group unless that is the core group or none.
func (e *ruleEntry) key() string {
	if e.group == "core" || e.group == NAValue {
		return e.label()
	}
	return e.label() + "." + e.group
}

// RuleRows returns the rows of the rules of a role: one row for each
// resource a rule names, with the verbs it allows. A row's Name is what
// the rule is for, and is unique in the table: the NAME cell, followed by
// "." and the API group unless the group is the core group
// ("deployments.apps", "secrets[db-password]", "/healthz").
//
// A rule gives a row for each of its resources in each of its API
// groups (the core group is shown as "core"), and a row for each of its
// non-resource URLs, with the group NAValue. A rule limited to named
// objects (resourceNames) gives rows of its own, never merged with an
// unlimited rule and never adding to one: the NAME cell is the resource
// followed by the sorted names in brackets ("secrets[db-password]").
// Rules that name one resource in one group and the same set of names are
// merged into one row. Rows are sorted by NAME cell, then group. Nil or
// empty rules give no rows.
func RuleRows(rules []rbacv1.PolicyRule) []Row {
	var entries []*ruleEntry
	index := map[[3]string]*ruleEntry{}
	add := func(resource, group string, names, verbs []string) {
		key := [3]string{resource, group, strings.Join(names, "\x00")}
		e, ok := index[key]
		if !ok {
			e = &ruleEntry{resource: resource, group: group, names: names}
			index[key] = e
			entries = append(entries, e)
		}
		e.verbs = append(e.verbs, verbs...)
	}
	for _, rule := range rules {
		names := ruleNames(rule.ResourceNames)
		for _, group := range rule.APIGroups {
			if group == "" {
				group = "core"
			}
			for _, res := range rule.Resources {
				add(res, group, names, rule.Verbs)
			}
		}
		for _, url := range rule.NonResourceURLs {
			if url == "" {
				continue
			}
			if url[0] != '/' {
				url = "/" + url
			}
			add(url, NAValue, nil, rule.Verbs)
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if li, lj := entries[i].label(), entries[j].label(); li != lj {
			return li < lj
		}
		return entries[i].group < entries[j].group
	})

	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		b := NewRow(&metav1.ObjectMeta{Name: e.key()}).Text(e.label()).Text(e.group)
		for _, v := range ruleVerbs {
			if ruleHasVerb(e.verbs, v) {
				b.Text(ruleAllowed)
			} else {
				b.Text(ruleDenied)
			}
		}
		rows = append(rows, b.Text(ruleExtras(e.verbs)).Row())
	}
	return rows
}

// ruleNames returns the object names of a rule sorted and without
// repeats.
func ruleNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := append([]string(nil), names...)
	sort.Strings(out)
	uniq := out[:1]
	for _, n := range out[1:] {
		if n != uniq[len(uniq)-1] {
			uniq = append(uniq, n)
		}
	}
	return uniq
}

// ruleHasVerb reports whether verbs allow verb: it is named, or its HTTP
// name is, or a verb is "*".
func ruleHasVerb(verbs []string, verb string) bool {
	for _, v := range verbs {
		if v == "*" || v == verb || ruleVerbAliases[v] == verb {
			return true
		}
	}
	return false
}

// ruleExtras returns the verbs that have no column, joined by commas and
// cut to the width of the column.
func ruleExtras(verbs []string) string {
	var extras []string
	seen := map[string]bool{}
	for _, v := range verbs {
		if a, ok := ruleVerbAliases[v]; ok {
			v = a
		}
		if v == "*" || seen[v] {
			continue
		}
		seen[v] = true
		if !ruleHasVerb(ruleVerbs, v) {
			extras = append(extras, v)
		}
	}
	s := strings.Join(extras, ",")
	if r := []rune(s); len(r) > ruleExtrasWidth {
		return string(r[:ruleExtrasWidth-1]) + "…"
	}
	return s
}
