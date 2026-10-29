package main

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/9elt/bumboat/api"
	tea "github.com/charmbracelet/bubbletea"
)

func testDaemon(t *testing.T) *daemon {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "logs"), 0o700)
	st, err := openStore(dir, filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{
		dir: dir, st: st,
		live: map[string]*live{}, pending: map[string]*api.Deployment{}, running: map[string]bool{},
		stats: map[string][2]float64{}, wake: make(chan struct{}, 1), subs: map[chan *api.ServiceList]bool{},
	}
	d.gh = &github{st: st, tokens: map[string]ghToken{}}
	d.px = newProxy(d)
	return d
}

func TestStoreValidationAndSecrets(t *testing.T) {
	d := testDaemon(t)
	for _, bad := range []*api.Service{
		{Name: "Bad Name", Source: "image", Image: "nginx"},
		{Name: "web", Source: "github", Repo: "nope", Branch: "main"},
		{Name: "web", Source: "image", Image: "nginx", Domains: []*api.Domain{{Host: "example.com"}}},
		{Name: "web", Source: "image", Image: "nginx", Port: 80, Domains: []*api.Domain{{Host: "example.com", Path: "/api/"}}},
		{Name: "web", Source: "image", Image: "nginx", Env: []*api.EnvVar{{Key: "1BAD"}}},
		{Name: "web", Source: "github", Repo: "a/b", Branch: "main", Dockerfile: "../etc/passwd"},
	} {
		if err := d.st.saveService(bad, true, false); err == nil {
			t.Errorf("saved invalid service %+v", bad)
		}
	}

	sv := &api.Service{Name: "web", Source: "image", Image: "nginx", Port: 80,
		Env:     []*api.EnvVar{{Key: "SECRET", Value: "hunter2", Build: true}},
		Domains: []*api.Domain{{Host: "example.com"}, {Host: "example.com", Path: "/api"}},
		Volumes: []*api.Volume{{Name: "data", Path: "/data"}},
	}
	if err := d.st.saveService(sv, true, false); err != nil {
		t.Fatal(err)
	}
	var raw string
	d.st.db.QueryRow(`SELECT value FROM env`).Scan(&raw)
	if strings.Contains(raw, "hunter2") {
		t.Fatal("env value stored in plain text")
	}
	got, err := d.st.service("web")
	if err != nil || got.Env[0].Value != "hunter2" || !got.Env[0].Build || len(got.Domains) != 2 || got.Volumes[0].Path != "/data" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if err := d.st.saveService(&api.Service{Name: "other", Source: "image", Image: "x", Port: 1, Domains: []*api.Domain{{Host: "example.com"}}}, true, false); err == nil {
		t.Fatal("two services claimed the same domain")
	}

	dep := &api.Deployment{Service: "web", Status: "live", Started: 1}
	d.st.addDeployment(dep)
	d.st.setLive("web", dep.Id)
	got.Port = 81
	if err := d.st.saveService(got, false, true); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.st.service("web"); !got.Dirty {
		t.Fatal("changing the port of a live service should mark it dirty")
	}
	d.st.setSetting("secret_github_key", "pem")
	if d.st.setting("secret_github_key") != "pem" {
		t.Fatal("secret setting round trip")
	}
}

func TestUpdateServiceDirtyOnlyForDeployChanges(t *testing.T) {
	d := testDaemon(t)
	ctx := context.Background()
	sv := &api.Service{Name: "web", Source: "image", Image: "nginx", Port: 80, AutoDeploy: true}
	if _, err := d.CreateService(ctx, sv); err != nil {
		t.Fatal(err)
	}
	dep := &api.Deployment{Service: "web", Status: "live", Started: 1}
	d.st.addDeployment(dep)
	d.st.setLive("web", dep.Id)

	sv.Domains = []*api.Domain{{Host: "web.localhost"}}
	if got, err := d.UpdateService(ctx, sv); err != nil || got.Dirty {
		t.Fatalf("adding a domain needs no redeploy: dirty=%v err=%v", got.GetDirty(), err)
	}
	sv.Env = []*api.EnvVar{{Key: "A", Value: "1"}}
	if got, err := d.UpdateService(ctx, sv); err != nil || !got.Dirty {
		t.Fatalf("an env change needs a redeploy: dirty=%v err=%v", got.GetDirty(), err)
	}
}

func TestQueueSupersedesPendingDeployments(t *testing.T) {
	d := testDaemon(t)
	d.st.saveService(&api.Service{Name: "web", Source: "image", Image: "nginx"}, true, false)
	first, err := d.enqueue("web", &api.Deployment{Sha: "a"})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := d.enqueue("web", &api.Deployment{Sha: "b"})
	if got, _ := d.st.deployment(first.Id); got.Status != "superseded" {
		t.Fatalf("first deployment is %s", got.Status)
	}
	if d.pending["web"].Id != second.Id {
		t.Fatal("the newest push should be the pending one")
	}
	if _, err := d.enqueue("nope", &api.Deployment{}); err == nil {
		t.Fatal("enqueued a deployment for a missing service")
	}
}

func TestWebhookPush(t *testing.T) {
	d := testDaemon(t)
	d.st.setSetting("secret_github_webhook", "s3cret")
	d.st.setSetting("webhook_host", "hooks.localhost")
	d.st.saveService(&api.Service{Name: "api", Source: "github", Repo: "acme/api", Branch: "main", AutoDeploy: true}, true, false)
	d.st.saveService(&api.Service{Name: "manual", Source: "github", Repo: "acme/api", Branch: "main"}, true, false)
	d.st.saveService(&api.Service{Name: "staging", Source: "github", Repo: "acme/api", Branch: "dev", AutoDeploy: true}, true, false)

	body := `{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"Acme/API"},"head_commit":{"message":"fix: things\n\nmore"}}`
	send := func(sig string) int {
		req := httptest.NewRequest("POST", "https://hooks.localhost/webhooks/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", sig)
		rec := httptest.NewRecorder()
		d.px.serve(rec, req)
		return rec.Code
	}
	if code := send("sha256=deadbeef"); code != http.StatusUnauthorized {
		t.Fatalf("bad signature got %d", code)
	}
	mac := "sha256=" + hmacHex("s3cret", body)
	if code := send(mac); code != http.StatusAccepted {
		t.Fatalf("good signature got %d", code)
	}
	if len(d.pending) != 1 || d.pending["api"] == nil || d.pending["api"].Sha != "abc123" || d.pending["api"].Message != "fix: things" {
		t.Fatalf("pending: %+v", d.pending)
	}
}

func hmacHex(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubManifestFlowAndTokens(t *testing.T) {
	d := testDaemon(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	var statuses []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app-manifests/CODE/conversions":
			json.NewEncoder(w).Encode(map[string]any{"id": 42, "slug": "bumboat-test", "pem": keyPEM, "webhook_secret": "whsec", "html_url": "https://github.com/apps/bumboat-test"})
		case r.URL.Path == "/repos/acme/api/installation":
			jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parts := strings.Split(jwt, ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
				http.Error(w, "bad jwt", 401)
				return
			}
			claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
			if !strings.Contains(string(claims), `"iss":"42"`) {
				http.Error(w, "bad iss", 401)
				return
			}
			w.Write([]byte(`{"id":7}`))
		case r.URL.Path == "/app/installations/7/access_tokens":
			w.Write([]byte(`{"token":"ghs_x","expires_at":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`))
		case r.URL.Path == "/repos/acme/api/branches":
			if r.Header.Get("Authorization") != "Bearer ghs_x" {
				http.Error(w, "no token", 401)
				return
			}
			w.Write([]byte(`[{"name":"main"},{"name":"dev"}]`))
		case r.URL.Path == "/repos/acme/api/contents/Dockerfile":
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/api/statuses/"):
			body, _ := io.ReadAll(r.Body)
			statuses = append(statuses, string(body))
		case r.URL.Path == "/repos/acme/api/deployments":
			w.Write([]byte(`{"id":99}`))
		case r.URL.Path == "/repos/acme/api/deployments/99/statuses":
			body, _ := io.ReadAll(r.Body)
			statuses = append(statuses, string(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	githubAPI = gh.URL

	reply, err := d.ConnectGitHub(context.Background(), &api.ConnectGitHubRequest{WebhookHost: "hooks.localhost"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(reply.Url)
	token := u.Query().Get("token")

	rec := httptest.NewRecorder()
	d.px.serve(rec, httptest.NewRequest("GET", "https://hooks.localhost/setup/github?token="+token, nil))
	if !strings.Contains(rec.Body.String(), "https://github.com/settings/apps/new?state="+token) || !strings.Contains(rec.Body.String(), "webhooks/github") {
		t.Fatalf("manifest page: %s", rec.Body)
	}
	rec = httptest.NewRecorder()
	d.px.serve(rec, httptest.NewRequest("GET", "https://hooks.localhost/setup/github/callback?code=CODE&state=wrong", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback with a wrong state got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	d.px.serve(rec, httptest.NewRequest("GET", "https://hooks.localhost/setup/github/callback?code=CODE&state="+token, nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://github.com/apps/bumboat-test/installations/new" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if d.st.setting("github_app") != "bumboat-test" || d.st.setting("secret_github_webhook") != "whsec" || d.checkSetupToken(token) {
		t.Fatal("app credentials not stored or setup token still valid")
	}

	branches, err := d.ListBranches(context.Background(), &api.Ref{Name: "acme/api"})
	if err != nil || strings.Join(branches.Branches, ",") != "main,dev" {
		t.Fatalf("branches: %v %v", branches, err)
	}
	if s, err := d.SuggestBuild(context.Background(), &api.SuggestBuildRequest{Repo: "acme/api", Branch: "main"}); err != nil || s.Build != "railpack" {
		t.Fatalf("suggest: %v %v", s, err)
	}
	var ghDeployment int64
	d.gh.report(context.Background(), "acme/api", "abc", "api", "success", "ok", "https://api.example.com", &ghDeployment)
	if ghDeployment != 99 || len(statuses) != 2 || !strings.Contains(statuses[0], `"state":"success"`) || !strings.Contains(statuses[1], "api.example.com") {
		t.Fatalf("report: %d %v", ghDeployment, statuses)
	}
}

func TestProxyRouting(t *testing.T) {
	d := testDaemon(t)
	backend := func(name string) (string, int32) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(name + " " + r.Host + " " + r.Header.Get("X-Forwarded-Proto")))
		}))
		t.Cleanup(srv.Close)
		host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		p, _ := strconv.Atoi(port)
		return host, int32(p)
	}
	webIP, webPort := backend("web")
	apiIP, apiPort := backend("api")
	d.st.saveService(&api.Service{Name: "web", Source: "image", Image: "x", Port: webPort, Domains: []*api.Domain{{Host: "app.localhost"}}}, true, false)
	d.st.saveService(&api.Service{Name: "api", Source: "image", Image: "x", Port: apiPort, Domains: []*api.Domain{{Host: "app.localhost", Path: "/api"}}}, true, false)
	d.st.saveService(&api.Service{Name: "down", Source: "image", Image: "x", Port: 1, Domains: []*api.Domain{{Host: "down.localhost"}}}, true, false)
	d.live["web"] = &live{"c1", webIP, webPort, 1}
	d.live["api"] = &live{"c2", apiIP, apiPort, 2}
	d.routes()

	get := func(target string) (int, string) {
		rec := httptest.NewRecorder()
		d.px.serveHTTP(rec, httptest.NewRequest("GET", target, nil))
		return rec.Code, rec.Body.String()
	}
	for target, want := range map[string]string{
		"http://app.localhost/":        "web app.localhost",
		"http://app.localhost/apiary":  "web",
		"http://app.localhost/api":     "api app.localhost",
		"http://app.localhost/api/v1/": "api",
	} {
		if code, body := get(target); code != 200 || !strings.HasPrefix(body, want) {
			t.Errorf("%s: %d %q, want %q", target, code, body, want)
		}
	}
	if code, _ := get("http://down.localhost/"); code != http.StatusServiceUnavailable {
		t.Errorf("not running service: %d", code)
	}
	if code, _ := get("http://unknown.localhost/"); code != http.StatusNotFound {
		t.Errorf("unknown host: %d", code)
	}
	rec := httptest.NewRecorder()
	d.px.serveHTTP(rec, httptest.NewRequest("GET", "http://example.com/path?q=1", nil))
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://example.com/path?q=1" {
		t.Errorf("public host over http: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestTailBuildLog(t *testing.T) {
	d := testDaemon(t)
	d.st.saveService(&api.Service{Name: "web", Source: "image", Image: "x"}, true, false)
	dep := &api.Deployment{Service: "web", Status: "building", Started: 1}
	d.st.addDeployment(dep)
	path := filepath.Join(d.dir, "logs", strconv.FormatInt(dep.Id, 10)+".log")
	os.WriteFile(path, []byte("one\ntw"), 0o600)
	go func() {
		time.Sleep(400 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("o\nthree\n")
		f.Close()
		dep.Status, dep.Finished = "live", 2
		d.st.saveDeployment(dep)
	}()
	var lines []string
	err := d.tailBuildLog(context.Background(), dep.Id, true, func(l string) error { lines = append(lines, l); return nil })
	if err != nil || strings.Join(lines, ",") != "one,two,three" {
		t.Fatalf("got %q %v", lines, err)
	}
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestVimMotions(t *testing.T) {
	m := newModel(nil)
	m.h, m.list = 14, &api.ServiceList{}
	for i := range 30 {
		m.list.Services = append(m.list.Services, &api.Service{Name: "svc" + strconv.Itoa(i)})
	}
	press := func(keys ...string) tea.Cmd {
		var cmd tea.Cmd
		for _, k := range keys {
			_, cmd = m.Update(keyMsg(k))
		}
		return cmd
	}
	press("5", "j")
	if m.cursor != 5 {
		t.Fatalf("5j: cursor %d", m.cursor)
	}
	press("k")
	press("G")
	if m.cursor != 29 {
		t.Fatalf("G: cursor %d", m.cursor)
	}
	if cmd := press("g"); cmd == nil || m.keys != "g" {
		t.Fatal("g should wait for the rest of the sequence")
	}
	press("g")
	if m.cursor != 0 || m.keys != "" {
		t.Fatalf("gg: cursor %d keys %q", m.cursor, m.keys)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3j")})
	if m.cursor != 3 {
		t.Fatalf("3j delivered in one read: cursor %d", m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("gg")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":"), Alt: true})
	if m.mode != command {
		t.Fatal("esc followed by : in one read should open the command line")
	}
	m.Update(keyMsg("esc"))
	press("ctrl+d")
	if m.cursor != m.bodyHeight()/2 {
		t.Fatalf("ctrl-d: cursor %d", m.cursor)
	}
	press("1", "2", "j")
	if m.cursor != m.bodyHeight()/2+12 {
		t.Fatalf("12j: cursor %d", m.cursor)
	}
	press("/")
	if m.mode != command {
		t.Fatal("/ should open the search line")
	}
	for _, r := range "svc2" {
		press(string(r))
	}
	press("enter")
	if got := m.list.Services[m.cursor].Name; !strings.HasPrefix(got, "svc2") {
		t.Fatalf("search landed on %s", got)
	}
	before := m.cursor
	press("n")
	if m.cursor == before {
		t.Fatal("n did not move to the next match")
	}
	press(":")
	for _, r := range "bogus" {
		press(string(r))
	}
	press("enter")
	if !m.isErr || !strings.Contains(m.msg, "E492") {
		t.Fatalf("unknown command message: %q", m.msg)
	}
	press("d")
	if m.keys != "d" {
		t.Fatal("d should wait for dd")
	}
	press("d")
	if !strings.HasPrefix(m.confirm, "delete svc") {
		t.Fatalf("dd should ask before deleting, got %q", m.confirm)
	}
	press("n")
	if m.confirm != "" || m.msg != "cancelled" {
		t.Fatal("anything but y cancels")
	}
}

func TestServiceDraftEditing(t *testing.T) {
	sv := &api.Service{Name: "web", Source: "image", Image: "nginx", Port: 80}
	m := newModel(nil)
	m.screen, m.name, m.base, m.draft, m.tab = "service", "web", sv, config(sv), 3
	press := func(keys ...string) {
		for _, k := range keys {
			m.Update(keyMsg(k))
		}
	}
	press("o")
	for _, r := range "API_KEY=abc" {
		press(string(r))
	}
	press("esc")
	if len(m.draft.Env) != 1 || m.draft.Env[0].Key != "API_KEY" || m.draft.Env[0].Value != "abc" || !m.unsaved() {
		t.Fatalf("env after o: %+v", m.draft.Env)
	}
	press("b")
	if !m.draft.Env[0].Build {
		t.Fatal("b should mark the variable as build-time")
	}
	press("u", "u")
	if len(m.draft.Env) != 0 || m.unsaved() {
		t.Fatalf("uu should undo both edits: %+v", m.draft.Env)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	if len(m.draft.Env) != 1 || m.draft.Env[0].Build {
		t.Fatalf("first redo: %+v", m.draft.Env)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	if len(m.draft.Env) != 1 || !m.draft.Env[0].Build {
		t.Fatalf("redo: %+v", m.draft.Env)
	}
	press("d", "d")
	if len(m.draft.Env) != 0 {
		t.Fatal("dd should delete the row")
	}
	press(":")
	for _, r := range "domain add app.example.com/api" {
		press(string(r))
	}
	press("enter")
	if len(m.draft.Domains) != 1 || m.draft.Domains[0].Host != "app.example.com" || m.draft.Domains[0].Path != "/api" {
		t.Fatalf("domains: %+v", m.draft.Domains)
	}
	press(":", "q", "enter")
	if !strings.Contains(m.msg, "E37") {
		t.Fatalf(":q with unsaved changes should refuse, got %q", m.msg)
	}
}
