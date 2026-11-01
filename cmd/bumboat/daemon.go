package main

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/9elt/bumboat/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type daemon struct {
	api.UnimplementedBumboatServer
	dir string
	st  *store
	dk  *docker
	gh  *github
	px  *proxy

	builder sync.RWMutex
	mu      sync.Mutex
	live    map[string]*live
	pending map[string]*api.Deployment
	running map[string]bool
	stats   map[string][2]float64
	hostCPU float64
	wake    chan struct{}

	subsMu sync.Mutex
	subs   map[chan *api.ServiceList]bool
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func runDaemon() error {
	d := &daemon{
		dir:     env("BUMBOAT_DIR", "/var/lib/bumboat"),
		dk:      newDocker(),
		live:    map[string]*live{},
		pending: map[string]*api.Deployment{},
		running: map[string]bool{},
		stats:   map[string][2]float64{},
		wake:    make(chan struct{}, 1),
		subs:    map[chan *api.ServiceList]bool{},
	}
	for _, sub := range []string{"logs", "build", "bin", "certs"} {
		if err := os.MkdirAll(filepath.Join(d.dir, sub), 0o700); err != nil {
			return err
		}
	}
	os.RemoveAll(filepath.Join(d.dir, "build"))
	os.MkdirAll(filepath.Join(d.dir, "build"), 0o700)

	var err error
	if d.st, err = openStore(d.dir, env("BUMBOAT_KEY", "/etc/bumboat/master.key")); err != nil {
		return err
	}
	d.gh = &github{st: d.st, tokens: map[string]ghToken{}}
	d.px = newProxy(d)
	if err := d.st.failUnfinished(time.Now().Unix()); err != nil {
		return err
	}
	ctx := context.Background()
	if err := d.dk.ensureNetwork(ctx); err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	if err := d.ensureBuilder(); err != nil {
		log.Printf("builds from repos will fail until the docker buildx plugin works: %v", err)
	}
	if err := d.reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	d.routes()
	if err := d.px.listen(); err != nil {
		return err
	}
	go d.dispatch()
	go d.watchStats()

	sock := env("BUMBOAT_SOCKET", "/run/bumboat/bumboat.sock")
	os.MkdirAll(filepath.Dir(sock), 0o755)
	os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o660)
	if g, err := user.LookupGroup("bumboat"); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		os.Chown(sock, 0, gid)
	}
	srv := grpc.NewServer()
	api.RegisterBumboatServer(srv, d)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		srv.Stop()
	}()
	log.Printf("bumboat %s listening on %s, :80 and :443", version, sock)
	return srv.Serve(lis)
}

func (d *daemon) reconcile(ctx context.Context) error {
	containers, err := d.dk.containers(ctx)
	if err != nil {
		return err
	}
	services, liveIDs, err := d.st.services("")
	if err != nil {
		return err
	}
	byDeployment := map[string]string{}
	for _, sv := range services {
		byDeployment[strconv.FormatInt(liveIDs[sv.Name], 10)] = sv.Name
	}
	for _, c := range containers {
		dep := c.Labels["bumboat.deployment"]
		service, ok := byDeployment[dep]
		if !ok || dep == "0" {
			log.Printf("removing leftover container %s", strings.Join(c.Names, ","))
			d.dk.remove(ctx, c.Id)
			continue
		}
		if c.State != "running" && !d.st.stopped(service) {
			log.Printf("starting %s", service)
			if err := d.dk.start(ctx, c.Id); err != nil {
				log.Printf("start %s: %v", service, err)
			}
		}
		ip, _, err := d.dk.inspect(ctx, c.Id)
		if err != nil {
			return err
		}
		port, _ := strconv.Atoi(c.Labels["bumboat.port"])
		d.live[service] = &live{c.Id, ip, int32(port), liveIDs[service]}
	}
	return nil
}

func (d *daemon) builds() int {
	if n, _ := strconv.Atoi(d.st.setting("builds")); n > 0 {
		return n
	}
	cpus, memTotal, _ := hostResources()
	return max(1, min(cpus/2, int((memTotal>>30-1)/2)))
}

func hostResources() (cpus int, total, used int64) {
	cpus = runtime.NumCPU()
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		var quota, period int
		if n, _ := fmt.Sscan(string(b), &quota, &period); n == 2 && period > 0 {
			cpus = min(cpus, max(1, quota/period))
		}
	}
	b, _ := os.ReadFile("/proc/meminfo")
	var available int64
	for _, line := range strings.Split(string(b), "\n") {
		var key string
		var kb int64
		fmt.Sscan(line, &key, &kb)
		switch key {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			available = kb << 10
		}
	}
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		if limit, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			total = min(total, limit)
		}
	}
	return cpus, total, total - available
}

// cpuTimes reads the busy and total jiffies of all CPUs from /proc/stat.
func cpuTimes() (busy, total uint64) {
	b, _ := os.ReadFile("/proc/stat")
	line, _, _ := strings.Cut(string(b), "\n")
	fields := strings.Fields(line)
	for i, f := range fields[min(1, len(fields)):min(9, len(fields))] { // guest time is already in user and nice
		n, _ := strconv.ParseUint(f, 10, 64)
		total += n
		if i != 3 && i != 4 {
			busy += n
		}
	}
	return busy, total
}

func (d *daemon) host() *api.Host {
	cpus, total, used := hostResources()
	d.mu.Lock()
	defer d.mu.Unlock()
	return &api.Host{Cpus: int32(cpus), MemTotal: total, MemUsed: used, Cpu: d.hostCPU}
}

func (d *daemon) list() (*api.ServiceList, error) {
	services, _, err := d.st.services("")
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	for _, sv := range services {
		l := d.live[sv.Name]
		switch {
		case d.pending[sv.Name] != nil || d.running[sv.Name]:
			sv.State = "deploying"
		case d.st.stopped(sv.Name):
			sv.State = "stopped"
		case l != nil:
			sv.State = "running"
		case sv.Last != nil && sv.Last.Status == "failed":
			sv.State = "failed"
		default:
			sv.State = "new"
		}
		sv.Cpu, sv.Mem = d.stats[sv.Name][0], int64(d.stats[sv.Name][1])
	}
	d.mu.Unlock()
	return &api.ServiceList{Services: services, Host: d.host()}, nil
}

func (d *daemon) changed() {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	if len(d.subs) == 0 {
		return
	}
	list, err := d.list()
	if err != nil {
		log.Printf("list: %v", err)
		return
	}
	for ch := range d.subs {
		select {
		case <-ch:
		default:
		}
		ch <- list
	}
}

func (d *daemon) watchStats() {
	busy, total := cpuTimes()
	for range time.Tick(5 * time.Second) {
		b, t := cpuTimes()
		if t > total {
			d.mu.Lock()
			d.hostCPU = float64(b-busy) / float64(t-total) * 100
			d.mu.Unlock()
		}
		busy, total = b, t
		d.subsMu.Lock()
		watched := len(d.subs) > 0
		d.subsMu.Unlock()
		if !watched {
			continue
		}
		d.mu.Lock()
		lives := map[string]string{}
		for name, l := range d.live {
			lives[name] = l.id
		}
		d.mu.Unlock()
		var wg sync.WaitGroup
		for name, id := range lives {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cpu, mem, err := d.dk.stats(context.Background(), id)
				if err == nil {
					d.mu.Lock()
					d.stats[name] = [2]float64{cpu, float64(mem)}
					d.mu.Unlock()
				}
			}()
		}
		wg.Wait()
		d.changed()
	}
}

func fail(err error) error {
	if err == nil {
		return nil
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

func (d *daemon) ListServices(context.Context, *api.Empty) (*api.ServiceList, error) {
	list, err := d.list()
	return list, fail(err)
}

func (d *daemon) WatchServices(_ *api.Empty, stream api.Bumboat_WatchServicesServer) error {
	ch := make(chan *api.ServiceList, 1)
	list, err := d.list()
	if err != nil {
		return fail(err)
	}
	ch <- list
	d.subsMu.Lock()
	d.subs[ch] = true
	d.subsMu.Unlock()
	defer func() {
		d.subsMu.Lock()
		delete(d.subs, ch)
		d.subsMu.Unlock()
	}()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case list := <-ch:
			if err := stream.Send(list); err != nil {
				return err
			}
		}
	}
}

func (d *daemon) GetService(_ context.Context, ref *api.Ref) (*api.Service, error) {
	list, err := d.list()
	if err != nil {
		return nil, fail(err)
	}
	for _, sv := range list.Services {
		if sv.Name == ref.Name {
			for _, dom := range sv.Domains {
				dom.Cert = d.px.certState(dom.Host)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				dom.Resolves, _ = net.DefaultResolver.LookupHost(ctx, dom.Host)
				cancel()
			}
			return sv, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "no service %q", ref.Name)
}

func (d *daemon) CreateService(ctx context.Context, sv *api.Service) (*api.Service, error) {
	if err := d.st.saveService(sv, true, false); err != nil {
		return nil, fail(err)
	}
	d.routes()
	d.changed()
	return d.GetService(ctx, &api.Ref{Name: sv.Name})
}

func (d *daemon) UpdateService(ctx context.Context, sv *api.Service) (*api.Service, error) {
	old, err := d.st.service(sv.Name)
	if err != nil {
		return nil, fail(err)
	}
	strip := func(s *api.Service) *api.Service {
		c := proto.Clone(s).(*api.Service)
		c.Domains, c.AutoDeploy, c.State, c.Dirty, c.Live, c.Last, c.Cpu, c.Mem = nil, false, "", false, nil, nil, 0, 0
		return c
	}
	if err := d.st.saveService(sv, false, !proto.Equal(strip(old), strip(sv))); err != nil {
		return nil, fail(err)
	}
	d.routes()
	d.changed()
	return d.GetService(ctx, &api.Ref{Name: sv.Name})
}

func (d *daemon) DeleteService(ctx context.Context, ref *api.Ref) (*api.Empty, error) {
	sv, err := d.st.service(ref.Name)
	if err != nil {
		return nil, fail(err)
	}
	d.mu.Lock()
	busy := d.running[ref.Name]
	delete(d.pending, ref.Name)
	liveID := ""
	if l := d.live[ref.Name]; l != nil {
		liveID = l.id
	}
	d.mu.Unlock()
	if busy {
		return nil, fail(errors.New("a deployment is running: wait for it to finish"))
	}
	containers, err := d.dk.containers(ctx)
	if err != nil {
		return nil, fail(err)
	}
	for _, c := range containers {
		if c.Labels["bumboat.service"] == ref.Name || c.Id == liveID {
			d.dk.remove(ctx, c.Id)
		}
	}
	tags, _ := d.dk.images(ctx, "bumboat/"+ref.Name)
	for _, tag := range tags {
		d.dk.removeImage(ctx, tag)
	}
	for _, v := range sv.Volumes {
		if err := d.dk.removeVolume(ctx, v.Docker); err != nil {
			return nil, fail(fmt.Errorf("volume %s: %w", v.Docker, err))
		}
	}
	d.mu.Lock()
	delete(d.live, ref.Name)
	delete(d.stats, ref.Name)
	d.mu.Unlock()
	if err := d.st.deleteService(ref.Name); err != nil {
		return nil, fail(err)
	}
	d.routes()
	d.changed()
	return &api.Empty{}, nil
}

func (d *daemon) Backup(req *api.BackupRequest, stream api.Bumboat_BackupServer) error {
	w := bufio.NewWriterSize(chunkWriter{stream}, 64<<10)
	var volumes func(*tar.Writer) error
	if !req.SkipVolumes {
		volumes = func(tw *tar.Writer) error {
			return d.backupVolumes(stream.Context(), tw, func(format string, args ...any) {
				stream.Send(&api.BackupChunk{Warning: fmt.Sprintf(format, args...)})
			})
		}
	}
	if err := writeBackup(w, d.dir, env("BUMBOAT_KEY", "/etc/bumboat/master.key"), volumes); err != nil {
		return fail(err)
	}
	return w.Flush()
}

// backupVolumes adds every volume as volumes/<docker name>/..., reading it from the
// service's container, paused meanwhile so the copy is consistent.
func (d *daemon) backupVolumes(ctx context.Context, tw *tar.Writer, warn func(string, ...any)) error {
	services, _, err := d.st.services("")
	if err != nil {
		return err
	}
	for _, sv := range services {
		if len(sv.Volumes) == 0 {
			continue
		}
		d.mu.Lock()
		l := d.live[sv.Name]
		d.mu.Unlock()
		if l == nil {
			warn("%s has no container, its volumes are not in the backup", sv.Name)
			continue
		}
		if err := d.backupContainerVolumes(ctx, tw, l.id, sv.Volumes); err != nil {
			return fmt.Errorf("%s: %w", sv.Name, err)
		}
	}
	return nil
}

func (d *daemon) backupContainerVolumes(ctx context.Context, tw *tar.Writer, id string, volumes []*api.Volume) error {
	if _, running, _ := d.dk.inspect(ctx, id); running {
		if err := d.dk.pause(ctx, id); err != nil {
			return err
		}
		defer d.dk.unpause(context.Background(), id)
	}
	for _, v := range volumes {
		rc, err := d.dk.archive(ctx, id, v.Path)
		if err != nil {
			return err
		}
		err = copyVolume(tw, tar.NewReader(rc), "volumes/"+v.Docker)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// copyVolume moves the entries of a docker archive under prefix, dropping the archived
// directory's own name.
func copyVolume(tw *tar.Writer, tr *tar.Reader, prefix string) error {
	rename := func(name string) string {
		_, rest, _ := strings.Cut(name, "/")
		return path.Join(prefix, rest)
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		h.Name = rename(h.Name)
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		if h.Typeflag == tar.TypeLink {
			h.Linkname = rename(h.Linkname)
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

type chunkWriter struct{ stream api.Bumboat_BackupServer }

func (c chunkWriter) Write(p []byte) (int, error) {
	return len(p), c.stream.Send(&api.BackupChunk{Data: p})
}

func (d *daemon) RenameService(ctx context.Context, req *api.RenameRequest) (*api.Service, error) {
	name, to := req.Name, req.NewName
	d.mu.Lock()
	busy := d.running[name] || d.pending[name] != nil
	if !busy {
		d.running[name] = true
	}
	d.mu.Unlock()
	if busy {
		return nil, fail(errors.New("a deployment is queued or running: wait for it to finish"))
	}
	err := d.st.renameService(name, to)
	if err == nil {
		tags, _ := d.dk.images(ctx, "bumboat/"+name)
		for _, tag := range tags {
			if err := d.dk.tag(ctx, tag, "bumboat/"+to+strings.TrimPrefix(tag, "bumboat/"+name)); err == nil {
				d.dk.removeImage(ctx, tag)
			}
		}
	}
	var sv *api.Service
	if err == nil {
		sv, err = d.st.service(to)
	}
	d.mu.Lock()
	delete(d.running, name)
	if err == nil {
		if l := d.live[name]; l != nil {
			d.live[to] = l
		}
		d.stats[to] = d.stats[name]
		delete(d.live, name)
		delete(d.stats, name)
	}
	d.mu.Unlock()
	if err != nil {
		return nil, fail(err)
	}
	// redeploy what is live so the container takes the new name, labels and alias
	if sv.Live != nil && sv.Live.Image != "" {
		if _, err := d.enqueue(to, &api.Deployment{Sha: sv.Live.Sha, Message: sv.Live.Message, Image: sv.Live.Image}); err != nil {
			return nil, fail(err)
		}
	}
	d.routes()
	d.changed()
	return d.GetService(ctx, &api.Ref{Name: to})
}

func (d *daemon) Deploy(_ context.Context, ref *api.Ref) (*api.Deployment, error) {
	dep, err := d.enqueue(ref.Name, &api.Deployment{})
	return dep, fail(err)
}

func (d *daemon) Rollback(_ context.Context, ref *api.Ref) (*api.Deployment, error) {
	dep, err := d.rollback(ref.Id)
	return dep, fail(err)
}

func (d *daemon) Restart(ctx context.Context, ref *api.Ref) (*api.Empty, error) {
	d.mu.Lock()
	l := d.live[ref.Name]
	d.mu.Unlock()
	if l == nil {
		return nil, fail(fmt.Errorf("%s has never been deployed", ref.Name))
	}
	if err := d.dk.restart(ctx, l.id); err != nil {
		return nil, fail(err)
	}
	ip, _, err := d.dk.inspect(ctx, l.id)
	if err != nil {
		return nil, fail(err)
	}
	d.mu.Lock()
	l.ip = ip
	d.mu.Unlock()
	d.st.setStopped(ref.Name, false)
	d.routes()
	d.changed()
	return &api.Empty{}, nil
}

func (d *daemon) Stop(ctx context.Context, ref *api.Ref) (*api.Empty, error) {
	d.mu.Lock()
	l := d.live[ref.Name]
	d.mu.Unlock()
	if l == nil {
		return nil, fail(fmt.Errorf("%s is not running", ref.Name))
	}
	if err := d.dk.stop(ctx, l.id); err != nil {
		return nil, fail(err)
	}
	d.st.setStopped(ref.Name, true)
	d.mu.Lock()
	delete(d.stats, ref.Name)
	d.mu.Unlock()
	d.routes()
	d.changed()
	return &api.Empty{}, nil
}

func (d *daemon) ListDeployments(_ context.Context, ref *api.Ref) (*api.DeploymentList, error) {
	list, err := d.st.deployments(ref.Name, 100)
	return &api.DeploymentList{Deployments: list}, fail(err)
}

func (d *daemon) StreamLogs(req *api.LogsRequest, stream api.Bumboat_StreamLogsServer) error {
	send := func(line string) error { return stream.Send(&api.LogLine{Text: line}) }
	if req.Build {
		id := req.Deployment
		if id == 0 {
			deps, err := d.st.deployments(req.Service, 1)
			if err != nil || len(deps) == 0 {
				return fail(fmt.Errorf("%s has no deployments yet", req.Service))
			}
			id = deps[0].Id
		}
		return fail(d.tailBuildLog(stream.Context(), id, req.Follow, send))
	}
	d.mu.Lock()
	l := d.live[req.Service]
	d.mu.Unlock()
	if l == nil {
		return fail(fmt.Errorf("%s has no running container", req.Service))
	}
	tail := int(req.Tail)
	if tail == 0 {
		tail = 500
	}
	return fail(d.dk.logs(stream.Context(), l.id, req.Follow, tail, send))
}

func (d *daemon) GetSettings(context.Context, *api.Empty) (*api.Settings, error) {
	return &api.Settings{
		AcmeEmail:   d.st.setting("acme_email"),
		Builds:      int32(d.builds()),
		WebhookHost: d.st.setting("webhook_host"),
		GithubApp:   d.st.setting("github_app"),
		Host:        d.host(),
	}, nil
}

func (d *daemon) UpdateSettings(ctx context.Context, s *api.Settings) (*api.Settings, error) {
	if s.WebhookHost != "" && !reHost.MatchString(s.WebhookHost) {
		return nil, fail(fmt.Errorf("%q is not a valid hostname", s.WebhookHost))
	}
	if s.Builds < 0 {
		return nil, fail(errors.New("builds must be 0 (automatic) or more"))
	}
	builds := ""
	if s.Builds > 0 {
		builds = strconv.Itoa(int(s.Builds))
	}
	for k, v := range map[string]string{"acme_email": s.AcmeEmail, "builds": builds, "webhook_host": s.WebhookHost} {
		if err := d.st.setSetting(k, v); err != nil {
			return nil, fail(err)
		}
	}
	d.px.issuer.Email = s.AcmeEmail
	d.routes()
	d.kick()
	return d.GetSettings(ctx, nil)
}

func (d *daemon) ConnectGitHub(_ context.Context, req *api.ConnectGitHubRequest) (*api.ConnectGitHubReply, error) {
	if !reHost.MatchString(req.WebhookHost) {
		return nil, fail(fmt.Errorf("%q is not a valid hostname", req.WebhookHost))
	}
	if err := d.st.setSetting("webhook_host", req.WebhookHost); err != nil {
		return nil, fail(err)
	}
	if err := d.st.setSetting("github_setup_org", req.Org); err != nil {
		return nil, fail(err)
	}
	tok, err := d.setupToken()
	if err != nil {
		return nil, fail(err)
	}
	d.routes()
	return &api.ConnectGitHubReply{Url: "https://" + req.WebhookHost + "/setup/github?token=" + tok}, nil
}

func (d *daemon) ListRepos(ctx context.Context, _ *api.Empty) (*api.RepoList, error) {
	repos, err := d.gh.repos(ctx)
	return &api.RepoList{Repos: repos}, fail(err)
}

func (d *daemon) ListBranches(ctx context.Context, ref *api.Ref) (*api.BranchList, error) {
	branches, err := d.gh.branches(ctx, ref.Name)
	return &api.BranchList{Branches: branches}, fail(err)
}

func (d *daemon) SuggestBuild(ctx context.Context, req *api.SuggestBuildRequest) (*api.SuggestBuildReply, error) {
	ok, err := d.gh.hasFile(ctx, req.Repo, req.Branch, "Dockerfile")
	if err != nil {
		return nil, fail(err)
	}
	if ok {
		return &api.SuggestBuildReply{Build: "dockerfile"}, nil
	}
	return &api.SuggestBuildReply{Build: "railpack"}, nil
}
