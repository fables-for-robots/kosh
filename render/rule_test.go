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
	if got.Name != name {
		t.Errorf("Name = %q, want %q", got.Name, name)
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
