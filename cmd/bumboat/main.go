package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/9elt/bumboat/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var version = "dev"

const usage = `usage:
  bumboat                    open the TUI
  bumboat daemon             run the daemon (systemd does this)
  bumboat ls                 list services
  bumboat deploy <service>   deploy and stream the build log
  bumboat logs <service> [-f] [--build]
  bumboat backup [--no-volumes] [file|-]
                             write a backup, - for stdout
  bumboat restore <file|->   put a backup back as root, - for stdin
  bumboat upgrade            install the latest release
  bumboat version`

func main() {
	args := append(os.Args[1:], "")
	var err error
	if args[0] == "restore" && os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "bumboat: %s needs root, run it as root (ssh root@<server> bumboat %s)\n", args[0], args[0])
		os.Exit(1)
	}
	switch args[0] {
	case "":
		err = runTUI()
	case "daemon":
		err = runDaemon()
	case "ls":
		err = cliList()
	case "deploy":
		err = cliDeploy(args[1])
	case "logs":
		err = cliLogs(args[1:])
	case "backup":
		err = backup(args[1 : len(args)-1])
	case "restore":
		err = restore(args[1])
	case "upgrade":
		err = upgrade()
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bumboat:", errorText(err))
		os.Exit(1)
	}
}

func socketPath() string { return env("BUMBOAT_SOCKET", "/run/bumboat/bumboat.sock") }

func dial() api.BumboatClient {
	conn, _ := grpc.NewClient("unix://"+socketPath(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	return api.NewBumboatClient(conn)
}

func errorText(err error) string {
	s, ok := status.FromError(err)
	switch {
	case !ok:
		return err.Error()
	case s.Code() == codes.Unavailable && strings.Contains(s.Message(), "permission denied"):
		return "no access to " + socketPath() + ": add yourself to the bumboat group (sudo usermod -aG bumboat $USER) and log in again"
	case s.Code() == codes.Unavailable:
		return "cannot reach the daemon at " + socketPath() + ": is it running? (systemctl status bumboat)"
	}
	return s.Message()
}

func cliList() error {
	list, err := dial().ListServices(context.Background(), &api.Empty{})
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tCOMMIT\tDOMAINS")
	for _, sv := range list.Services {
		var domains []string
		for _, d := range sv.Domains {
			domains = append(domains, d.Host+d.Path)
		}
		commit := ""
		if sv.Live != nil {
			commit = strings.TrimSpace(fmt.Sprintf("%.7s %s", sv.Live.Sha, sv.Live.Message))
		}
		fmt.Fprintf(w, "%s\t%s\t%.50s\t%s\n", sv.Name, sv.State, commit, strings.Join(domains, " "))
	}
	return w.Flush()
}

func cliDeploy(service string) error {
	if service == "" {
		return errors.New("usage: bumboat deploy <service>")
	}
	c := dial()
	dep, err := c.Deploy(context.Background(), &api.Ref{Name: service})
	if err != nil {
		return err
	}
	fmt.Printf("deployment #%d queued\n", dep.Id)
	if err := printLogs(c, &api.LogsRequest{Service: service, Deployment: dep.Id, Build: true, Follow: true}, os.Stdout); err != nil {
		return err
	}
	deps, err := c.ListDeployments(context.Background(), &api.Ref{Name: service})
	if err != nil {
		return err
	}
	for _, d := range deps.Deployments {
		if d.Id == dep.Id && d.Status != "live" {
			return fmt.Errorf("deployment #%d %s", d.Id, d.Status)
		}
	}
	return nil
}

func cliLogs(args []string) error {
	req := &api.LogsRequest{}
	for _, a := range args {
		switch a {
		case "-f":
			req.Follow = true
		case "--build":
			req.Build = true
		case "":
		default:
			req.Service = a
		}
	}
	if req.Service == "" {
		return errors.New("usage: bumboat logs <service> [-f] [--build]")
	}
	return printLogs(dial(), req, os.Stdout)
}

func printLogs(c api.BumboatClient, req *api.LogsRequest, w io.Writer) error {
	stream, err := c.StreamLogs(context.Background(), req)
	if err != nil {
		return err
	}
	for {
		line, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(w, line.Text)
	}
}

func backup(args []string) error {
	req, file := &api.BackupRequest{}, ""
	for _, a := range args {
		if a == "--no-volumes" {
			req.SkipVolumes = true
		} else {
			file = a
		}
	}
	if file == "" {
		file = "bumboat-backup-" + time.Now().Format("2006-01-02") + ".tar.gz"
	}
	out := os.Stdout
	if file == "-" {
		if info, _ := out.Stat(); info != nil && info.Mode()&os.ModeCharDevice != 0 {
			return errors.New("backup - writes to stdout: redirect it, e.g. ssh <server> bumboat backup - > backup.tar.gz")
		}
	}
	stream, err := dial().Backup(context.Background(), req)
	if err != nil {
		return err
	}
	if file != "-" {
		if out, err = os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err != nil {
			return err
		}
	}
	for err == nil {
		var chunk *api.BackupChunk
		if chunk, err = stream.Recv(); err == nil && chunk.Warning != "" {
			fmt.Fprintln(os.Stderr, "bumboat: warning:", chunk.Warning)
		} else if err == nil {
			_, err = out.Write(chunk.Data)
		}
	}
	if err == io.EOF && file != "-" {
		err = out.Close()
	} else if err == io.EOF {
		err = nil
	}
	if err != nil && file != "-" {
		out.Close()
		os.Remove(file)
	}
	if err != nil || file == "-" {
		return err
	}
	fmt.Printf("wrote %s\nrestore it as root: bumboat restore %s\n", file, file)
	return nil
}

// restore puts a backup back: the database, the key, the certificates and the volumes.
// It stops the daemon while it writes and starts it again after.
func restore(file string) error {
	if file == "" {
		return errors.New("usage: bumboat restore <file|->")
	}
	f := os.Stdin
	if file != "-" {
		var err error
		if f, err = os.Open(file); err != nil {
			return err
		}
		defer f.Close()
	} else if info, _ := f.Stat(); info != nil && info.Mode()&os.ModeCharDevice != 0 {
		return errors.New("restore - reads stdin: redirect it, e.g. ssh root@<server> bumboat restore - < backup.tar.gz")
	}
	// reading the header first fails on a wrong file before the daemon goes down
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a bumboat backup: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, err = dial().ListServices(ctx, &api.Empty{})
	cancel()
	if err == nil {
		fmt.Println("stopping the daemon")
		if err := systemctl("stop"); err != nil {
			return fmt.Errorf("stop the daemon first: %w", err)
		}
		defer func() {
			fmt.Println("starting the daemon")
			if err := systemctl("start"); err != nil {
				fmt.Fprintln(os.Stderr, "bumboat: start the daemon:", err)
			}
		}()
	}
	dir := env("BUMBOAT_DIR", "/var/lib/bumboat")
	os.Remove(filepath.Join(dir, "bumboat.db-wal"))
	os.Remove(filepath.Join(dir, "bumboat.db-shm"))

	dk, ctx := newDocker(), context.Background()
	mounts := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		// entries are files under / or, for volumes, volumes/<docker name>/<path in the volume>
		place := func(name string) (string, error) {
			rest, ok := strings.CutPrefix(name, "volumes/")
			if !ok {
				return safeJoin("/", name)
			}
			volume, inside, _ := strings.Cut(rest, "/")
			if mounts[volume] == "" {
				m, err := emptyVolume(ctx, dk, volume)
				if err != nil {
					return "", fmt.Errorf("volume %s: %w", volume, err)
				}
				mounts[volume] = m
				fmt.Println("restoring volume", volume)
			}
			return safeJoin(mounts[volume], inside)
		}
		target, err := place(h.Name)
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeLink {
			if h.Linkname, err = place(h.Linkname); err != nil {
				return err
			}
		}
		if err := extract(tr, h, target); err != nil {
			return err
		}
	}
	fmt.Println("restored")
	return nil
}

func systemctl(action string) error {
	out, err := exec.Command("systemctl", action, "bumboat").CombinedOutput()
	if out = bytes.TrimSpace(out); err != nil && len(out) > 0 {
		return errors.New(string(out))
	}
	return err
}

// emptyVolume stops the containers using the volume and empties it.
func emptyVolume(ctx context.Context, dk *docker, name string) (string, error) {
	users, err := dk.containersUsing(ctx, name)
	if err != nil {
		return "", err
	}
	for _, c := range users {
		if err := dk.stop(ctx, c.Id); err != nil {
			return "", err
		}
	}
	mount, err := dk.volume(ctx, name)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(mount)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(mount, e.Name())); err != nil {
			return "", err
		}
	}
	return mount, nil
}

func safeJoin(root, name string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	if name != "" && !filepath.IsLocal(name) {
		return "", fmt.Errorf("unsafe path in backup: %s", name)
	}
	return filepath.Join(root, name), nil
}

// extract writes one tar entry at target, keeping its mode and owner.
func extract(tr *tar.Reader, h *tar.Header, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o700); err != nil {
			return err
		}
	case tar.TypeReg:
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	case tar.TypeSymlink:
		os.Remove(target)
		if err := os.Symlink(h.Linkname, target); err != nil {
			return err
		}
		return os.Lchown(target, h.Uid, h.Gid)
	case tar.TypeLink:
		os.Remove(target)
		return os.Link(h.Linkname, target)
	default:
		return nil
	}
	if err := os.Chown(target, h.Uid, h.Gid); err != nil {
		return err
	}
	if err := os.Chmod(target, h.FileInfo().Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
		return err
	}
	return os.Chtimes(target, h.ModTime, h.ModTime)
}

// writeBackup writes the database, the master key and the certificates as a .tar.gz,
// then whatever extra adds (the volumes).
func writeBackup(w io.Writer, dir, key string, extra func(*tar.Writer) error) error {
	snapshot := filepath.Join(dir, "backup.db")
	os.Remove(snapshot)
	defer os.Remove(snapshot)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "bumboat.db")+"?mode=ro")
	if err != nil {
		return err
	}
	_, err = db.Exec(`VACUUM INTO ?`, snapshot)
	db.Close()
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(src, name string) error {
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		h, _ := tar.FileInfoHeader(info, "")
		h.Name = strings.TrimPrefix(name, "/")
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	}
	if err := add(snapshot, filepath.Join(dir, "bumboat.db")); err != nil {
		return err
	}
	if err := add(key, key); err != nil {
		return err
	}
	err = filepath.WalkDir(filepath.Join(dir, "certs"), func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		return add(path, path)
	})
	if err != nil {
		return err
	}
	if extra != nil {
		if err := extra(tw); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// upgrade asks the daemon to install the latest release, so it works without root.
// As root it installs it directly when the daemon is down or too old to do it.
func upgrade() error {
	reply, err := dial().Upgrade(context.Background(), &api.Empty{})
	if c := status.Code(err); (c == codes.Unimplemented || c == codes.Unavailable) && os.Geteuid() == 0 {
		reply = &api.UpgradeReply{}
		reply.Version, reply.Installed, err = installRelease()
		if err == nil && reply.Installed {
			fmt.Printf("installed %s, restarting the daemon\n", reply.Version)
			return systemctl("restart")
		}
	} else if c == codes.Unimplemented {
		return errors.New("this daemon is too old to upgrade itself, run it as root once: ssh root@<server> bumboat upgrade")
	}
	if err != nil {
		return err
	}
	if !reply.Installed {
		fmt.Println("already on", reply.Version)
		return nil
	}
	fmt.Printf("installed %s, the daemon restarts in a moment\n", reply.Version)
	return nil
}

// installRelease replaces this binary with the latest release, checked against its checksum.
func installRelease() (tag string, installed bool, err error) {
	var release struct {
		TagName string `json:"tag_name"`
	}
	res, err := http.Get("https://api.github.com/repos/9elt/bumboat/releases/latest")
	if err != nil {
		return "", false, err
	}
	err = json.NewDecoder(res.Body).Decode(&release)
	res.Body.Close()
	if err != nil || release.TagName == "" {
		return "", false, fmt.Errorf("no release found (%s)", res.Status)
	}
	if release.TagName == version {
		return version, false, nil
	}
	base := "https://github.com/9elt/bumboat/releases/download/" + release.TagName + "/"
	asset := "bumboat-linux-" + runtime.GOARCH
	get := func(name string) ([]byte, error) {
		res, err := http.Get(base + name)
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
		return "", false, err
	}
	bin, err := get(asset)
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(bin)
	if !strings.Contains(string(sums), hex.EncodeToString(sum[:])+"  "+asset) {
		return "", false, fmt.Errorf("%s: checksum mismatch", asset)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(exe+".new", bin, 0o755); err != nil {
		return "", false, err
	}
	if err := os.Rename(exe+".new", exe); err != nil {
		return "", false, err
	}
	return release.TagName, true, nil
}
