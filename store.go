package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/9elt/bumboat/api"
	_ "modernc.org/sqlite"
)

type store struct {
	db  *sql.DB
	key cipher.AEAD
}

const schema = `
CREATE TABLE IF NOT EXISTS services (
	name TEXT PRIMARY KEY,
	source TEXT NOT NULL,
	repo TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL DEFAULT '',
	image TEXT NOT NULL DEFAULT '',
	build TEXT NOT NULL DEFAULT 'dockerfile',
	dockerfile TEXT NOT NULL DEFAULT 'Dockerfile',
	context TEXT NOT NULL DEFAULT '.',
	port INTEGER NOT NULL DEFAULT 0,
	health_path TEXT NOT NULL DEFAULT '',
	auto_deploy INTEGER NOT NULL DEFAULT 1,
	dirty INTEGER NOT NULL DEFAULT 0,
	live INTEGER NOT NULL DEFAULT 0,
	stopped INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS env (
	service TEXT NOT NULL REFERENCES services(name) ON DELETE CASCADE,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	build INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (service, key)
);
CREATE TABLE IF NOT EXISTS domains (
	host TEXT NOT NULL,
	path TEXT NOT NULL DEFAULT '',
	service TEXT NOT NULL REFERENCES services(name) ON DELETE CASCADE,
	PRIMARY KEY (host, path)
);
CREATE TABLE IF NOT EXISTS volumes (
	service TEXT NOT NULL REFERENCES services(name) ON DELETE CASCADE,
	name TEXT NOT NULL,
	path TEXT NOT NULL,
	docker TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (service, name)
);
CREATE TABLE IF NOT EXISTS deployments (
	id INTEGER PRIMARY KEY,
	service TEXT NOT NULL REFERENCES services(name) ON DELETE CASCADE,
	sha TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	image TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	started INTEGER NOT NULL,
	finished INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

var (
	reName   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	reEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reHost   = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	reRepo   = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
)

func openStore(dir, keyPath string) (*store, error) {
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		rand.Read(key)
		if err = os.MkdirAll(filepath.Dir(keyPath), 0o700); err == nil {
			err = os.WriteFile(keyPath, key, 0o600)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	aead, _ := cipher.NewGCM(block)

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "bumboat.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	// volumes keep their docker name when the service is renamed
	if _, err := db.Exec(`ALTER TABLE volumes ADD COLUMN docker TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if _, err := db.Exec(`UPDATE volumes SET docker = 'bumboat-' || service || '-' || name WHERE docker = ''`); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &store{db, aead}, nil
}

func (s *store) seal(plain string) string {
	nonce := make([]byte, s.key.NonceSize())
	rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(s.key.Seal(nonce, nonce, []byte(plain), nil))
}

func (s *store) open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < s.key.NonceSize() {
		return "", errors.New("corrupt secret")
	}
	plain, err := s.key.Open(nil, raw[:s.key.NonceSize()], raw[s.key.NonceSize():], nil)
	return string(plain), err
}

const serviceColumns = `name, source, repo, branch, image, build, dockerfile, context, port, health_path, auto_deploy, dirty, live`

func (s *store) services(name string) ([]*api.Service, map[string]int64, error) {
	rows, err := s.db.Query(`SELECT `+serviceColumns+` FROM services WHERE ? = '' OR name = ? ORDER BY name`, name, name)
	if err != nil {
		return nil, nil, err
	}
	var list []*api.Service
	live := map[string]int64{}
	byName := map[string]*api.Service{}
	for rows.Next() {
		var sv api.Service
		var id int64
		if err := rows.Scan(&sv.Name, &sv.Source, &sv.Repo, &sv.Branch, &sv.Image, &sv.Build, &sv.Dockerfile, &sv.Context, &sv.Port, &sv.HealthPath, &sv.AutoDeploy, &sv.Dirty, &id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		list = append(list, &sv)
		byName[sv.Name] = &sv
		live[sv.Name] = id
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT service, key, value, build FROM env WHERE ? = '' OR service = ? ORDER BY key`, name, name)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var svc, sealed string
		var e api.EnvVar
		rows.Scan(&svc, &e.Key, &sealed, &e.Build)
		if e.Value, err = s.open(sealed); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("env %s/%s: %w", svc, e.Key, err)
		}
		byName[svc].Env = append(byName[svc].Env, &e)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT service, host, path FROM domains WHERE ? = '' OR service = ? ORDER BY host, path`, name, name)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var svc string
		var d api.Domain
		rows.Scan(&svc, &d.Host, &d.Path)
		byName[svc].Domains = append(byName[svc].Domains, &d)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT service, name, path, docker FROM volumes WHERE ? = '' OR service = ? ORDER BY name`, name, name)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var svc string
		var v api.Volume
		rows.Scan(&svc, &v.Name, &v.Path, &v.Docker)
		byName[svc].Volumes = append(byName[svc].Volumes, &v)
	}
	rows.Close()

	for _, sv := range list {
		if live[sv.Name] != 0 {
			sv.Live, _ = s.deployment(live[sv.Name])
		}
		if last, _ := s.deployments(sv.Name, 1); len(last) > 0 {
			sv.Last = last[0]
		}
	}
	return list, live, nil
}

func (s *store) service(name string) (*api.Service, error) {
	list, _, err := s.services(name)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 || name == "" {
		return nil, fmt.Errorf("no service %q", name)
	}
	return list[0], nil
}

func validate(sv *api.Service) error {
	switch {
	case !reName.MatchString(sv.Name):
		return fmt.Errorf("name %q: use lowercase letters, digits and dashes", sv.Name)
	case sv.Source != "github" && sv.Source != "image":
		return fmt.Errorf("source must be github or image")
	case sv.Source == "github" && !reRepo.MatchString(sv.Repo):
		return fmt.Errorf("repo %q: use owner/name", sv.Repo)
	case sv.Source == "github" && sv.Branch == "":
		return fmt.Errorf("branch is required")
	case sv.Source == "image" && sv.Image == "":
		return fmt.Errorf("image is required")
	case sv.Build != "dockerfile" && sv.Build != "railpack":
		return fmt.Errorf("build must be dockerfile or railpack")
	case sv.Port < 0 || sv.Port > 65535:
		return fmt.Errorf("port %d out of range", sv.Port)
	case sv.HealthPath != "" && !strings.HasPrefix(sv.HealthPath, "/"):
		return fmt.Errorf("health path must start with /")
	case filepath.IsAbs(sv.Dockerfile) || strings.Contains(sv.Dockerfile, "..") || filepath.IsAbs(sv.Context) || strings.Contains(sv.Context, ".."):
		return fmt.Errorf("dockerfile and context must stay inside the repo")
	}
	for _, e := range sv.Env {
		if !reEnvKey.MatchString(e.Key) {
			return fmt.Errorf("env key %q is not a valid variable name", e.Key)
		}
	}
	for _, d := range sv.Domains {
		if !reHost.MatchString(d.Host) {
			return fmt.Errorf("domain %q is not a valid hostname", d.Host)
		}
		if d.Path != "" && (!strings.HasPrefix(d.Path, "/") || strings.HasSuffix(d.Path, "/")) {
			return fmt.Errorf("path %q must start with / and not end with /", d.Path)
		}
	}
	if len(sv.Domains) > 0 && sv.Port == 0 {
		return fmt.Errorf("a service with domains needs a port")
	}
	for _, v := range sv.Volumes {
		if !reName.MatchString(v.Name) || !filepath.IsAbs(v.Path) {
			return fmt.Errorf("volume %q: name must be lowercase, path absolute", v.Name)
		}
	}
	return nil
}

func (s *store) saveService(sv *api.Service, create, dirty bool) error {
	if sv.Build == "" {
		sv.Build = "dockerfile"
	}
	if sv.Dockerfile == "" {
		sv.Dockerfile = "Dockerfile"
	}
	if sv.Context == "" {
		sv.Context = "."
	}
	if err := validate(sv); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if create {
		_, err = tx.Exec(`INSERT INTO services (name, source, repo, branch, image, build, dockerfile, context, port, health_path, auto_deploy) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sv.Name, sv.Source, sv.Repo, sv.Branch, sv.Image, sv.Build, sv.Dockerfile, sv.Context, sv.Port, sv.HealthPath, sv.AutoDeploy)
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("service %q already exists", sv.Name)
		}
	} else {
		var res sql.Result
		res, err = tx.Exec(`UPDATE services SET source = ?, repo = ?, branch = ?, image = ?, build = ?, dockerfile = ?, context = ?, port = ?, health_path = ?, auto_deploy = ?, dirty = CASE WHEN ? THEN live != 0 ELSE dirty END WHERE name = ?`,
			sv.Source, sv.Repo, sv.Branch, sv.Image, sv.Build, sv.Dockerfile, sv.Context, sv.Port, sv.HealthPath, sv.AutoDeploy, dirty, sv.Name)
		if n, _ := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("no service %q", sv.Name)
		}
	}
	if err != nil {
		return err
	}

	docker := map[string]string{}
	rows, err := tx.Query(`SELECT name, docker FROM volumes WHERE service = ?`, sv.Name)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, d string
		rows.Scan(&name, &d)
		docker[name] = d
	}
	rows.Close()
	for _, q := range []string{`DELETE FROM env WHERE service = ?`, `DELETE FROM domains WHERE service = ?`, `DELETE FROM volumes WHERE service = ?`} {
		if _, err := tx.Exec(q, sv.Name); err != nil {
			return err
		}
	}
	for _, e := range sv.Env {
		if _, err := tx.Exec(`INSERT INTO env (service, key, value, build) VALUES (?, ?, ?, ?)`, sv.Name, e.Key, s.seal(e.Value), e.Build); err != nil {
			return fmt.Errorf("env %s: %w", e.Key, err)
		}
	}
	for _, d := range sv.Domains {
		if _, err := tx.Exec(`INSERT INTO domains (host, path, service) VALUES (?, ?, ?)`, d.Host, d.Path, sv.Name); err != nil {
			return fmt.Errorf("domain %s%s is already used", d.Host, d.Path)
		}
	}
	for _, v := range sv.Volumes {
		v.Docker = docker[v.Name]
		for n := 1; v.Docker == ""; n++ {
			v.Docker = "bumboat-" + sv.Name + "-" + v.Name
			if n > 1 {
				v.Docker += "-" + strconv.Itoa(n)
			}
			var used bool
			tx.QueryRow(`SELECT 1 FROM volumes WHERE docker = ?`, v.Docker).Scan(&used)
			if used {
				v.Docker = ""
			}
		}
		if _, err := tx.Exec(`INSERT INTO volumes (service, name, path, docker) VALUES (?, ?, ?, ?)`, sv.Name, v.Name, v.Path, v.Docker); err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
	}
	return tx.Commit()
}

// renameService moves a service and everything it owns to a new name.
// Its volumes keep their docker names, its built images are retagged by the caller.
func (s *store) renameService(name, to string) error {
	if !reName.MatchString(to) {
		return fmt.Errorf("name %q must be lowercase letters, digits and dashes", to)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE services SET name = ? WHERE name = ?`, to, name)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("service %q already exists", to)
	} else if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no service %q", name)
	}
	for _, table := range []string{"env", "domains", "volumes", "deployments"} {
		if _, err := tx.Exec(`UPDATE `+table+` SET service = ? WHERE service = ?`, to, name); err != nil {
			return err
		}
	}
	prefix := "bumboat/" + name + ":"
	if _, err := tx.Exec(`UPDATE deployments SET image = ? || substr(image, ?) WHERE service = ? AND substr(image, 1, ?) = ?`,
		"bumboat/"+to+":", len(prefix)+1, to, len(prefix), prefix); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) deleteService(name string) error {
	_, err := s.db.Exec(`DELETE FROM services WHERE name = ?`, name)
	return err
}

func (s *store) setLive(service string, id int64) error {
	_, err := s.db.Exec(`UPDATE services SET live = ?, dirty = 0, stopped = 0 WHERE name = ?`, id, service)
	return err
}

func (s *store) setStopped(service string, stopped bool) error {
	_, err := s.db.Exec(`UPDATE services SET stopped = ? WHERE name = ?`, stopped, service)
	return err
}

func (s *store) stopped(service string) bool {
	var stopped bool
	s.db.QueryRow(`SELECT stopped FROM services WHERE name = ?`, service).Scan(&stopped)
	return stopped
}

const deploymentColumns = `id, service, sha, message, image, status, error, started, finished`

func scanDeployment(row interface{ Scan(...any) error }) (*api.Deployment, error) {
	var d api.Deployment
	err := row.Scan(&d.Id, &d.Service, &d.Sha, &d.Message, &d.Image, &d.Status, &d.Error, &d.Started, &d.Finished)
	return &d, err
}

func (s *store) addDeployment(d *api.Deployment) error {
	res, err := s.db.Exec(`INSERT INTO deployments (service, sha, message, image, status, started) VALUES (?, ?, ?, ?, ?, ?)`,
		d.Service, d.Sha, d.Message, d.Image, d.Status, d.Started)
	if err == nil {
		d.Id, err = res.LastInsertId()
	}
	return err
}

func (s *store) saveDeployment(d *api.Deployment) error {
	_, err := s.db.Exec(`UPDATE deployments SET sha = ?, message = ?, image = ?, status = ?, error = ?, finished = ? WHERE id = ?`,
		d.Sha, d.Message, d.Image, d.Status, d.Error, d.Finished, d.Id)
	return err
}

func (s *store) deployment(id int64) (*api.Deployment, error) {
	return scanDeployment(s.db.QueryRow(`SELECT `+deploymentColumns+` FROM deployments WHERE id = ?`, id))
}

func (s *store) deployments(service string, limit int) ([]*api.Deployment, error) {
	rows, err := s.db.Query(`SELECT `+deploymentColumns+` FROM deployments WHERE service = ? ORDER BY id DESC LIMIT ?`, service, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*api.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, nil
}

func (s *store) failUnfinished(now int64) error {
	_, err := s.db.Exec(`UPDATE deployments SET status = 'failed', error = 'interrupted by a daemon restart', finished = ? WHERE status IN ('queued', 'building', 'starting')`, now)
	return err
}

func (s *store) setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if strings.HasPrefix(key, "secret_") && v != "" {
		v, _ = s.open(v)
	}
	return v
}

func (s *store) setSetting(key, value string) error {
	if strings.HasPrefix(key, "secret_") {
		value = s.seal(value)
	}
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
