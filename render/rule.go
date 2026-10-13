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

// ruleEntry is what the rules allow on one resource or one URL.
type ruleEntry struct {
	name, group string
	verbs       []string
}

// RuleRows returns the rows of the rules of a role: one row for each
// resource a rule names, with the verbs it allows. A row's Name is what
// the rule is for.
//
// A rule gives a row for each of its resources in each of its API
// groups (the core group is shown as "core"), and a row for each of its
// non-resource URLs, with the group NAValue. Rules that name one
// resource in one group are merged into one row. Resource names are not
// shown. Rows are sorted by name, then group. Nil or empty rules give no
// rows.
func RuleRows(rules []rbacv1.PolicyRule) []Row {
	var entries []*ruleEntry
	index := map[[2]string]*ruleEntry{}
	add := func(name, group string, verbs []string) {
		key := [2]string{name, group}
		e, ok := index[key]
		if !ok {
			e = &ruleEntry{name: name, group: group}
			index[key] = e
			entries = append(entries, e)
		}
		e.verbs = append(e.verbs, verbs...)
	}
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			if group == "" {
				group = "core"
			}
			for _, res := range rule.Resources {
				add(res, group, rule.Verbs)
			}
		}
		for _, url := range rule.NonResourceURLs {
			if url == "" {
				continue
			}
			if url[0] != '/' {
				url = "/" + url
			}
			add(url, NAValue, rule.Verbs)
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return entries[i].group < entries[j].group
	})

	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		b := NewRow(&metav1.ObjectMeta{Name: e.name}).Text(e.name).Text(e.group)
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
