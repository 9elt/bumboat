package main

import (
	"archive/tar"
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
  bumboat backup [file]      write a backup tarball (restore: tar -xzf file -C /)
  bumboat upgrade            install the latest release
  bumboat version`

func main() {
	args := append(os.Args[1:], "")
	var err error
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
		err = backup(args[1])
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
			commit = fmt.Sprintf("%.7s %s", sv.Live.Sha, sv.Live.Message)
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

func backup(file string) error {
	dir, key := env("BUMBOAT_DIR", "/var/lib/bumboat"), env("BUMBOAT_KEY", "/etc/bumboat/master.key")
	if file == "" {
		file = "bumboat-backup-" + time.Now().Format("2006-01-02") + ".tar.gz"
	}
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

	out, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
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
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	fmt.Printf("wrote %s\nrestore with the daemon stopped: sudo tar -xzf %s -C /\n", file, file)
	return nil
}

func upgrade() error {
	var release struct {
		TagName string `json:"tag_name"`
	}
	res, err := http.Get("https://api.github.com/repos/9elt/bumboat/releases/latest")
	if err != nil {
		return err
	}
	err = json.NewDecoder(res.Body).Decode(&release)
	res.Body.Close()
	if err != nil || release.TagName == "" {
		return fmt.Errorf("no release found (%s)", res.Status)
	}
	if release.TagName == version {
		fmt.Println("already on", version)
		return nil
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
		return err
	}
	bin, err := get(asset)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	if !strings.Contains(string(sums), hex.EncodeToString(sum[:])+"  "+asset) {
		return fmt.Errorf("%s: checksum mismatch", asset)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.WriteFile(exe+".new", bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(exe+".new", exe); err != nil {
		return err
	}
	fmt.Printf("installed %s, restarting the daemon\n", release.TagName)
	cmd := exec.Command("systemctl", "restart", "bumboat")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
