package tui

import tea "charm.land/bubbletea/v2"

type prompt struct{}

func (p *prompt) open() bool                      { return false }
func (p *prompt) resize(int)                      {}
func (p *prompt) line(w int) string               { return fit("", w) }
func (p *prompt) suggest([]string)                {}
func (r *root) promptKey(tea.KeyPressMsg) tea.Cmd { return nil }
func (r *root) promptPaste(tea.PasteMsg) tea.Cmd  { return nil }
func (r *root) openCommand() tea.Cmd              { return nil }
func (r *root) openFilter() tea.Cmd               { return nil }

type suggestionsMsg struct{ names []string }

type history struct{}

func (h *history) add(string)           {}
func (r *root) historyBack() tea.Cmd    { return nil }
func (r *root) historyForward() tea.Cmd { return nil }
func (r *root) historyLast() tea.Cmd    { return nil }
