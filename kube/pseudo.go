package kube

// watchPseudo opens the table of Containers or Rules.
func (s *session) watchPseudo(q Query) (Watch, error) {
	return nil, ErrNoRelation
}
