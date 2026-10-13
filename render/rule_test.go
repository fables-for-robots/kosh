package render

import (
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestRuleColumns(t *testing.T) {
	wantColumnList(t, RuleColumns(), "NAME", "API-GROUP", "GET", "LIST", "WATCH", "CREATE", "PATCH", "UPDATE", "DELETE", "DEL-LIST", "EXTRAS*")
	for _, c := range RuleColumns() {
		if c.Numeric {
			t.Errorf("column %s is numeric", c.Name)
		}
	}
}

const (
	ruleYes = "✓"
	ruleNo  = "×"
)

// ruleWant is the verbs a row marks, in the order of the columns.
func ruleMarks(verbs ...string) []string {
	order := []string{"get", "list", "watch", "create", "patch", "update", "delete", "deletecollection"}
	marks := make([]string, len(order))
	for i, o := range order {
		marks[i] = ruleNo
		for _, v := range verbs {
			if v == o {
				marks[i] = ruleYes
			}
		}
	}
	return marks
}

func ruleWantRow(t *testing.T, got Row, name, group string, marks []string, extras string) {
	t.Helper()
	want := append([]string{name, group}, marks...)
	want = append(want, extras)
	if strings.Join(got.Cells, "|") != strings.Join(want, "|") {
		t.Errorf("row:\n got %q\nwant %q", got.Cells, want)
	}
	wantName := name
	if group != "core" && group != NAValue {
		wantName += "." + group
	}
	if got.Name != wantName {
		t.Errorf("Name = %q, want %q", got.Name, wantName)
	}
	if got.Namespace != "" {
		t.Errorf("Namespace = %q, want none", got.Namespace)
	}
	if len(got.Order) != len(got.Cells) {
		t.Errorf("%d numbers for %d cells", len(got.Order), len(got.Cells))
	}
	if got.Health != OK {
		t.Errorf("health = %v", got.Health)
	}
}

func TestRuleRowsWithoutRules(t *testing.T) {
	if rows := RuleRows(nil); len(rows) != 0 {
		t.Errorf("nil rules gave %d rows", len(rows))
	}
	if rows := RuleRows([]rbacv1.PolicyRule{}); len(rows) != 0 {
		t.Errorf("no rules gave %d rows", len(rows))
	}
	if rows := RuleRows([]rbacv1.PolicyRule{{Verbs: []string{"get"}}}); len(rows) != 0 {
		t.Errorf("a rule of nothing gave %d rows", len(rows))
	}
}

func TestRuleRowsOneForEachResource(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{{
		APIGroups: []string{""},
		Resources: []string{"pods/log", "pods"},
		Verbs:     []string{"get", "list"},
	}})
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	// Sorted by name.
	ruleWantRow(t, rows[0], "pods", "core", ruleMarks("get", "list"), "")
	ruleWantRow(t, rows[1], "pods/log", "core", ruleMarks("get", "list"), "")
}

func TestRuleRowsResourceWithAGroup(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{{
		APIGroups: []string{"apps", "extensions"},
		Resources: []string{"deployments"},
		Verbs:     []string{"patch"},
	}})
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	ruleWantRow(t, rows[0], "deployments", "apps", ruleMarks("patch"), "")
	ruleWantRow(t, rows[1], "deployments", "extensions", ruleMarks("patch"), "")
}

func TestRuleRowsStarVerbMarksThemAll(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{{
		APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"*"},
	}})
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	ruleWantRow(t, rows[0], "jobs", "batch", ruleMarks("get", "list", "watch", "create", "patch", "update", "delete", "deletecollection"), "")
}

func TestRuleRowsStarResource(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{{
		APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"get"},
	}})
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	ruleWantRow(t, rows[0], "*", "*", ruleMarks("get"), "")
}

func TestRuleRowsNonResourceURLs(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{{
		NonResourceURLs: []string{"/healthz", "metrics", ""},
		Verbs:           []string{"get"},
	}})
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: %v", len(rows), rows)
	}
	ruleWantRow(t, rows[0], "/healthz", NAValue, ruleMarks("get"), "")
	ruleWantRow(t, rows[1], "/metrics", NAValue, ruleMarks("get"), "")
}

func TestRuleRowsMergeTwoRulesOfOneResource(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps"}, Verbs: []string{"post", "impersonate", "get"}},
		{APIGroups: []string{"other"}, Resources: []string{"secrets"}, Verbs: []string{"delete"}},
	})
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3: %v", len(rows), rows)
	}
	ruleWantRow(t, rows[0], "configmaps", "core", ruleMarks("get", "create"), "impersonate")
	// "post" is a create.
	ruleWantRow(t, rows[1], "secrets", "core", ruleMarks("get", "list", "create"), "impersonate")
	ruleWantRow(t, rows[2], "secrets", "other", ruleMarks("delete"), "")
}

func TestRuleRowsStarMergedWithAnotherVerb(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"*"}},
	})
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	ruleWantRow(t, rows[0], "pods", "core", ruleMarks("get", "list", "watch", "create", "patch", "update", "delete", "deletecollection"), "")
}

func TestRuleRowsExtrasAreCut(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"pods"},
		Verbs: []string{"impersonate", "escalate", "bind", "approve", "sign", "use"},
	}})
	extras := rows[0].Cells[len(rows[0].Cells)-1]
	if got := []rune(extras); len(got) != 30 || got[29] != '…' {
		t.Errorf("extras = %q (%d runes), want 30 runes ending in an ellipsis", extras, len(got))
	}
	if !strings.HasPrefix(extras, "impersonate,escalate,bind,") {
		t.Errorf("extras = %q", extras)
	}
}

func TestRuleRowsLimitedRuleAlone(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"secrets"},
		ResourceNames: []string{"db-password"}, Verbs: []string{"get"},
	}})
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1: %v", len(rows), rows)
	}
	ruleWantRow(t, rows[0], "secrets[db-password]", "core", ruleMarks("get"), "")
}

func TestRuleRowsLimitedBesideUnlimited(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"b", "a"}, Verbs: []string{"delete"}},
	})
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: %v", len(rows), rows)
	}
	ruleWantRow(t, rows[0], "secrets", "core", ruleMarks("get", "list"), "")
	ruleWantRow(t, rows[1], "secrets[a,b]", "core", ruleMarks("delete"), "")
}

func TestRuleRowsSameNamesInAnyOrderMerge(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"a", "b"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"b", "a"}, Verbs: []string{"update"}},
	})
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1: %v", len(rows), rows)
	}
	ruleWantRow(t, rows[0], "configmaps[a,b]", "core", ruleMarks("get", "update"), "")
}

func TestRuleRowsSameResourceInTwoGroupsHasUniqueNames(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{
		{APIGroups: []string{"apps", ""}, Resources: []string{"deployments"}, Verbs: []string{"get"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{"x"}, Verbs: []string{"get"}},
	})
	var names, cells []string
	for _, r := range rows {
		names = append(names, r.Name)
		cells = append(cells, r.Cells[0]+"/"+r.Cells[1])
	}
	if got, want := strings.Join(names, "|"), "deployments.apps|deployments|leases[x].coordination.k8s.io"; got != want {
		t.Errorf("Names = %s, want %s", got, want)
	}
	if got, want := strings.Join(cells, "|"), "deployments/apps|deployments/core|leases[x]/coordination.k8s.io"; got != want {
		t.Errorf("cells = %s, want %s", got, want)
	}
}

func TestRuleRowsNonResourceURLName(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{{NonResourceURLs: []string{"healthz"}, Verbs: []string{"get"}}})
	if len(rows) != 1 || rows[0].Name != "/healthz" {
		t.Errorf("rows = %v, want one named /healthz", rows)
	}
}

func TestRuleRowsEmptyNameIsALimit(t *testing.T) {
	limited := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{""}, Verbs: []string{"delete"}}
	unlimited := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}
	for name, rules := range map[string][]rbacv1.PolicyRule{
		"limited first":   {limited, unlimited},
		"unlimited first": {unlimited, limited},
	} {
		t.Run(name, func(t *testing.T) {
			rows := RuleRows(rules)
			if len(rows) != 2 {
				t.Fatalf("%d rows, want 2: %v", len(rows), rows)
			}
			ruleWantRow(t, rows[0], "secrets", "core", ruleMarks("get"), "")
			ruleWantRow(t, rows[1], `secrets[""]`, "core", ruleMarks("delete"), "")
		})
	}
}

func TestRuleRowsEmptyAndANameIsASetOfItsOwn(t *testing.T) {
	rows := RuleRows([]rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"a", ""}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"a"}, Verbs: []string{"delete"}},
	})
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: %v", len(rows), rows)
	}
	ruleWantRow(t, rows[0], `secrets["",a]`, "core", ruleMarks("get"), "")
	ruleWantRow(t, rows[1], "secrets[a]", "core", ruleMarks("delete"), "")
}
