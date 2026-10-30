package main

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type binding struct {
	keys  string
	where string
	help  string
	run   func(m *model, n int) tea.Cmd
}

var bindings []binding

func init() {
	bindings = []binding{
		{"<enter> l", "crumb", "go to services", func(m *model, _ int) tea.Cmd { return m.back() }},
		{"<ctrl-j> j <esc>", "crumb", "back down", func(m *model, _ int) tea.Cmd { m.crumb = false; return nil }},
		{"<enter> l", "deployments", "open the build log", (*model).openDeployment},
		{"<space>R", "deployments", "roll back to this deployment", (*model).rollbackCursor},
		{"G", "logs", "go to the end and follow", func(m *model, _ int) tea.Cmd { m.follow = true; m.cursor = len(m.logs) - 1; return nil }},
		{"b", "logs", "toggle build / runtime log", func(m *model, _ int) tea.Cmd { m.logBuild = !m.logBuild; m.logDep = 0; return m.streamLogs() }},
		{"V", "logs", "select lines (y yanks)", func(m *model, _ int) tea.Cmd { m.visual = m.cursor; return nil }},
		{"v", "env", "reveal / mask values", func(m *model, _ int) tea.Cmd { m.reveal = !m.reveal; return nil }},
		{"b", "env", "toggle build-time for this variable", (*model).toggleBuildEnv},
		{"<enter> i a", "fields", "edit the field", func(m *model, _ int) tea.Cmd { return m.editField(false) }},
		{"cc", "fields", "replace the field", func(m *model, _ int) tea.Cmd { return m.editField(true) }},
		{"i a <enter> l", "rows", "edit the row", func(m *model, _ int) tea.Cmd { return m.editRow(false) }},
		{"cc", "rows", "replace the row", func(m *model, _ int) tea.Cmd { return m.editRow(true) }},
		{"o O", "rows", "add a row", (*model).addRow},
		{"dd", "rows", "delete the row", (*model).deleteRow},
		{"<space>g", "settings", "connect GitHub", (*model).connectPrompt},
		{"<enter> l", "wizard", "choose", (*model).wizardChoose},
		{"h <esc>", "wizard", "previous step", (*model).wizardBack},
		{"<enter> l", "services", "open the service", func(m *model, _ int) tea.Cmd { return m.open("service", m.serviceAt(m.cursor), 0) }},
		{"o O", "services", "new service", func(m *model, _ int) tea.Cmd { return m.startWizard() }},
		{"dd", "services", "delete the service", func(m *model, _ int) tea.Cmd { return m.deleteService(m.serviceAt(m.cursor), false) }},
		{"<space>d", "services service", "deploy", func(m *model, _ int) tea.Cmd { return m.action("deploy", m.current()) }},
		{"<space>r", "services service", "restart", func(m *model, _ int) tea.Cmd { return m.action("restart", m.current()) }},
		{"<space>s", "services service", "stop", func(m *model, _ int) tea.Cmd { return m.action("stop", m.current()) }},
		{"<ctrl-l>", "service", "next tab", func(m *model, n int) tea.Cmd { return m.nextTab(max(n, 1)) }},
		{"<ctrl-h>", "service", "previous tab", func(m *model, n int) tea.Cmd { return m.nextTab(-max(n, 1)) }},
		{"<ctrl-k>", "service settings", "up to the breadcrumb", func(m *model, _ int) tea.Cmd { m.crumb = true; return nil }},
		{"u", "service", "undo", func(m *model, n int) tea.Cmd { m.undoRedo(&m.undo, &m.redo, max(n, 1)); return nil }},
		{"<ctrl-r>", "service", "redo", func(m *model, n int) tea.Cmd { m.undoRedo(&m.redo, &m.undo, max(n, 1)); return nil }},
		{"E", "service", "edit env vars in $EDITOR", (*model).editEnvFile},
		{"yy", "", "yank the value under the cursor", (*model).yank},
		{"j <down>", "", "down", func(m *model, n int) tea.Cmd { m.move(max(n, 1)); return nil }},
		{"k <up>", "", "up", func(m *model, n int) tea.Cmd { m.move(-max(n, 1)); return nil }},
		{"gg", "", "top", func(m *model, _ int) tea.Cmd { m.move(-1 << 30); return nil }},
		{"G", "", "bottom", func(m *model, _ int) tea.Cmd { m.move(1 << 30); return nil }},
		{"<ctrl-d>", "", "half page down", func(m *model, n int) tea.Cmd { m.move(max(n, 1) * m.bodyHeight() / 2); return nil }},
		{"<ctrl-u>", "", "half page up", func(m *model, n int) tea.Cmd { m.move(-max(n, 1) * m.bodyHeight() / 2); return nil }},
		{"h", "", "back up a level", func(m *model, _ int) tea.Cmd { return m.back() }},
		{"/", "", "search", func(m *model, _ int) tea.Cmd { return m.startCommand("/") }},
		{"n", "", "next match", func(m *model, n int) tea.Cmd { m.findNext(max(n, 1)); return nil }},
		{"N", "", "previous match", func(m *model, n int) tea.Cmd { m.findNext(-max(n, 1)); return nil }},
		{"<ctrl-o>", "", "jump back", func(m *model, n int) tea.Cmd { return m.jump(-max(n, 1)) }},
		{"<tab>", "", "jump forward", func(m *model, n int) tea.Cmd { return m.jump(max(n, 1)) }},
		{":", "", "command", func(m *model, _ int) tea.Cmd { return m.startCommand(":") }},
		{"?", "", "key help", func(m *model, _ int) tea.Cmd { m.help = !m.help; return nil }},
		{"<esc>", "", "cancel", func(m *model, _ int) tea.Cmd { m.help, m.visual, m.msg = false, -1, ""; return nil }},
	}
}

func token(k tea.KeyMsg) string {
	s := k.String()
	switch {
	case s == " ":
		return "<space>"
	case s == "<":
		return "<lt>"
	case len([]rune(s)) == 1:
		return s
	}
	return "<" + strings.ReplaceAll(s, "+", "-") + ">"
}

func (m *model) active(where string) bool {
	if where == "" {
		return true
	}
	contexts := m.contexts()
	for _, w := range strings.Fields(where) {
		if slices.Contains(contexts, w) {
			return true
		}
	}
	return false
}

func (m *model) lookup(seq string) (match *binding, longer bool) {
	for i := range bindings {
		b := &bindings[i]
		if !m.active(b.where) || m.crumb && b.where != "crumb" && b.where != "" {
			continue
		}
		for _, k := range strings.Fields(b.keys) {
			if k == seq && match == nil {
				match = b
			} else if len(k) > len(seq) && strings.HasPrefix(k, seq) {
				longer = true
			}
		}
	}
	return match, longer
}
