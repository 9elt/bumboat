package main

import (
	"bytes"
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
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/9elt/bumboat/api"
)

var githubAPI = "https://api.github.com"

type github struct {
	st     *store
	mu     sync.Mutex
	tokens map[string]ghToken
}

type ghToken struct {
	value   string
	expires time.Time
}

type githubError struct {
	code int
	msg  string
}

func (e *githubError) Error() string { return fmt.Sprintf("github: %s (%d)", e.msg, e.code) }

func (g *github) appJWT() (string, error) {
	id, key := g.st.setting("github_app_id"), g.st.setting("secret_github_key")
	if id == "" {
		return "", errors.New("GitHub is not connected: Settings → g")
	}
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return "", errors.New("GitHub app key is corrupt")
	}
	rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	now := time.Now().Unix()
	unsigned := b64([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + b64(fmt.Appendf(nil, `{"iat":%d,"exp":%d,"iss":"%s"}`, now-60, now+540, id))
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(nil, rsaKey, crypto.SHA256, sum[:])
	return unsigned + "." + b64(sig), err
}

func (g *github) call(ctx context.Context, method, path, auth string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, githubAPI+path, r)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e struct{ Message string }
		json.NewDecoder(res.Body).Decode(&e)
		return &githubError{res.StatusCode, e.Message}
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

func (g *github) repoToken(ctx context.Context, repo string) (string, error) {
	g.mu.Lock()
	t, ok := g.tokens[repo]
	g.mu.Unlock()
	if ok && time.Until(t.expires) > 5*time.Minute {
		return t.value, nil
	}
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	var inst struct{ Id int64 }
	if err := g.call(ctx, "GET", "/repos/"+repo+"/installation", jwt, nil, &inst); err != nil {
		return "", fmt.Errorf("%s: is the app installed on this repo? %w", repo, err)
	}
	var tok struct {
		Token     string
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.call(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", inst.Id), jwt, nil, &tok); err != nil {
		return "", err
	}
	g.mu.Lock()
	g.tokens[repo] = ghToken{tok.Token, tok.ExpiresAt}
	g.mu.Unlock()
	return tok.Token, nil
}

func (g *github) repos(ctx context.Context) ([]*api.Repo, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return nil, err
	}
	var installations []struct{ Id int64 }
	if err := g.call(ctx, "GET", "/app/installations?per_page=100", jwt, nil, &installations); err != nil {
		return nil, err
	}
	var repos []*api.Repo
	for _, inst := range installations {
		var tok struct{ Token string }
		if err := g.call(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", inst.Id), jwt, nil, &tok); err != nil {
			return nil, err
		}
		for page := 1; ; page++ {
			var res struct {
				Repositories []struct {
					FullName      string `json:"full_name"`
					DefaultBranch string `json:"default_branch"`
				}
			}
			if err := g.call(ctx, "GET", fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), tok.Token, nil, &res); err != nil {
				return nil, err
			}
			for _, r := range res.Repositories {
				repos = append(repos, &api.Repo{FullName: r.FullName, DefaultBranch: r.DefaultBranch})
			}
			if len(res.Repositories) < 100 {
				break
			}
		}
	}
	return repos, nil
}

func (g *github) branches(ctx context.Context, repo string) ([]string, error) {
	tok, err := g.repoToken(ctx, repo)
	if err != nil {
		return nil, err
	}
	var names []string
	for page := 1; ; page++ {
		var res []struct{ Name string }
		if err := g.call(ctx, "GET", fmt.Sprintf("/repos/%s/branches?per_page=100&page=%d", repo, page), tok, nil, &res); err != nil {
			return nil, err
		}
		for _, b := range res {
			names = append(names, b.Name)
		}
		if len(res) < 100 {
			return names, nil
		}
	}
}

func (g *github) hasFile(ctx context.Context, repo, branch, path string) (bool, error) {
	tok, err := g.repoToken(ctx, repo)
	if err != nil {
		return false, err
	}
	err = g.call(ctx, "GET", "/repos/"+repo+"/contents/"+path+"?ref="+branch, tok, nil, nil)
	if e, ok := err.(*githubError); ok && e.code == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func (g *github) report(ctx context.Context, repo, sha, service, state, description, envURL string, deployment *int64) {
	tok, err := g.repoToken(ctx, repo)
	if err != nil {
		log.Printf("github report %s: %v", repo, err)
		return
	}
	commitState := map[string]string{"in_progress": "pending", "success": "success", "failure": "failure"}[state]
	if err := g.call(ctx, "POST", "/repos/"+repo+"/statuses/"+sha, tok, map[string]string{
		"state": commitState, "description": description, "context": "bumboat/" + service,
	}, nil); err != nil {
		log.Printf("github status %s@%s: %v", repo, sha, err)
	}
	if *deployment == 0 {
		var res struct{ Id int64 }
		if err := g.call(ctx, "POST", "/repos/"+repo+"/deployments", tok, map[string]any{
			"ref": sha, "environment": service, "auto_merge": false, "required_contexts": []string{},
		}, &res); err != nil {
			log.Printf("github deployment %s@%s: %v", repo, sha, err)
			return
		}
		*deployment = res.Id
	}
	if err := g.call(ctx, "POST", fmt.Sprintf("/repos/%s/deployments/%d/statuses", repo, *deployment), tok, map[string]string{
		"state": state, "description": description, "environment_url": envURL,
	}, nil); err != nil {
		log.Printf("github deployment status %s@%s: %v", repo, sha, err)
	}
}

func validSignature(secret string, body []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(header))
}

var manifestPage = template.Must(template.New("").Parse(`<!doctype html>
<title>Bumboat · connect GitHub</title>
<form id="f" method="post" action="{{.Action}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<button>Create the GitHub App</button>
</form>
<script>document.getElementById("f").submit()</script>`))

func (d *daemon) setupToken() (string, error) {
	tok := make([]byte, 16)
	rand.Read(tok)
	t := hex.EncodeToString(tok)
	if err := d.st.setSetting("github_setup_token", t); err != nil {
		return "", err
	}
	return t, d.st.setSetting("github_setup_expires", strconv.FormatInt(time.Now().Add(15*time.Minute).Unix(), 10))
}

func (d *daemon) checkSetupToken(t string) bool {
	expires, _ := strconv.ParseInt(d.st.setting("github_setup_expires"), 10, 64)
	return t != "" && hmac.Equal([]byte(t), []byte(d.st.setting("github_setup_token"))) && time.Now().Unix() < expires
}

func (d *daemon) githubHTTP(w http.ResponseWriter, r *http.Request) {
	host := d.st.setting("webhook_host")
	switch r.URL.Path {
	case "/setup/github":
		if !d.checkSetupToken(r.URL.Query().Get("token")) {
			http.Error(w, "this setup link has expired: start again from the TUI", http.StatusForbidden)
			return
		}
		suffix := make([]byte, 3)
		rand.Read(suffix)
		name := "bumboat-" + strings.Split(host, ".")[0]
		manifest, _ := json.Marshal(map[string]any{
			"name":            fmt.Sprintf("%.27s-%x", name, suffix),
			"url":             "https://" + host,
			"hook_attributes": map[string]string{"url": "https://" + host + "/webhooks/github"},
			"redirect_url":    "https://" + host + "/setup/github/callback",
			"setup_url":       "https://" + host + "/setup/github/done",
			"public":          false,
			"default_permissions": map[string]string{
				"contents": "read", "metadata": "read", "statuses": "write", "deployments": "write",
			},
			"default_events": []string{"push"},
		})
		action := "https://github.com/settings/apps/new"
		if org := d.st.setting("github_setup_org"); org != "" {
			action = "https://github.com/organizations/" + org + "/settings/apps/new"
		}
		manifestPage.Execute(w, map[string]string{"Action": action + "?state=" + r.URL.Query().Get("token"), "Manifest": string(manifest)})

	case "/setup/github/callback":
		if !d.checkSetupToken(r.URL.Query().Get("state")) {
			http.Error(w, "this setup link has expired: start again from the TUI", http.StatusForbidden)
			return
		}
		var app struct {
			Id            int64
			Slug          string
			Pem           string
			WebhookSecret string `json:"webhook_secret"`
			HtmlUrl       string `json:"html_url"`
		}
		if err := d.gh.call(r.Context(), "POST", "/app-manifests/"+url.PathEscape(r.URL.Query().Get("code"))+"/conversions", "", nil, &app); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for k, v := range map[string]string{
			"github_app_id": strconv.FormatInt(app.Id, 10), "github_app": app.Slug,
			"secret_github_key": app.Pem, "secret_github_webhook": app.WebhookSecret, "github_setup_token": "",
		} {
			if err := d.st.setSetting(k, v); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		d.changed()
		http.Redirect(w, r, app.HtmlUrl+"/installations/new", http.StatusFound)

	case "/setup/github/done":
		fmt.Fprint(w, "Bumboat is connected to GitHub. Go back to the TUI to pick a repository.")

	case "/webhooks/github":
		body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
		if err != nil || r.Method != http.MethodPost || !validSignature(d.st.setting("secret_github_webhook"), body, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-GitHub-Event") == "push" {
			d.push(body)
		}
		w.WriteHeader(http.StatusAccepted)

	default:
		http.NotFound(w, r)
	}
}

func (d *daemon) push(body []byte) {
	var p struct {
		Ref        string
		After      string
		Deleted    bool
		Repository struct {
			FullName string `json:"full_name"`
		}
		HeadCommit struct{ Message string } `json:"head_commit"`
	}
	if json.Unmarshal(body, &p) != nil || p.Deleted {
		return
	}
	list, _, err := d.st.services("")
	if err != nil {
		log.Printf("push: %v", err)
		return
	}
	for _, sv := range list {
		if sv.Source == "github" && sv.AutoDeploy && strings.EqualFold(sv.Repo, p.Repository.FullName) && p.Ref == "refs/heads/"+sv.Branch {
			message, _, _ := strings.Cut(p.HeadCommit.Message, "\n")
			if _, err := d.enqueue(sv.Name, &api.Deployment{Sha: p.After, Message: message}); err != nil {
				log.Printf("push %s: %v", sv.Name, err)
			}
		}
	}
}
