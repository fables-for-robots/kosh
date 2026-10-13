package tui

import tea "charm.land/bubbletea/v2"

type namesView struct{ *tableView }

func (r *root) openNames() tea.Cmd { return nil }
