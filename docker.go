package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type docker struct{ c *http.Client }

type dockerError struct {
	code int
	msg  string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker: %s (%d)", e.msg, e.code) }

func isNotFound(err error) bool {
	e, ok := err.(*dockerError)
	return ok && e.code == http.StatusNotFound
}

type containerSummary struct {
	Id     string
	Names  []string
	Labels map[string]string
	State  string
}

type containerConfig struct {
	Image      string
	Env        []string
	Labels     map[string]string
	HostConfig struct {
		NetworkMode   string
		Binds         []string
		RestartPolicy struct{ Name string }
		LogConfig     struct {
			Type   string
			Config map[string]string
		}
	}
}

func newDocker() *docker {
	return &docker{&http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	}}}
}

func (d *docker) stream(ctx context.Context, method, path string, body any) (io.ReadCloser, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, "http://docker/v1.44"+path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.c.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotModified {
		defer res.Body.Close()
		var e struct{ Message string }
		json.NewDecoder(res.Body).Decode(&e)
		return nil, &dockerError{res.StatusCode, e.Message}
	}
	return res.Body, nil
}

func (d *docker) do(ctx context.Context, method, path string, body, out any) error {
	rc, err := d.stream(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer rc.Close()
	if out != nil {
		return json.NewDecoder(rc).Decode(out)
	}
	_, err = io.Copy(io.Discard, rc)
	return err
}

func (d *docker) ensureNetwork(ctx context.Context) error {
	err := d.do(ctx, "GET", "/networks/bumboat", nil, nil)
	if isNotFound(err) {
		err = d.do(ctx, "POST", "/networks/create", map[string]string{"Name": "bumboat", "Driver": "bridge"}, nil)
	}
	return err
}

func (d *docker) pull(ctx context.Context, image string, w io.Writer) error {
	ref, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		ref, tag = image[:i], image[i+1:]
	}
	if strings.Contains(image, "@") {
		ref, tag = image, ""
	}
	rc, err := d.stream(ctx, "POST", "/images/create?fromImage="+url.QueryEscape(ref)+"&tag="+url.QueryEscape(tag), nil)
	if err != nil {
		return err
	}
	defer rc.Close()
	dec := json.NewDecoder(rc)
	for {
		var msg struct{ Status, Id, Error string }
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", image, msg.Error)
		}
		if msg.Id == "" || strings.HasPrefix(msg.Status, "Pull complete") {
			fmt.Fprintln(w, strings.TrimSpace(msg.Id+" "+msg.Status))
		}
	}
}

func (d *docker) create(ctx context.Context, name string, cfg *containerConfig) (string, error) {
	cfg.HostConfig.NetworkMode = "bumboat"
	cfg.HostConfig.RestartPolicy.Name = "unless-stopped"
	cfg.HostConfig.LogConfig.Type = "json-file"
	cfg.HostConfig.LogConfig.Config = map[string]string{"max-size": "10m", "max-file": "3"}
	var res struct{ Id string }
	err := d.do(ctx, "POST", "/containers/create?name="+name, cfg, &res)
	return res.Id, err
}

func (d *docker) start(ctx context.Context, id string) error {
	return d.do(ctx, "POST", "/containers/"+id+"/start", nil, nil)
}

func (d *docker) stop(ctx context.Context, id string) error {
	return d.do(ctx, "POST", "/containers/"+id+"/stop?t=10", nil, nil)
}

func (d *docker) restart(ctx context.Context, id string) error {
	return d.do(ctx, "POST", "/containers/"+id+"/restart?t=10", nil, nil)
}

func (d *docker) remove(ctx context.Context, id string) error {
	err := d.do(ctx, "DELETE", "/containers/"+id+"?force=true", nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (d *docker) containers(ctx context.Context) ([]containerSummary, error) {
	var list []containerSummary
	err := d.do(ctx, "GET", "/containers/json?all=1&filters="+url.QueryEscape(`{"label":["bumboat.service"]}`), nil, &list)
	return list, err
}

func (d *docker) inspect(ctx context.Context, id string) (ip string, running bool, err error) {
	var res struct {
		State           struct{ Running bool }
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	err = d.do(ctx, "GET", "/containers/"+id+"/json", nil, &res)
	return res.NetworkSettings.Networks["bumboat"].IPAddress, res.State.Running, err
}

func (d *docker) logs(ctx context.Context, id string, follow bool, tail int, line func(string) error) error {
	rc, err := d.stream(ctx, "GET", fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&follow=%t&tail=%d", id, follow, tail), nil)
	if err != nil {
		return err
	}
	defer rc.Close()
	pr, pw := io.Pipe()
	go func() {
		header := make([]byte, 8)
		for {
			if _, err := io.ReadFull(rc, header); err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := io.CopyN(pw, rc, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	defer pr.Close()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := line(sc.Text()); err != nil {
			return err
		}
	}
	return nil
}

func (d *docker) stats(ctx context.Context, id string) (cpu float64, mem int64, err error) {
	type cpuStats struct {
		CpuUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCpuUsage uint64 `json:"system_cpu_usage"`
		OnlineCpus     uint64 `json:"online_cpus"`
	}
	var s struct {
		CpuStats    cpuStats `json:"cpu_stats"`
		PrecpuStats cpuStats `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64
			Stats map[string]uint64
		} `json:"memory_stats"`
	}
	if err = d.do(ctx, "GET", "/containers/"+id+"/stats?stream=false", nil, &s); err != nil {
		return
	}
	if sys := float64(s.CpuStats.SystemCpuUsage - s.PrecpuStats.SystemCpuUsage); sys > 0 {
		cpu = float64(s.CpuStats.CpuUsage.TotalUsage-s.PrecpuStats.CpuUsage.TotalUsage) / sys * float64(s.CpuStats.OnlineCpus) * 100
	}
	return cpu, int64(s.MemoryStats.Usage - s.MemoryStats.Stats["inactive_file"]), nil
}

func (d *docker) images(ctx context.Context, repo string) ([]string, error) {
	var list []struct{ RepoTags []string }
	err := d.do(ctx, "GET", "/images/json?filters="+url.QueryEscape(`{"reference":["`+repo+`"]}`), nil, &list)
	var tags []string
	for _, img := range list {
		tags = append(tags, img.RepoTags...)
	}
	return tags, err
}

func (d *docker) removeImage(ctx context.Context, ref string) error {
	return d.do(ctx, "DELETE", "/images/"+ref, nil, nil)
}
