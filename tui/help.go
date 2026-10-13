package tui

import tea "charm.land/bubbletea/v2"

type helpView struct{ *tableView }

func (r *root) openHelp() tea.Cmd { return nil }
