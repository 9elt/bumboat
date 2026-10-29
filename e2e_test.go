//go:build e2e

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/9elt/bumboat/api"
)

func TestE2E(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BUMBOAT_DIR", dir)
	t.Setenv("BUMBOAT_KEY", filepath.Join(dir, "master.key"))
	t.Setenv("BUMBOAT_SOCKET", filepath.Join(dir, "bumboat.sock"))
	go func() { t.Log("daemon exited:", runDaemon()) }()
	ctx := context.Background()
	c := dial()
	for i := 0; ; i++ {
		if _, err := c.ListServices(ctx, &api.Empty{}); err == nil {
			break
		} else if i > 100 {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Cleanup(func() {
		for _, name := range []string{"web", "who"} {
			c.DeleteService(ctx, &api.Ref{Name: name})
		}
	})

	get := func(host string) (int, string) {
		req, _ := http.NewRequest("GET", "http://127.0.0.1/", nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	deploy := func(name string, dep *api.Deployment, err error) *api.Deployment {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for range 600 {
			list, _ := c.ListDeployments(ctx, &api.Ref{Name: name})
			for _, d := range list.Deployments {
				if d.Id == dep.Id && d.Status == "live" {
					return d
				}
				if d.Id == dep.Id && d.Status == "failed" {
					var log bytes.Buffer
					printLogs(c, &api.LogsRequest{Service: name, Deployment: dep.Id, Build: true}, &log)
					t.Fatalf("deployment %d failed: %s\n%s", d.Id, d.Error, log.String())
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("deployment %d never went live", dep.Id)
		return nil
	}

	_, err := c.CreateService(ctx, &api.Service{
		Name: "web", Source: "image", Image: "nginx:alpine", Port: 80, HealthPath: "/",
		Env:     []*api.EnvVar{{Key: "HELLO", Value: "world"}},
		Domains: []*api.Domain{{Host: "web.localhost"}},
		Volumes: []*api.Volume{{Name: "data", Path: "/data"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := c.Deploy(ctx, &api.Ref{Name: "web"})
	first := deploy("web", dep, err)
	if code, body := get("web.localhost"); code != 200 || !strings.Contains(body, "nginx") {
		t.Fatalf("web: %d %s", code, body)
	}
	sv, err := c.GetService(ctx, &api.Ref{Name: "web"})
	if err != nil || sv.State != "running" || sv.Domains[0].Cert != "plain http (local name)" {
		t.Fatalf("service: %+v %v", sv, err)
	}
	dk := newDocker()
	containers, _ := dk.containers(ctx)
	var env, binds string
	for _, ct := range containers {
		if ct.Labels["bumboat.service"] == "web" {
			var res struct {
				Config     struct{ Env []string }
				HostConfig struct{ Binds []string }
			}
			dk.do(ctx, "GET", "/containers/"+ct.Id+"/json", nil, &res)
			env, binds = strings.Join(res.Config.Env, " "), strings.Join(res.HostConfig.Binds, " ")
		}
	}
	if !strings.Contains(env, "HELLO=world") || binds != "bumboat-web-data:/data" {
		t.Fatalf("container env %q binds %q", env, binds)
	}

	var logs bytes.Buffer
	if err := printLogs(c, &api.LogsRequest{Service: "web", Tail: 50}, &logs); err != nil || !strings.Contains(logs.String(), "GET /") {
		t.Fatalf("runtime logs: %v %q", err, logs.String())
	}

	if _, err := c.Stop(ctx, &api.Ref{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := get("web.localhost"); code != http.StatusServiceUnavailable {
		t.Fatalf("stopped service answered %d", code)
	}
	if _, err := c.Restart(ctx, &api.Ref{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if code, _ := get("web.localhost"); code != 200 {
		t.Fatalf("restarted service answered %d", code)
	}

	_, err = c.CreateService(ctx, &api.Service{Name: "who", Source: "image", Image: "traefik/whoami", Port: 80, Domains: []*api.Domain{{Host: "who.localhost"}}})
	if err != nil {
		t.Fatal(err)
	}
	dep, err = c.Deploy(ctx, &api.Ref{Name: "who"})
	deploy("who", dep, err)
	var ok, failed atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if code, _ := get("who.localhost"); code == 200 {
				ok.Add(1)
			} else {
				failed.Add(1)
			}
		}
	}()
	dep, err = c.Deploy(ctx, &api.Ref{Name: "who"})
	deploy("who", dep, err)
	time.Sleep(12 * time.Second)
	close(stop)
	<-done
	if failed.Load() > 0 || ok.Load() < 100 {
		t.Fatalf("redeploy dropped requests: %d ok, %d failed", ok.Load(), failed.Load())
	}
	t.Logf("redeploy under load: %d requests, none failed", ok.Load())

	dep, err = c.Rollback(ctx, &api.Ref{Id: first.Id})
	if back := deploy("web", dep, err); back.Image != "nginx:alpine" {
		t.Fatalf("rollback image %s", back.Image)
	}

	d2 := &daemon{dir: dir, dk: dk, live: map[string]*live{}}
	if d2.st, err = openStore(dir, filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rm := exec.Command("docker", "buildx", "rm", "bumboat")
		rm.Env = d2.dockerEnv()
		rm.Run()
	})
	if err := d2.reconcile(ctx); err != nil || d2.live["web"] == nil || d2.live["who"] == nil {
		t.Fatalf("reconcile: %v %+v", err, d2.live)
	}

	build := func(name string, files map[string]string, sv *api.Service) string {
		src := t.TempDir()
		for f, content := range files {
			os.WriteFile(filepath.Join(src, f), []byte(content), 0o644)
		}
		var out bytes.Buffer
		image := "bumboat/e2e-" + name + ":1"
		if err := d2.build(ctx, sv, src, image, &out); err != nil {
			t.Fatalf("%s build: %v\n%s", name, err, out.String())
		}
		t.Cleanup(func() { dk.removeImage(ctx, image) })
		return out.String() + "\nimage " + image
	}

	out := build("dockerfile", map[string]string{"Dockerfile": "FROM alpine\nARG GREETING\nRUN echo \"greeting=$GREETING\"\n"},
		&api.Service{Build: "dockerfile", Dockerfile: "Dockerfile", Context: ".", Env: []*api.EnvVar{{Key: "GREETING", Value: "hi", Build: true}, {Key: "RUNTIME_ONLY", Value: "x"}}})
	if !strings.Contains(out, "greeting=hi") {
		t.Fatalf("build arg not passed:\n%s", out)
	}

	out = build("railpack", map[string]string{
		"package.json": `{"name":"e2e","version":"1.0.0","scripts":{"start":"node index.js"}}`,
		"index.js":     `require("http").createServer((q, s) => s.end("railpack " + process.env.MSG)).listen(process.env.PORT || 3000)`,
	}, &api.Service{Build: "railpack", Context: ".", Env: []*api.EnvVar{{Key: "BUILD_SECRET", Value: "s", Build: true}}})
	image := out[strings.LastIndex(out, "image ")+6:]
	d2.stopBuilder()
	if _, running, err := dk.inspect(ctx, "buildx_buildkit_bumboat0"); err != nil || running {
		t.Fatalf("the builder should stop once builds are done: running=%v err=%v", running, err)
	}
	cfg := &containerConfig{Image: image, Env: []string{"PORT=3000", "MSG=works"}, Labels: map[string]string{"e2e": "1"}}
	id, err := dk.create(ctx, "bumboat-e2e-railpack", cfg)
	if err == nil {
		defer dk.remove(ctx, id)
		err = dk.start(ctx, id)
	}
	if err != nil {
		t.Fatal(err)
	}
	ip, err := d2.health(ctx, &api.Service{Port: 3000, HealthPath: "/"}, id, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(fmt.Sprintf("http://%s:3000/", ip))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "railpack works" {
		t.Fatalf("railpack app answered %q", body)
	}
}
