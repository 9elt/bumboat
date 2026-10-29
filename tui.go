package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/9elt/bumboat/api"
	osc52 "github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/protobuf/proto"
	"rsc.io/qr"
)

const (
	normal = iota
	insert
	command
)

var reInvalidName = regexp.MustCompile(`[^a-z0-9-]+`)

var tabs = []string{"overview", "deployments", "logs", "env", "domains", "volumes"}

var (
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bold   = lipgloss.NewStyle().Bold(true)
	sel    = lipgloss.NewStyle().Reverse(true)
	vis    = lipgloss.NewStyle().Background(lipgloss.Color("4")).Foreground(lipgloss.Color("15"))
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	blue   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
)

type place struct {
	screen, name string
	tab          int
}

type wizard struct {
	sv      *api.Service
	domain  string
	step    int
	options []string
	loading bool
}

type model struct {
	c    api.BumboatClient
	w, h int
	list *api.ServiceList
	feed chan tea.Msg

	screen, name   string
	tab            int
	cursor, offset int
	jumps          []place
	jumpAt         int

	mode    int
	keys    string
	count   int
	seq     int
	msg     string
	isErr   bool
	help    bool
	confirm string
	onYes   func() tea.Cmd

	cmdline textinput.Model
	history []string
	histAt  int
	input   textinput.Model
	label   string
	commit  func(string) tea.Cmd
	cancel  func() tea.Cmd

	base, draft *api.Service
	undo, redo  []*api.Service
	reveal      bool
	deps        []*api.Deployment

	logs      []string
	logGen    int
	logCancel context.CancelFunc
	logBuild  bool
	logDep    int64
	follow    bool
	visual    int
	search    string

	settings, settingsDraft *api.Settings
	connectURL              string

	wiz *wizard
}

type (
	feedMsg    struct{ inner tea.Msg }
	listMsg    struct{ l *api.ServiceList }
	feedErrMsg struct{ err error }
	serviceMsg struct {
		sv      *api.Service
		written bool
		then    func() tea.Cmd
	}
	depsMsg     struct{ deps []*api.Deployment }
	settingsMsg struct {
		s       *api.Settings
		written bool
	}
	connectMsg  struct{ url string }
	errMsg      struct{ err error }
	infoMsg     string
	timeoutMsg  struct{ seq int }
	deployedMsg struct {
		dep  *api.Deployment
		open bool
	}
	wizMsg struct {
		step    int
		options []string
	}
	logsMsg struct {
		gen   int
		lines []string
		done  bool
		err   error
		ch    chan string
		errc  chan error
	}
)

func newModel(c api.BumboatClient) *model {
	m := &model{c: c, screen: "services", feed: make(chan tea.Msg, 16), visual: -1, w: 120, h: 30}
	m.cmdline = textinput.New()
	m.input = textinput.New()
	m.input.Prompt = ""
	m.jumps = []place{{screen: "services"}}
	return m
}

func runTUI() error {
	m := newModel(dial())
	go m.watch()
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m *model) watch() {
	for {
		stream, err := m.c.WatchServices(context.Background(), &api.Empty{})
		for err == nil {
			var l *api.ServiceList
			if l, err = stream.Recv(); err == nil {
				m.feed <- listMsg{l}
			}
		}
		m.feed <- feedErrMsg{err}
		time.Sleep(2 * time.Second)
	}
}

func (m *model) next() tea.Cmd { return func() tea.Msg { return feedMsg{<-m.feed} } }

func call[T any](f func(ctx context.Context) (T, error), ok func(T) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		v, err := f(ctx)
		if err != nil {
			return errMsg{err}
		}
		return ok(v)
	}
}

func (m *model) Init() tea.Cmd { return m.next() }

func (m *model) flash(format string, args ...any) {
	m.msg, m.isErr = fmt.Sprintf(format, args...), false
}

func (m *model) fail(format string, args ...any) {
	m.msg, m.isErr = fmt.Sprintf(format, args...), true
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case feedMsg:
		_, cmd = m.Update(msg.inner)
		return m, tea.Batch(cmd, m.next())
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.input.Width = m.w - len(m.label) - 16
	case listMsg:
		m.list = msg.l
		if m.isErr && strings.HasPrefix(m.msg, "cannot reach") {
			m.msg = ""
		}
		if m.screen == "service" && m.base != nil {
			for _, sv := range m.list.Services {
				if sv.Name == m.name {
					for _, t := range []*api.Service{m.base, m.draft} {
						t.State, t.Dirty, t.Live, t.Last, t.Cpu, t.Mem = sv.State, sv.Dirty, sv.Live, sv.Last, sv.Cpu, sv.Mem
					}
				}
			}
			if tabs[m.tab] == "deployments" {
				cmd = m.loadDeps()
			}
		}
	case feedErrMsg:
		m.fail("%s", errorText(msg.err))
	case serviceMsg:
		if m.screen == "service" && msg.sv.Name == m.name {
			m.base = msg.sv
			m.draft = proto.Clone(msg.sv).(*api.Service)
		}
		if msg.written {
			m.flash("%q written", msg.sv.Name)
			if msg.then != nil {
				cmd = msg.then()
			}
		}
	case envEditedMsg:
		cmd = m.envEdited(msg)
	case depsMsg:
		m.deps = msg.deps
	case settingsMsg:
		m.settings, m.settingsDraft = msg.s, proto.Clone(msg.s).(*api.Settings)
		if msg.written {
			m.flash("settings written")
		}
	case connectMsg:
		m.connectURL = msg.url
		m.flash("open the link below in a browser signed in to GitHub")
		cmd = call(func(ctx context.Context) (*api.Settings, error) { return m.c.GetSettings(ctx, &api.Empty{}) }, func(s *api.Settings) tea.Msg { return settingsMsg{s: s} })
	case deployedMsg:
		m.flash("deployment #%d queued", msg.dep.Id)
		if msg.open {
			cmd = m.open("service", msg.dep.Service, 0)
		}
		if m.screen == "service" && m.name == msg.dep.Service {
			m.logBuild, m.logDep = true, msg.dep.Id
			m.setTab(slices.Index(tabs, "logs"))
			cmd = tea.Batch(cmd, m.streamLogs())
		}
	case wizMsg:
		if m.wiz != nil && m.wiz.step == msg.step {
			m.wiz.options, m.wiz.loading = msg.options, false
		}
	case logsMsg:
		if msg.gen != m.logGen {
			break
		}
		m.logs = append(m.logs, msg.lines...)
		if over := len(m.logs) - 5000; over > 0 {
			m.logs = m.logs[over:]
			m.cursor = max(0, m.cursor-over)
		}
		if m.follow {
			m.cursor = len(m.logs) - 1
		}
		switch {
		case msg.err != nil:
			m.fail("%s", errorText(msg.err))
		case msg.done:
			m.flash("end of log")
		default:
			cmd = waitLogs(msg.gen, msg.ch, msg.errc)
		}
	case errMsg:
		m.fail("%s", errorText(msg.err))
		if m.wiz != nil {
			m.wiz.loading = false
		}
	case infoMsg:
		m.flash("%s", string(msg))
	case timeoutMsg:
		if msg.seq == m.seq && m.keys != "" {
			b, _ := m.lookup(m.keys)
			n := m.count
			m.keys, m.count = "", 0
			if b != nil {
				cmd = b.run(m, n)
			}
		}
	case tea.KeyMsg:
		var cmds []tea.Cmd
		if msg.Alt && msg.Type == tea.KeyRunes {
			cmds = append(cmds, m.key(tea.KeyMsg{Type: tea.KeyEscape}))
		}
		if msg.Type != tea.KeyRunes {
			cmds = append(cmds, m.key(msg))
		} else {
			for _, r := range msg.Runes {
				cmds = append(cmds, m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
			}
		}
		cmd = tea.Batch(cmds...)
	}
	m.scroll()
	return m, cmd
}

func (m *model) key(k tea.KeyMsg) tea.Cmd {
	if k.String() == "ctrl+c" {
		return tea.Quit
	}
	if m.confirm != "" {
		yes := k.String() == "y"
		m.confirm = ""
		if yes {
			return m.onYes()
		}
		m.flash("cancelled")
		return nil
	}
	switch m.mode {
	case insert:
		switch k.String() {
		case "esc":
			m.mode = normal
			if m.cancel != nil {
				return m.cancel()
			}
			return m.commit(m.input.Value())
		case "enter":
			m.mode = normal
			return m.commit(m.input.Value())
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		return cmd
	case command:
		return m.commandKey(k)
	}

	if m.visual >= 0 && (k.String() == "y" || k.String() == "esc") {
		if k.String() == "y" {
			lo, hi := min(m.visual, m.cursor), max(m.visual, m.cursor)
			m.copy(strings.Join(m.logs[lo:hi+1], "\n"))
			m.flash("%d lines yanked", hi-lo+1)
		}
		m.visual = -1
		return nil
	}

	t := token(k)
	if m.keys == "" && len(t) == 1 && t[0] >= '0' && t[0] <= '9' && (t != "0" || m.count > 0) {
		m.count = m.count*10 + int(t[0]-'0')
		return nil
	}
	m.keys += t
	b, longer := m.lookup(m.keys)
	switch {
	case longer:
		m.seq++
		seq := m.seq
		return tea.Tick(time.Second, func(time.Time) tea.Msg { return timeoutMsg{seq} })
	case b != nil:
		n := m.count
		m.keys, m.count = "", 0
		if !m.isErr {
			m.msg = ""
		}
		return b.run(m, n)
	}
	m.keys, m.count = "", 0
	return nil
}

func (m *model) contexts() []string {
	switch m.screen {
	case "service":
		t := tabs[m.tab]
		switch t {
		case "overview":
			return []string{"service", t, "fields"}
		case "env", "domains", "volumes":
			return []string{"service", t, "rows"}
		}
		return []string{"service", t}
	case "settings":
		return []string{"settings", "fields"}
	}
	return []string{m.screen}
}

func (m *model) serviceAt(i int) string {
	if m.list == nil || i < 0 || i >= len(m.list.Services) {
		return ""
	}
	return m.list.Services[i].Name
}

func (m *model) current() string {
	if m.screen == "service" {
		return m.name
	}
	return m.serviceAt(m.cursor)
}

func config(sv *api.Service) *api.Service {
	c := proto.Clone(sv).(*api.Service)
	c.State, c.Dirty, c.Live, c.Last, c.Cpu, c.Mem = "", false, nil, nil, 0, 0
	for _, d := range c.Domains {
		d.Cert, d.Resolves = "", nil
	}
	return c
}

func (m *model) unsaved() bool {
	switch m.screen {
	case "service":
		return m.draft != nil && !proto.Equal(config(m.draft), config(m.base))
	case "settings":
		return m.settings != nil && !proto.Equal(m.settings, m.settingsDraft)
	}
	return false
}

func (m *model) open(screen, name string, tab int) tea.Cmd {
	if screen == "service" && name == "" {
		return nil
	}
	if m.unsaved() && (screen != m.screen || name != m.name) {
		m.fail("E37: No write since last change (:w to save, :e! to discard)")
		return nil
	}
	m.jumps = append(m.jumps[:m.jumpAt+1], place{screen, name, tab})
	m.jumpAt = len(m.jumps) - 1
	return m.show(place{screen, name, tab})
}

func (m *model) show(p place) tea.Cmd {
	prev := m.name
	m.screen, m.name, m.help, m.visual, m.connectURL = p.screen, p.name, false, -1, ""
	if m.logCancel != nil {
		m.logCancel()
		m.logCancel = nil
	}
	switch p.screen {
	case "services":
		m.cursor = max(0, slices.IndexFunc(m.list.GetServices(), func(sv *api.Service) bool { return sv.Name == prev }))
		return nil
	case "settings":
		m.cursor = 0
		return call(func(ctx context.Context) (*api.Settings, error) { return m.c.GetSettings(ctx, &api.Empty{}) }, func(s *api.Settings) tea.Msg { return settingsMsg{s: s} })
	case "service":
		if p.name != prev || m.base == nil {
			m.base, m.draft, m.undo, m.redo, m.deps, m.logs, m.logDep, m.cursor = nil, nil, nil, nil, nil, nil, 0, 0
		}
		m.setTab(p.tab)
		return tea.Batch(m.loadService(), m.enterTab())
	}
	return nil
}

func (m *model) jump(n int) tea.Cmd {
	to := m.jumpAt + n
	if to < 0 || to >= len(m.jumps) {
		return nil
	}
	if m.unsaved() {
		m.fail("E37: No write since last change (:w to save, :e! to discard)")
		return nil
	}
	m.jumpAt = to
	return m.show(m.jumps[to])
}

func (m *model) back() tea.Cmd {
	switch m.screen {
	case "service", "settings":
		return m.open("services", "", 0)
	case "wizard":
		return m.wizardBack(0)
	}
	return nil
}

func (m *model) loadService() tea.Cmd {
	name := m.name
	return call(func(ctx context.Context) (*api.Service, error) { return m.c.GetService(ctx, &api.Ref{Name: name}) }, func(sv *api.Service) tea.Msg { return serviceMsg{sv: sv} })
}

func (m *model) loadDeps() tea.Cmd {
	name := m.name
	return call(func(ctx context.Context) (*api.DeploymentList, error) {
		return m.c.ListDeployments(ctx, &api.Ref{Name: name})
	}, func(l *api.DeploymentList) tea.Msg { return depsMsg{l.Deployments} })
}

func (m *model) setTab(t int) {
	if t != m.tab {
		m.cursor, m.offset = 0, 0
	}
	m.tab, m.visual = t, -1
	if m.jumpAt < len(m.jumps) {
		m.jumps[m.jumpAt].tab = t
	}
}

func (m *model) nextTab(n int) tea.Cmd {
	if n > 0 {
		m.setTab(min(n, len(tabs)) - 1)
	} else {
		m.setTab((m.tab + 1) % len(tabs))
	}
	return m.enterTab()
}

func (m *model) enterTab() tea.Cmd {
	if m.logCancel != nil && tabs[m.tab] != "logs" {
		m.logCancel()
		m.logCancel = nil
	}
	switch tabs[m.tab] {
	case "deployments":
		return m.loadDeps()
	case "logs":
		if m.logCancel == nil {
			if m.logDep == 0 {
				m.logBuild = m.base == nil || m.base.Live == nil || m.base.State == "deploying"
			}
			return m.streamLogs()
		}
	case "domains":
		if !m.unsaved() {
			return m.loadService()
		}
	}
	return nil
}

func (m *model) streamLogs() tea.Cmd {
	if m.logCancel != nil {
		m.logCancel()
	}
	m.logGen++
	m.logs, m.cursor, m.offset, m.follow, m.visual = nil, 0, 0, true, -1
	ctx, cancel := context.WithCancel(context.Background())
	m.logCancel = cancel
	ch, errc := make(chan string, 4096), make(chan error, 1)
	req := &api.LogsRequest{Service: m.name, Deployment: m.logDep, Build: m.logBuild, Follow: true}
	go func() {
		defer close(ch)
		stream, err := m.c.StreamLogs(ctx, req)
		for err == nil {
			var line *api.LogLine
			if line, err = stream.Recv(); err == nil {
				select {
				case ch <- line.Text:
				case <-ctx.Done():
					return
				}
			}
		}
		if err != io.EOF && ctx.Err() == nil {
			errc <- err
		}
	}()
	return waitLogs(m.logGen, ch, errc)
}

func waitLogs(gen int, ch chan string, errc chan error) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			select {
			case err := <-errc:
				return logsMsg{gen: gen, done: true, err: err}
			default:
				return logsMsg{gen: gen, done: true}
			}
		}
		lines := []string{line}
		for len(lines) < 1000 {
			select {
			case line, ok := <-ch:
				if !ok {
					return logsMsg{gen: gen, lines: lines, ch: ch, errc: errc}
				}
				lines = append(lines, line)
			default:
				return logsMsg{gen: gen, lines: lines, ch: ch, errc: errc}
			}
		}
		return logsMsg{gen: gen, lines: lines, ch: ch, errc: errc}
	}
}

func (m *model) action(kind, name string) tea.Cmd {
	if name == "" {
		return nil
	}
	ref := &api.Ref{Name: name}
	switch kind {
	case "deploy":
		if m.unsaved() {
			m.fail("unsaved changes are not deployed: :wd writes and deploys")
		}
		return call(func(ctx context.Context) (*api.Deployment, error) { return m.c.Deploy(ctx, ref) }, func(d *api.Deployment) tea.Msg { return deployedMsg{dep: d} })
	case "restart":
		return call(func(ctx context.Context) (*api.Empty, error) { return m.c.Restart(ctx, ref) }, func(*api.Empty) tea.Msg { return infoMsg(name + " restarted") })
	case "stop":
		return call(func(ctx context.Context) (*api.Empty, error) { return m.c.Stop(ctx, ref) }, func(*api.Empty) tea.Msg { return infoMsg(name + " stopped") })
	}
	return nil
}

func (m *model) deleteService(name string, force bool) tea.Cmd {
	if name == "" {
		return nil
	}
	del := func() tea.Cmd {
		return call(func(ctx context.Context) (*api.Empty, error) { return m.c.DeleteService(ctx, &api.Ref{Name: name}) }, func(*api.Empty) tea.Msg { return infoMsg(name + " deleted") })
	}
	if force {
		return del()
	}
	m.confirm, m.onYes = "delete "+name+" with its containers and images? volumes are kept (y/n)", del
	return nil
}

func (m *model) rollbackCursor(int) tea.Cmd {
	if m.cursor >= len(m.deps) {
		return nil
	}
	id := m.deps[m.cursor].Id
	return call(func(ctx context.Context) (*api.Deployment, error) { return m.c.Rollback(ctx, &api.Ref{Id: id}) }, func(d *api.Deployment) tea.Msg { return deployedMsg{dep: d} })
}

func (m *model) openDeployment(int) tea.Cmd {
	if m.cursor >= len(m.deps) {
		return nil
	}
	m.logBuild, m.logDep = true, m.deps[m.cursor].Id
	m.setTab(slices.Index(tabs, "logs"))
	return m.streamLogs()
}

func (m *model) write(then func() tea.Cmd) tea.Cmd {
	switch {
	case m.screen == "service" && m.draft != nil:
		draft := proto.Clone(m.draft).(*api.Service)
		return call(func(ctx context.Context) (*api.Service, error) { return m.c.UpdateService(ctx, draft) }, func(sv *api.Service) tea.Msg {
			return serviceMsg{sv: sv, written: true, then: then}
		})
	case m.screen == "settings" && m.settingsDraft != nil:
		draft := proto.Clone(m.settingsDraft).(*api.Settings)
		return call(func(ctx context.Context) (*api.Settings, error) { return m.c.UpdateSettings(ctx, draft) }, func(s *api.Settings) tea.Msg {
			return settingsMsg{s: s, written: true}
		})
	}
	m.flash("nothing to write")
	return nil
}

func (m *model) snapshot() {
	m.undo = append(m.undo, proto.Clone(m.draft).(*api.Service))
	m.redo = nil
}

func (m *model) undoRedo(from, to *[]*api.Service, n int) {
	if m.draft == nil {
		return
	}
	for ; n > 0 && len(*from) > 0; n-- {
		*to = append(*to, m.draft)
		m.draft = (*from)[len(*from)-1]
		*from = (*from)[:len(*from)-1]
	}
	if n > 0 {
		m.flash("already at the oldest change")
	}
}

func (m *model) startInsert(label, value string, commit func(string) tea.Cmd) tea.Cmd {
	m.mode, m.label, m.commit, m.cancel = insert, label, commit, nil
	m.input.SetValue(value)
	m.input.Width = m.w - len(label) - 16
	m.input.CursorEnd()
	return m.input.Focus()
}

type field struct {
	label string
	value string
	set   func(string) error
}

func (m *model) fields() []field {
	if m.screen == "settings" {
		s := m.settingsDraft
		if s == nil {
			return nil
		}
		app := s.GithubApp
		if app == "" {
			app = "not connected · <space>g to connect"
		}
		return []field{
			{"ACME email", s.AcmeEmail, func(v string) error { s.AcmeEmail = v; return nil }},
			{"Build concurrency", strconv.Itoa(int(s.Builds)), func(v string) error {
				n, err := strconv.Atoi(v)
				s.Builds = int32(n)
				return err
			}},
			{"Webhook host", s.WebhookHost, func(v string) error { s.WebhookHost = v; return nil }},
			{"GitHub app", app, nil},
			{"Server", fmt.Sprintf("%d CPUs · %s / %s RAM used", s.Host.GetCpus(), size(s.Host.GetMemUsed()), size(s.Host.GetMemTotal())), nil},
		}
	}
	sv := m.draft
	if sv == nil {
		return nil
	}
	f := []field{{"Source", sv.Source, func(v string) error {
		if v != "github" && v != "image" {
			return errors.New("source is github or image")
		}
		sv.Source = v
		return nil
	}}}
	if sv.Source == "image" {
		f = append(f, field{"Image", sv.Image, func(v string) error { sv.Image = v; return nil }})
	} else {
		f = append(f,
			field{"Repository", sv.Repo, func(v string) error { sv.Repo = v; return nil }},
			field{"Branch", sv.Branch, func(v string) error { sv.Branch = v; return nil }},
			field{"Build method", sv.Build, func(v string) error {
				if v != "dockerfile" && v != "railpack" {
					return errors.New("build method is dockerfile or railpack")
				}
				sv.Build = v
				return nil
			}},
			field{"Dockerfile", sv.Dockerfile, func(v string) error { sv.Dockerfile = v; return nil }},
			field{"Build context", sv.Context, func(v string) error { sv.Context = v; return nil }},
		)
	}
	auto := "off"
	if sv.AutoDeploy {
		auto = "on"
	}
	live := "none"
	if sv.Live != nil {
		live = fmt.Sprintf("#%d %.7s %s", sv.Live.Id, sv.Live.Sha, sv.Live.Message)
	}
	return append(f,
		field{"Port", strconv.Itoa(int(sv.Port)), func(v string) error {
			n, err := strconv.Atoi(v)
			sv.Port = int32(n)
			return err
		}},
		field{"Health path", sv.HealthPath, func(v string) error { sv.HealthPath = v; return nil }},
		field{"Auto deploy", auto, func(v string) error {
			sv.AutoDeploy = v == "on" || v == "yes" || v == "true"
			return nil
		}},
		field{"State", sv.State, nil},
		field{"Live", live, nil},
	)
}

func (m *model) editField(replace bool) tea.Cmd {
	fs := m.fields()
	if m.cursor >= len(fs) {
		return nil
	}
	f := fs[m.cursor]
	if f.set == nil {
		m.fail("%s is read-only", f.label)
		return nil
	}
	value := f.value
	if replace {
		value = ""
	}
	if m.screen == "service" {
		m.snapshot()
	}
	return m.startInsert(f.label, value, func(v string) tea.Cmd {
		if err := f.set(strings.TrimSpace(v)); err != nil {
			m.fail("%s: %v", f.label, err)
		}
		if m.screen == "service" && proto.Equal(m.draft, m.undo[len(m.undo)-1]) {
			m.undo = m.undo[:len(m.undo)-1]
		}
		return nil
	})
}

func (m *model) rows() []string {
	sv := m.draft
	if sv == nil {
		return nil
	}
	var rows []string
	switch tabs[m.tab] {
	case "env":
		for _, e := range sv.Env {
			v := e.Value
			if !m.reveal {
				v = strings.Repeat("•", min(len(v), 12))
			}
			if e.Build {
				v += dim.Render("  build-time")
			}
			rows = append(rows, e.Key+" = "+v)
		}
	case "domains":
		for _, d := range sv.Domains {
			state := d.Cert
			if state == "" {
				state = "unsaved"
			}
			resolves := "does not resolve"
			if len(d.Resolves) > 0 {
				resolves = "→ " + strings.Join(d.Resolves, ", ")
			}
			rows = append(rows, fmt.Sprintf("%-40s %-28s %s", d.Host+d.Path, state, resolves))
		}
	case "volumes":
		for _, v := range sv.Volumes {
			rows = append(rows, fmt.Sprintf("%-20s → %-30s %s", v.Name, v.Path, dim.Render("docker volume bumboat-"+sv.Name+"-"+v.Name)))
		}
	}
	return rows
}

func (m *model) rowValue(i int) string {
	switch tabs[m.tab] {
	case "env":
		return m.draft.Env[i].Key + "=" + m.draft.Env[i].Value
	case "domains":
		return m.draft.Domains[i].Host + m.draft.Domains[i].Path
	case "volumes":
		return m.draft.Volumes[i].Name + ":" + m.draft.Volumes[i].Path
	}
	return ""
}

func (m *model) setRow(i int, v string) error {
	sv := m.draft
	switch tabs[m.tab] {
	case "env":
		key, value, ok := strings.Cut(v, "=")
		if !ok {
			return errors.New("use KEY=value")
		}
		sv.Env[i].Key, sv.Env[i].Value = strings.TrimSpace(key), value
	case "domains":
		host, path, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(v, "https://"), "http://"), "/")
		sv.Domains[i] = &api.Domain{Host: strings.ToLower(host)}
		if path != "" {
			sv.Domains[i].Path = "/" + strings.TrimSuffix(path, "/")
		}
	case "volumes":
		name, path, ok := strings.Cut(v, ":")
		if !ok {
			return errors.New("use name:/path/in/container")
		}
		sv.Volumes[i].Name, sv.Volumes[i].Path = name, path
	}
	return nil
}

func (m *model) rowCount() int {
	switch tabs[m.tab] {
	case "env":
		return len(m.draft.GetEnv())
	case "domains":
		return len(m.draft.GetDomains())
	case "volumes":
		return len(m.draft.GetVolumes())
	}
	return 0
}

func (m *model) removeRow(i int) {
	sv := m.draft
	switch tabs[m.tab] {
	case "env":
		sv.Env = slices.Delete(sv.Env, i, i+1)
	case "domains":
		sv.Domains = slices.Delete(sv.Domains, i, i+1)
	case "volumes":
		sv.Volumes = slices.Delete(sv.Volumes, i, i+1)
	}
	m.cursor = min(m.cursor, max(0, m.rowCount()-1))
}

func (m *model) editRow(replace bool) tea.Cmd {
	if m.draft == nil || m.cursor >= m.rowCount() {
		return nil
	}
	return m.rowInsert(m.cursor, replace, false)
}

func (m *model) rowInsert(i int, replace, fresh bool) tea.Cmd {
	if !fresh {
		m.snapshot()
	}
	value := m.rowValue(i)
	if replace || fresh {
		value = ""
	}
	label := map[string]string{"env": "KEY=value", "domains": "host[/path]", "volumes": "name:/path"}[tabs[m.tab]]
	return m.startInsert(label, value, func(v string) tea.Cmd {
		if strings.TrimSpace(v) == "" && fresh {
			m.removeRow(i)
			m.undo = m.undo[:len(m.undo)-1]
			return nil
		}
		if err := m.setRow(i, v); err != nil {
			m.fail("%v", err)
			m.draft = m.undo[len(m.undo)-1]
			m.undo = m.undo[:len(m.undo)-1]
		}
		return nil
	})
}

func (m *model) addRow(int) tea.Cmd {
	if m.draft == nil {
		return nil
	}
	m.snapshot()
	i := min(m.cursor+1, m.rowCount())
	switch tabs[m.tab] {
	case "env":
		m.draft.Env = slices.Insert(m.draft.Env, i, &api.EnvVar{})
	case "domains":
		m.draft.Domains = slices.Insert(m.draft.Domains, i, &api.Domain{})
	case "volumes":
		m.draft.Volumes = slices.Insert(m.draft.Volumes, i, &api.Volume{})
	}
	m.cursor = i
	return m.rowInsert(i, false, true)
}

func (m *model) deleteRow(n int) tea.Cmd {
	if m.draft == nil || m.cursor >= m.rowCount() {
		return nil
	}
	m.snapshot()
	for range max(n, 1) {
		if m.cursor < m.rowCount() {
			m.removeRow(m.cursor)
		}
	}
	return nil
}

func (m *model) toggleBuildEnv(int) tea.Cmd {
	if m.draft == nil || m.cursor >= len(m.draft.Env) {
		return nil
	}
	m.snapshot()
	m.draft.Env[m.cursor].Build = !m.draft.Env[m.cursor].Build
	return nil
}

type envEditedMsg struct {
	path string
	err  error
}

func (m *model) editEnvFile(int) tea.Cmd {
	if m.draft == nil {
		return nil
	}
	f, err := os.CreateTemp("", "bumboat-"+m.name+"-*.env")
	if err != nil {
		m.fail("%v", err)
		return nil
	}
	for _, e := range m.draft.Env {
		fmt.Fprintf(f, "%s=%s\n", e.Key, e.Value)
	}
	f.Close()
	editor := env("VISUAL", env("EDITOR", "vi"))
	path := f.Name()
	return tea.ExecProcess(exec.Command(editor, path), func(err error) tea.Msg { return envEditedMsg{path, err} })
}

func (m *model) envEdited(msg envEditedMsg) tea.Cmd {
	defer os.Remove(msg.path)
	b, err := os.ReadFile(msg.path)
	if msg.err != nil || err != nil {
		m.fail("editor: %v", errors.Join(msg.err, err))
		return nil
	}
	build := map[string]bool{}
	for _, e := range m.draft.Env {
		build[e.Key] = e.Build
	}
	var vars []*api.EnvVar
	for _, line := range strings.Split(string(b), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export "))
			vars = append(vars, &api.EnvVar{Key: key, Value: value, Build: build[key]})
		}
	}
	m.snapshot()
	m.draft.Env = vars
	return m.write(nil)
}

func (m *model) yank(int) tea.Cmd {
	var v string
	switch {
	case m.screen == "services":
		if i := m.cursor; m.list != nil && i < len(m.list.Services) {
			v = m.list.Services[i].Name
			if ds := m.list.Services[i].Domains; len(ds) > 0 {
				v = "https://" + ds[0].Host + ds[0].Path
			}
		}
	case m.active("fields"):
		if fs := m.fields(); m.cursor < len(fs) {
			v = fs[m.cursor].value
		}
	case m.active("rows"):
		if m.cursor < m.rowCount() {
			v = m.rowValue(m.cursor)
			if tabs[m.tab] == "env" {
				_, v, _ = strings.Cut(v, "=")
			}
		}
	case m.active("deployments"):
		if m.cursor < len(m.deps) {
			v = m.deps[m.cursor].Sha
		}
	case m.active("logs"):
		if m.cursor < len(m.logs) {
			v = m.logs[m.cursor]
		}
	}
	if v != "" {
		m.copy(v)
		m.flash("yanked %q", fmt.Sprintf("%.40s", v))
	}
	return nil
}

func (m *model) copy(s string) {
	osc52.New(s).WriteTo(os.Stderr)
}

func (m *model) lines() []string {
	switch {
	case m.screen == "services":
		var l []string
		for _, sv := range m.list.GetServices() {
			l = append(l, sv.Name)
		}
		return l
	case m.screen == "wizard":
		return m.wiz.options
	case m.active("fields"):
		var l []string
		for _, f := range m.fields() {
			l = append(l, f.label+" "+f.value)
		}
		return l
	case m.active("rows"):
		return m.rows()
	case m.active("deployments"):
		var l []string
		for _, d := range m.deps {
			l = append(l, fmt.Sprintf("#%d %s %s %s", d.Id, d.Status, d.Sha, d.Message))
		}
		return l
	case m.active("logs"):
		return m.logs
	}
	return nil
}

func (m *model) move(delta int) {
	n := len(m.lines())
	if m.active("logs") && delta < 0 {
		m.follow = false
	}
	m.cursor = max(0, min(n-1, m.cursor+delta))
}

func (m *model) findNext(dir int) {
	if m.search == "" {
		return
	}
	lines := m.lines()
	if len(lines) == 0 {
		return
	}
	step := 1
	if dir < 0 {
		step, dir = -1, -dir
	}
	for i, at := 1, m.cursor; i <= len(lines); i++ {
		j := ((at+i*step)%len(lines) + len(lines)) % len(lines)
		if strings.Contains(strings.ToLower(lines[j]), strings.ToLower(m.search)) {
			if dir--; dir == 0 {
				m.cursor = j
				if m.active("logs") {
					m.follow = false
				}
				return
			}
		}
	}
	m.fail("E486: Pattern not found: %s", m.search)
}

func (m *model) bodyHeight() int { return max(1, m.h-4) }

func (m *model) scroll() {
	m.cursor = max(0, m.cursor)
	h := m.bodyHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
}

var commands = []string{"deploy", "rollback", "restart", "stop", "new", "rm", "e", "logs", "set", "unset", "domain", "config", "settings", "w", "wd", "wq", "q", "qa", "help"}

func (m *model) startCommand(prompt string) tea.Cmd {
	m.mode = command
	m.cmdline.Prompt = prompt
	m.cmdline.SetValue("")
	m.histAt = len(m.history)
	return m.cmdline.Focus()
}

func (m *model) commandKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.mode = normal
		return nil
	case "enter":
		m.mode = normal
		line := strings.TrimSpace(m.cmdline.Value())
		if m.cmdline.Prompt == "/" {
			m.search = line
			m.findNext(1)
			return nil
		}
		if line != "" {
			m.history = append(m.history, line)
		}
		return m.exec(line)
	case "up", "down":
		if k.String() == "up" {
			m.histAt = max(0, m.histAt-1)
		} else {
			m.histAt = min(len(m.history), m.histAt+1)
		}
		if m.histAt < len(m.history) {
			m.cmdline.SetValue(m.history[m.histAt])
		} else {
			m.cmdline.SetValue("")
		}
		m.cmdline.CursorEnd()
		return nil
	case "tab":
		if m.cmdline.Prompt == ":" {
			m.complete()
		}
		return nil
	}
	var cmd tea.Cmd
	m.cmdline, cmd = m.cmdline.Update(k)
	return cmd
}

func (m *model) complete() {
	words := strings.Split(m.cmdline.Value(), " ")
	last := words[len(words)-1]
	options := commands
	if len(words) > 1 {
		options = nil
		switch strings.TrimSuffix(words[0], "!") {
		case "deploy", "restart", "stop", "rm", "e", "logs":
			for _, sv := range m.list.GetServices() {
				options = append(options, sv.Name)
			}
		case "config":
			options = []string{"builds=", "email=", "host="}
		case "domain":
			options = []string{"add", "rm"}
		case "unset":
			for _, e := range m.draft.GetEnv() {
				options = append(options, e.Key)
			}
		}
	}
	var matches []string
	for _, o := range options {
		if strings.HasPrefix(o, last) {
			matches = append(matches, o)
		}
	}
	if len(matches) == 0 {
		return
	}
	common := matches[0]
	for _, o := range matches[1:] {
		for !strings.HasPrefix(o, common) {
			common = common[:len(common)-1]
		}
	}
	words[len(words)-1] = common
	if len(matches) == 1 && !strings.HasSuffix(common, "=") {
		words[len(words)-1] += " "
	}
	m.cmdline.SetValue(strings.Join(words, " "))
	m.cmdline.CursorEnd()
	if len(matches) > 1 {
		m.flash("%s", strings.Join(matches, "  "))
	}
}

func (m *model) exec(line string) tea.Cmd {
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	force := strings.HasSuffix(name, "!")
	name = strings.TrimSuffix(name, "!")
	target := arg
	if target == "" {
		target = m.current()
	}
	draftOnly := func() bool {
		if m.screen != "service" || m.draft == nil {
			m.fail("E%s: open a service first", name)
			return false
		}
		m.snapshot()
		return true
	}
	switch name {
	case "":
		return nil
	case "w":
		return m.write(nil)
	case "wd":
		svc := m.name
		return m.write(func() tea.Cmd { return m.action("deploy", svc) })
	case "wq", "x":
		return m.write(func() tea.Cmd { return m.open("services", "", 0) })
	case "q":
		if m.unsaved() && !force {
			m.fail("E37: No write since last change (add ! to override)")
			return nil
		}
		if force {
			m.base, m.draft = nil, nil
		}
		if m.screen == "services" {
			return tea.Quit
		}
		m.settings = nil
		return m.open("services", "", 0)
	case "qa":
		if m.unsaved() && !force {
			m.fail("E37: No write since last change (add ! to override)")
			return nil
		}
		return tea.Quit
	case "deploy", "restart", "stop":
		return m.action(name, target)
	case "rollback":
		id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
		if err != nil {
			m.fail("usage: :rollback <deployment id>")
			return nil
		}
		return call(func(ctx context.Context) (*api.Deployment, error) { return m.c.Rollback(ctx, &api.Ref{Id: id}) }, func(d *api.Deployment) tea.Msg { return deployedMsg{dep: d} })
	case "new":
		return m.startWizard()
	case "rm":
		return m.deleteService(target, force)
	case "e", "logs":
		if force {
			m.base, m.draft, m.settings = nil, nil, nil
		}
		tab := 0
		if name == "logs" {
			tab = slices.Index(tabs, "logs")
		}
		return m.open("service", target, tab)
	case "settings":
		return m.open("settings", "", 0)
	case "help":
		m.help = true
		return nil
	case "set":
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			m.fail("usage: :set KEY=value")
			return nil
		}
		if draftOnly() {
			i := slices.IndexFunc(m.draft.Env, func(e *api.EnvVar) bool { return e.Key == key })
			if i < 0 {
				m.draft.Env = append(m.draft.Env, &api.EnvVar{Key: key, Value: value})
			} else {
				m.draft.Env[i].Value = value
			}
		}
		return nil
	case "unset":
		if draftOnly() {
			m.draft.Env = slices.DeleteFunc(m.draft.Env, func(e *api.EnvVar) bool { return e.Key == arg })
		}
		return nil
	case "domain":
		op, host, _ := strings.Cut(arg, " ")
		if (op != "add" && op != "rm") || host == "" {
			m.fail("usage: :domain add|rm <host[/path]>")
			return nil
		}
		if draftOnly() {
			h, p, _ := strings.Cut(host, "/")
			if p != "" {
				p = "/" + p
			}
			m.draft.Domains = slices.DeleteFunc(m.draft.Domains, func(d *api.Domain) bool { return d.Host == h && d.Path == p })
			if op == "add" {
				m.draft.Domains = append(m.draft.Domains, &api.Domain{Host: strings.ToLower(h), Path: p})
			}
		}
		return nil
	case "config":
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			m.fail("usage: :config builds=N | email=you@example.com | host=bumboat.example.com")
			return nil
		}
		return func() tea.Msg {
			ctx := context.Background()
			s, err := m.c.GetSettings(ctx, &api.Empty{})
			if err != nil {
				return errMsg{err}
			}
			switch key {
			case "builds":
				n, err := strconv.Atoi(value)
				if err != nil {
					return errMsg{fmt.Errorf("builds must be a number (0 = automatic)")}
				}
				s.Builds = int32(n)
			case "email":
				s.AcmeEmail = value
			case "host":
				s.WebhookHost = value
			default:
				return errMsg{fmt.Errorf("unknown setting %q: builds, email or host", key)}
			}
			if s, err = m.c.UpdateSettings(ctx, s); err != nil {
				return errMsg{err}
			}
			m.feed <- settingsMsg{s: s}
			return infoMsg(key + " set")
		}
	}
	m.fail("E492: Not an editor command: %s", line)
	return nil
}

func (m *model) connectPrompt(int) tea.Cmd {
	host := ""
	if m.settings != nil {
		host = m.settings.WebhookHost
	}
	return m.startInsert("webhook host [org]", host, func(v string) tea.Cmd {
		parts := strings.Fields(v)
		if len(parts) == 0 {
			return nil
		}
		req := &api.ConnectGitHubRequest{WebhookHost: parts[0]}
		if len(parts) > 1 {
			req.Org = parts[1]
		}
		return call(func(ctx context.Context) (*api.ConnectGitHubReply, error) { return m.c.ConnectGitHub(ctx, req) }, func(r *api.ConnectGitHubReply) tea.Msg { return connectMsg{r.Url} })
	})
}

func (m *model) wizardSteps() []string {
	if m.wiz.sv.Source == "image" {
		return []string{"source", "image", "port", "name", "domain"}
	}
	return []string{"source", "repository", "branch", "build method", "port", "name", "domain"}
}

func (m *model) startWizard() tea.Cmd {
	if m.unsaved() {
		m.fail("E37: No write since last change (:w to save, :e! to discard)")
		return nil
	}
	m.wiz = &wizard{sv: &api.Service{Source: "github", AutoDeploy: true}}
	m.screen, m.cursor, m.offset = "wizard", 0, 0
	return m.wizardEnter()
}

func (m *model) wizardEnter() tea.Cmd {
	w, sv := m.wiz, m.wiz.sv
	m.cursor, m.offset, w.options, w.loading = 0, 0, nil, false
	step := m.wizardSteps()[w.step]
	advance := func(apply func(string) error) func(string) tea.Cmd {
		return func(v string) tea.Cmd {
			if err := apply(strings.TrimSpace(v)); err != nil {
				m.fail("%v", err)
				return m.wizardEnter()
			}
			return m.wizardNext()
		}
	}
	var cmd tea.Cmd
	switch step {
	case "source":
		w.options = []string{"github", "image"}
	case "repository":
		w.loading = true
		cmd = call(func(ctx context.Context) (*api.RepoList, error) { return m.c.ListRepos(ctx, &api.Empty{}) }, func(l *api.RepoList) tea.Msg {
			var names []string
			for _, r := range l.Repos {
				names = append(names, r.FullName)
			}
			return wizMsg{w.step, names}
		})
	case "branch":
		w.loading = true
		cmd = call(func(ctx context.Context) (*api.BranchList, error) {
			return m.c.ListBranches(ctx, &api.Ref{Name: sv.Repo})
		}, func(l *api.BranchList) tea.Msg {
			return wizMsg{w.step, l.Branches}
		})
	case "build method":
		w.loading = true
		cmd = call(func(ctx context.Context) (*api.SuggestBuildReply, error) {
			return m.c.SuggestBuild(ctx, &api.SuggestBuildRequest{Repo: sv.Repo, Branch: sv.Branch})
		}, func(r *api.SuggestBuildReply) tea.Msg {
			if r.Build == "railpack" {
				return wizMsg{w.step, []string{"railpack", "dockerfile"}}
			}
			return wizMsg{w.step, []string{"dockerfile", "railpack"}}
		})
	case "image":
		cmd = m.startInsert("image (e.g. postgres:17)", sv.Image, advance(func(v string) error {
			if v == "" {
				return errors.New("an image is required")
			}
			sv.Image = v
			return nil
		}))
	case "port":
		port := ""
		if sv.Port > 0 {
			port = strconv.Itoa(int(sv.Port))
		}
		cmd = m.startInsert("port the app listens on (empty: none)", port, advance(func(v string) error {
			n, err := strconv.Atoi(v)
			if v != "" && (err != nil || n < 1 || n > 65535) {
				return errors.New("port must be a number between 1 and 65535")
			}
			sv.Port = int32(n)
			return nil
		}))
	case "name":
		name := sv.Name
		if name == "" {
			base := sv.Repo
			if sv.Source == "image" {
				base, _, _ = strings.Cut(sv.Image, ":")
			}
			name = strings.Trim(reInvalidName.ReplaceAllString(strings.ToLower(base[strings.LastIndex(base, "/")+1:]), "-"), "-")
		}
		cmd = m.startInsert("service name", name, advance(func(v string) error {
			if !reName.MatchString(v) {
				return errors.New("lowercase letters, digits and dashes")
			}
			sv.Name = v
			return nil
		}))
	case "domain":
		cmd = m.startInsert("domain (empty: none)", w.domain, advance(func(v string) error {
			w.domain = strings.ToLower(v)
			sv.Domains = nil
			if v != "" {
				host, path, _ := strings.Cut(w.domain, "/")
				sv.Domains = []*api.Domain{{Host: host}}
				if path != "" {
					sv.Domains[0].Path = "/" + path
				}
				if sv.Port == 0 {
					return errors.New("a domain needs a port: go back with h")
				}
			}
			return nil
		}))
	}
	if m.mode == insert {
		m.cancel = func() tea.Cmd { return m.wizardBack(0) }
	}
	return cmd
}

func (m *model) wizardNext() tea.Cmd {
	if m.wiz.step++; m.wiz.step < len(m.wizardSteps()) {
		return m.wizardEnter()
	}
	sv := m.wiz.sv
	m.wiz = nil
	m.screen = "services"
	return func() tea.Msg {
		ctx := context.Background()
		if _, err := m.c.CreateService(ctx, sv); err != nil {
			return errMsg{err}
		}
		dep, err := m.c.Deploy(ctx, &api.Ref{Name: sv.Name})
		if err != nil {
			return errMsg{err}
		}
		return deployedMsg{dep, true}
	}
}

func (m *model) wizardBack(int) tea.Cmd {
	m.mode = normal
	if m.wiz == nil || m.wiz.step == 0 {
		m.wiz = nil
		m.screen = "services"
		m.flash("cancelled")
		return nil
	}
	m.wiz.step--
	return m.wizardEnter()
}

func (m *model) wizardChoose(int) tea.Cmd {
	w := m.wiz
	if w.loading || m.cursor >= len(w.options) {
		return nil
	}
	v := w.options[m.cursor]
	switch m.wizardSteps()[w.step] {
	case "source":
		w.sv.Source = v
	case "repository":
		if w.sv.Repo != v {
			w.sv.Branch, w.sv.Name = "", ""
		}
		w.sv.Repo = v
	case "branch":
		w.sv.Branch = v
	case "build method":
		w.sv.Build = v
	}
	return m.wizardNext()
}

func ago(unix int64) string {
	if unix == 0 {
		return ""
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	}
	return fmt.Sprintf("%dK", n>>10)
}

func stateStyle(state string) lipgloss.Style {
	switch state {
	case "running", "live":
		return green
	case "deploying", "queued", "building", "starting":
		return yellow
	case "failed":
		return red
	}
	return dim
}

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	var b strings.Builder
	crumb := "services"
	switch m.screen {
	case "service":
		crumb += " › " + m.name
	case "settings":
		crumb += " › settings"
	case "wizard":
		crumb += " › new service"
	}
	if m.unsaved() {
		crumb += " [+]"
	}
	host := ""
	if h := m.list.GetHost(); h != nil {
		host = fmt.Sprintf("%d CPUs · %s/%s", h.Cpus, size(h.MemUsed), size(h.MemTotal))
	}
	b.WriteString(bold.Render("bumboat") + "  " + crumb + strings.Repeat(" ", max(1, m.w-len("bumboat  ")-lipgloss.Width(crumb)-len(host))) + dim.Render(host) + "\n")

	second := ""
	switch {
	case m.screen == "service":
		for i, t := range tabs {
			if i == m.tab {
				second += sel.Render(" "+t+" ") + " "
			} else {
				second += dim.Render(" "+t+" ") + " "
			}
		}
		if m.draft != nil && m.draft.Dirty {
			second += yellow.Render(" saved changes need a deploy: <space>d")
		}
	case m.screen == "wizard":
		steps := m.wizardSteps()
		for i, s := range steps {
			st := dim
			if i == m.wiz.step {
				st = sel
			}
			second += st.Render(" "+s+" ") + " "
		}
	}
	b.WriteString(second + "\n")

	body := m.body()
	if m.help {
		body = m.helpLines()
	}
	h := m.bodyHeight()
	for i := 0; i < h; i++ {
		line := ""
		if j := m.offset + i; m.help && i < len(body) {
			line = body[i]
		} else if !m.help && j < len(body) {
			line = body[j]
		}
		b.WriteString(lipgloss.NewStyle().MaxWidth(m.w).Render(line) + "\n")
	}

	switch {
	case m.mode == command:
		b.WriteString(m.cmdline.View())
	case m.mode == insert:
		b.WriteString(bold.Render("-- INSERT --") + " " + m.label + ": " + m.input.View())
	case m.confirm != "":
		b.WriteString(yellow.Render(m.confirm))
	default:
		mode := "NORMAL"
		if m.visual >= 0 {
			mode = "VISUAL LINE"
		}
		status := dim.Render(mode)
		if m.msg != "" {
			st := lipgloss.NewStyle()
			if m.isErr {
				st = red
			}
			status += "  " + st.Render(m.msg)
		}
		pending := m.keys
		if m.count > 0 {
			pending = strconv.Itoa(m.count) + pending
		}
		b.WriteString(lipgloss.NewStyle().MaxWidth(m.w - 12).Render(status))
		b.WriteString(strings.Repeat(" ", max(1, m.w-lipgloss.Width(status)-12)) + fmt.Sprintf("%-10s", pending))
	}
	return b.String()
}

func (m *model) row(i int, s string) string {
	if i == m.cursor && !m.help {
		return sel.Render(s)
	}
	return s
}

func (m *model) body() []string {
	var out []string
	switch {
	case m.screen == "services":
		if m.list == nil {
			return []string{dim.Render("connecting to the daemon…")}
		}
		if len(m.list.Services) == 0 {
			return []string{"", "  no services yet: press o to create one, or :settings to connect GitHub"}
		}
		for i, sv := range m.list.Services {
			last, commit := "", ""
			if sv.Last != nil {
				last = stateStyle(sv.Last.Status).Render(fmt.Sprintf("#%d %s", sv.Last.Id, sv.Last.Status)) + " " + dim.Render(ago(sv.Last.Started))
			}
			if sv.Live != nil {
				commit = fmt.Sprintf("%.7s %.30s", sv.Live.Sha, sv.Live.Message)
			}
			var domains []string
			for _, d := range sv.Domains {
				domains = append(domains, d.Host+d.Path)
			}
			usage := ""
			if sv.State == "running" {
				usage = fmt.Sprintf("%4.0f%% %5s", sv.Cpu, size(sv.Mem))
			}
			name := fmt.Sprintf("%-20s", sv.Name)
			if i == m.cursor {
				name = sel.Render(name)
			}
			out = append(out, fmt.Sprintf("%s %s %-10s %s  %-40s %-12s %s",
				stateStyle(sv.State).Render("●"), name, sv.State, lipgloss.NewStyle().Width(28).Render(last), commit, usage, dim.Render(strings.Join(domains, " "))))
		}
	case m.screen == "wizard":
		if m.wiz.loading {
			return []string{dim.Render("loading…")}
		}
		titles := map[string]string{
			"source":       "Where does the code come from?",
			"repository":   "Pick a repository (only repos the GitHub App can see)",
			"branch":       "Which branch deploys?",
			"build method": "How should it build? (suggested first)",
		}
		step := m.wizardSteps()[m.wiz.step]
		if title, ok := titles[step]; ok {
			out = append(out, bold.Render(title), "")
			for i, o := range m.wiz.options {
				out = append(out, m.row(i, "  "+o))
			}
			m.offset = 0
		} else {
			out = append(out, bold.Render("Type the "+step+" below, enter to continue, esc to go back"))
		}
	case m.screen == "settings" || m.active("fields"):
		for i, f := range m.fields() {
			value := f.value
			if f.set == nil {
				value = dim.Render(value)
			}
			out = append(out, m.row(i, fmt.Sprintf("  %-18s", f.label))+" "+value)
		}
		if m.screen == "settings" && m.connectURL != "" {
			out = append(out, "", "  "+blue.Render(m.connectURL), "")
			out = append(out, qrLines(m.connectURL)...)
		}
	case m.active("rows"):
		rows := m.rows()
		if len(rows) == 0 {
			return []string{"", "  nothing here yet: o adds a row, :w saves"}
		}
		for i, r := range rows {
			out = append(out, m.row(i, "  "+r))
		}
	case m.active("deployments"):
		for i, d := range m.deps {
			duration := ""
			if d.Finished > 0 {
				duration = fmt.Sprintf("%ds", d.Finished-d.Started)
			}
			line := fmt.Sprintf("  #%-5d %s %-8.7s %-40.40s %-9s %s", d.Id, stateStyle(d.Status).Render(fmt.Sprintf("%-11s", d.Status)), d.Sha, d.Message, ago(d.Started), duration)
			if d.Error != "" {
				line += red.Render(" " + d.Error)
			}
			out = append(out, m.row(i, line))
		}
		if len(out) == 0 {
			out = []string{"", "  no deployments yet: <space>d deploys"}
		}
	case m.active("logs"):
		lo, hi := min(m.visual, m.cursor), max(m.visual, m.cursor)
		for i, l := range m.logs {
			switch {
			case m.visual >= 0 && i >= lo && i <= hi:
				l = vis.Render(l)
			case i == m.cursor && !m.follow:
				l = sel.Render(l)
			}
			out = append(out, l)
		}
		if len(out) == 0 {
			kind := "runtime"
			if m.logBuild {
				kind = "build"
			}
			out = []string{dim.Render("waiting for " + kind + " log… (b toggles build / runtime)")}
		}
	}
	return out
}

func (m *model) helpLines() []string {
	out := []string{bold.Render("keys on this screen") + dim.Render("  (? or esc closes)"), ""}
	for _, b := range bindings {
		if m.active(b.where) {
			out = append(out, fmt.Sprintf("  %-18s %s", strings.ReplaceAll(b.keys, " ", " / "), b.help))
		}
	}
	return append(out, "", bold.Render("commands"),
		"  :deploy [svc]  :rollback <id>  :restart [svc]  :stop [svc]  :new  :rm[!] <svc>",
		"  :e[!] <svc>  :logs [svc]  :settings  :set KEY=value  :unset KEY  :domain add|rm <host[/path]>",
		"  :config builds=N|email=…|host=…  :w  :wd  :wq  :q[!]  :qa[!]",
	)
}

func qrLines(s string) []string {
	code, err := qr.Encode(s, qr.L)
	if err != nil {
		return nil
	}
	black := func(x, y int) bool { return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y) }
	var out []string
	for y := -1; y <= code.Size; y += 2 {
		line := "  "
		for x := -1; x <= code.Size; x++ {
			switch top, bottom := black(x, y), black(x, y+1); {
			case top && bottom:
				line += " "
			case top:
				line += "▄"
			case bottom:
				line += "▀"
			default:
				line += "█"
			}
		}
		out = append(out, line)
	}
	return out
}
