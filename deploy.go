package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/9elt/bumboat/api"
)

const railpackVersion = "v0.43.0"

var railpackFrontend = "ghcr.io/railwayapp/railpack-frontend:" + railpackVersion

type live struct {
	id, ip     string
	port       int32
	deployment int64
}

func (d *daemon) enqueue(service string, dep *api.Deployment) (*api.Deployment, error) {
	if _, err := d.st.service(service); err != nil {
		return nil, err
	}
	dep.Service, dep.Status, dep.Started = service, "queued", time.Now().Unix()
	if err := d.st.addDeployment(dep); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if old := d.pending[service]; old != nil {
		old.Status, old.Finished = "superseded", time.Now().Unix()
		d.st.saveDeployment(old)
	}
	d.pending[service] = dep
	d.mu.Unlock()
	d.kick()
	d.changed()
	return dep, nil
}

func (d *daemon) kick() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *daemon) dispatch() {
	for range d.wake {
		builds := d.builds()
		d.mu.Lock()
		for service, dep := range d.pending {
			if d.running[service] || len(d.running) >= builds {
				continue
			}
			delete(d.pending, service)
			d.running[service] = true
			go func() {
				d.run(dep)
				d.mu.Lock()
				delete(d.running, service)
				d.mu.Unlock()
				d.kick()
				d.changed()
			}()
		}
		d.mu.Unlock()
	}
}

func (d *daemon) setStatus(dep *api.Deployment, status string) {
	dep.Status = status
	if err := d.st.saveDeployment(dep); err != nil {
		log.Printf("deployment %d: %v", dep.Id, err)
	}
	d.changed()
}

func (d *daemon) run(dep *api.Deployment) {
	f, err := os.Create(filepath.Join(d.dir, "logs", strconv.FormatInt(dep.Id, 10)+".log"))
	if err != nil {
		log.Printf("deployment %d: %v", dep.Id, err)
		return
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	sv, err := d.st.service(dep.Service)
	var ghDeployment int64
	if err == nil {
		err = d.deploy(ctx, sv, dep, f, &ghDeployment)
	}
	dep.Finished = time.Now().Unix()
	state, description := "success", fmt.Sprintf("deployed in %ds", dep.Finished-dep.Started)
	if err != nil {
		fmt.Fprintf(f, "\nerror: %v\n", err)
		dep.Error = err.Error()
		d.setStatus(dep, "failed")
		state, description = "failure", fmt.Sprintf("%.140s", err)
	} else if err := d.st.saveDeployment(dep); err != nil {
		log.Printf("deployment %d: %v", dep.Id, err)
	}
	if sv != nil && sv.Source == "github" && dep.Sha != "" && ghDeployment != 0 {
		envURL := ""
		if len(sv.Domains) > 0 {
			envURL = "https://" + sv.Domains[0].Host + sv.Domains[0].Path
		}
		d.gh.report(ctx, sv.Repo, dep.Sha, sv.Name, state, description, envURL, &ghDeployment)
	}
}

func (d *daemon) deploy(ctx context.Context, sv *api.Service, dep *api.Deployment, w io.Writer, ghDeployment *int64) error {
	d.setStatus(dep, "building")
	switch {
	case dep.Image != "":
		fmt.Fprintf(w, "rolling back to %s\n", dep.Image)
	case sv.Source == "image":
		dep.Image, dep.Message = sv.Image, sv.Image
		fmt.Fprintf(w, "pulling %s\n", sv.Image)
		if err := d.dk.pull(ctx, sv.Image, w); err != nil {
			return err
		}
	default:
		dir := filepath.Join(d.dir, "build", strconv.FormatInt(dep.Id, 10))
		defer os.RemoveAll(dir)
		if err := d.clone(ctx, sv, dep, dir, w); err != nil {
			return err
		}
		d.gh.report(ctx, sv.Repo, dep.Sha, sv.Name, "in_progress", "building", "", ghDeployment)
		dep.Image = fmt.Sprintf("bumboat/%s:%d", sv.Name, dep.Id)
		if err := d.build(ctx, sv, dir, dep.Image, w); err != nil {
			return err
		}
	}

	d.setStatus(dep, "starting")
	cfg := &containerConfig{Image: dep.Image, Labels: map[string]string{
		"bumboat.service": sv.Name, "bumboat.deployment": strconv.FormatInt(dep.Id, 10), "bumboat.port": strconv.Itoa(int(sv.Port)),
	}}
	for _, e := range sv.Env {
		cfg.Env = append(cfg.Env, e.Key+"="+e.Value)
	}
	for _, v := range sv.Volumes {
		cfg.HostConfig.Binds = append(cfg.HostConfig.Binds, "bumboat-"+sv.Name+"-"+v.Name+":"+v.Path)
	}

	d.mu.Lock()
	old := d.live[sv.Name]
	d.mu.Unlock()
	if old != nil && len(sv.Volumes) > 0 {
		fmt.Fprintln(w, "stopping the previous container: services with volumes never overlap")
		if err := d.dk.stop(ctx, old.id); err != nil {
			return err
		}
	}

	id, err := d.dk.create(ctx, fmt.Sprintf("bumboat-%s-%d", sv.Name, dep.Id), cfg)
	if err == nil {
		err = d.dk.start(ctx, id)
	}
	var ip string
	if err == nil {
		fmt.Fprintf(w, "started container %.12s, waiting for it to be healthy\n", id)
		ip, err = d.health(ctx, sv, id, w)
	}
	if err != nil {
		if id != "" {
			d.dk.remove(context.Background(), id)
		}
		if old != nil && len(sv.Volumes) > 0 {
			d.dk.start(context.Background(), old.id)
		}
		return err
	}

	d.mu.Lock()
	d.live[sv.Name] = &live{id, ip, sv.Port, dep.Id}
	d.mu.Unlock()
	if err := d.st.setLive(sv.Name, dep.Id); err != nil {
		return err
	}
	if sv.Live != nil {
		sv.Live.Status = "superseded"
		d.st.saveDeployment(sv.Live)
	}
	d.routes()
	d.setStatus(dep, "live")
	fmt.Fprintln(w, "live")

	if old != nil {
		go func() {
			if len(sv.Volumes) == 0 {
				time.Sleep(10 * time.Second)
			}
			d.dk.remove(context.Background(), old.id)
		}()
	}
	if sv.Source == "github" {
		d.prune(ctx, sv.Name)
	}
	return nil
}

func (d *daemon) clone(ctx context.Context, sv *api.Service, dep *api.Deployment, dir string, w io.Writer) error {
	tok, err := d.gh.repoToken(ctx, sv.Repo)
	if err != nil {
		return err
	}
	ref := dep.Sha
	if ref == "" {
		ref = sv.Branch
	}
	fmt.Fprintf(w, "fetching %s@%s\n", sv.Repo, ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	git := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s", args[0], strings.ReplaceAll(string(out), tok, "***"))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("init", "-q"); err != nil {
		return err
	}
	if _, err := git("fetch", "-q", "--depth", "1", "https://x-access-token:"+tok+"@github.com/"+sv.Repo+".git", ref); err != nil {
		return err
	}
	if _, err := git("checkout", "-q", "FETCH_HEAD"); err != nil {
		return err
	}
	if dep.Sha, err = git("rev-parse", "HEAD"); err != nil {
		return err
	}
	dep.Message, err = git("log", "-1", "--format=%s")
	fmt.Fprintf(w, "commit %.12s %s\n", dep.Sha, dep.Message)
	d.setStatus(dep, "building")
	return err
}

func (d *daemon) build(ctx context.Context, sv *api.Service, dir, image string, w io.Writer) error {
	context := filepath.Join(dir, sv.Context)
	args := []string{"build", "--progress=plain", "-t", image}
	env := os.Environ()
	var buildEnv []*api.EnvVar
	for _, e := range sv.Env {
		if e.Build {
			buildEnv = append(buildEnv, e)
			env = append(env, e.Key+"="+e.Value)
		}
	}

	if sv.Build == "railpack" {
		bin, err := d.railpack(ctx, w)
		if err != nil {
			return err
		}
		plan := filepath.Join(dir, "railpack-plan.json")
		prepare := []string{"prepare", context, "--plan-out", plan}
		hash := sha256.New()
		for _, e := range buildEnv {
			prepare = append(prepare, "--env", e.Key+"="+e.Value)
			args = append(args, "--secret", "id="+e.Key+",env="+e.Key)
			hash.Write([]byte(e.Key + "=" + e.Value + "\n"))
		}
		cmd := exec.CommandContext(ctx, bin, prepare...)
		cmd.Stdout, cmd.Stderr = w, w
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("railpack prepare: %w", err)
		}
		args = append(args, "--build-arg", "BUILDKIT_SYNTAX="+railpackFrontend, "--build-arg", "secrets-hash="+hex.EncodeToString(hash.Sum(nil)), "-f", plan)
	} else {
		args = append(args, "-f", filepath.Join(dir, sv.Dockerfile))
		for _, e := range buildEnv {
			args = append(args, "--build-arg", e.Key)
		}
	}

	cmd := exec.CommandContext(ctx, "docker", append(args, context)...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

func (d *daemon) railpack(ctx context.Context, w io.Writer) (string, error) {
	bin := filepath.Join(d.dir, "bin", "railpack-"+railpackVersion)
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
	asset := fmt.Sprintf("railpack-%s-%s-unknown-linux-musl.tar.gz", railpackVersion, arch)
	base := "https://github.com/railwayapp/railpack/releases/download/" + railpackVersion + "/"
	fmt.Fprintf(w, "downloading %s\n", asset)

	get := func(name string) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+name, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download %s: %s", name, res.Status)
		}
		return io.ReadAll(res.Body)
	}
	sums, err := get("checksums.txt")
	if err != nil {
		return "", err
	}
	archive, err := get(asset)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(archive)
	if !strings.Contains(string(sums), hex.EncodeToString(sum[:])+"  "+asset) {
		return "", fmt.Errorf("%s: checksum mismatch", asset)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return "", fmt.Errorf("%s: no railpack binary inside", asset)
		}
		if filepath.Base(h.Name) == "railpack" {
			os.MkdirAll(filepath.Dir(bin), 0o755)
			f, err := os.OpenFile(bin+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return "", err
			}
			return bin, os.Rename(bin+".tmp", bin)
		}
	}
}

func (d *daemon) health(ctx context.Context, sv *api.Service, id string, w io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	since := time.Now()
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		ip, running, err := d.dk.inspect(ctx, id)
		if err != nil {
			return "", err
		}
		if !running {
			fmt.Fprintln(w, "container exited, last output:")
			d.dk.logs(context.Background(), id, false, 30, func(l string) error { _, err := fmt.Fprintln(w, l); return err })
			return "", errors.New("container exited before becoming healthy")
		}
		addr := net.JoinHostPort(ip, strconv.Itoa(int(sv.Port)))
		switch {
		case ip == "":
		case sv.Port == 0:
			if time.Since(since) > 3*time.Second {
				return ip, nil
			}
		case sv.HealthPath != "":
			if res, err := client.Get("http://" + addr + sv.HealthPath); err == nil {
				res.Body.Close()
				if res.StatusCode < 500 {
					fmt.Fprintf(w, "GET %s → %d\n", sv.HealthPath, res.StatusCode)
					return ip, nil
				}
			}
		default:
			if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
				c.Close()
				fmt.Fprintf(w, "port %d is accepting connections\n", sv.Port)
				return ip, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("not healthy after 60s on port %d", sv.Port)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (d *daemon) prune(ctx context.Context, service string) {
	deps, err := d.st.deployments(service, 50)
	if err != nil {
		return
	}
	var keep []string
	for _, dep := range deps {
		if (dep.Status == "live" || dep.Status == "superseded") && len(keep) < 3 {
			keep = append(keep, dep.Image)
		}
	}
	tags, _ := d.dk.images(ctx, "bumboat/"+service)
	for _, tag := range tags {
		if !slices.Contains(keep, tag) {
			d.dk.removeImage(ctx, tag)
		}
	}
}

func (d *daemon) rollback(id int64) (*api.Deployment, error) {
	target, err := d.st.deployment(id)
	if err != nil {
		return nil, fmt.Errorf("no deployment %d", id)
	}
	if target.Image == "" {
		return nil, fmt.Errorf("deployment %d never produced an image", id)
	}
	return d.enqueue(target.Service, &api.Deployment{Sha: target.Sha, Message: "rollback to #" + strconv.FormatInt(id, 10) + " " + target.Message, Image: target.Image})
}

func (d *daemon) tailBuildLog(ctx context.Context, id int64, follow bool, send func(string) error) error {
	path := filepath.Join(d.dir, "logs", strconv.FormatInt(id, 10)+".log")
	var r *bufio.Reader
	var partial string
	for {
		dep, err := d.st.deployment(id)
		if err != nil {
			return fmt.Errorf("no deployment %d", id)
		}
		done := dep.Finished != 0 || dep.Status == "failed" || dep.Status == "superseded"
		if r == nil {
			if f, err := os.Open(path); err == nil {
				defer f.Close()
				r = bufio.NewReader(f)
			}
		}
		for r != nil {
			line, err := r.ReadString('\n')
			partial += line
			if err != nil {
				break
			}
			if err := send(strings.TrimSuffix(partial, "\n")); err != nil {
				return err
			}
			partial = ""
		}
		if !follow || done {
			if partial != "" {
				return send(partial)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}
