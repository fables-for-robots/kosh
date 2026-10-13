package render

import (
	rbacv1 "k8s.io/api/rbac/v1"
)

// RuleColumns returns the columns of the table of a role's rules.
func RuleColumns() []Column {
	panic("not implemented")
}

// RuleRows returns the rows of the rules of a role: one row for each
// resource a rule names, with the verbs it allows. A row's Name is what
// the rule is for.
func RuleRows(rules []rbacv1.PolicyRule) []Row {
	panic("not implemented")
}
