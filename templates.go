package main

import (
	"cmp"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/9elt/bumboat/api"
)

// imageTemplate is a ready-made image service: the wizard only asks for a password and a name.
type imageTemplate struct {
	name, image, command, volume string
	port                         int32
	env                          func(password string) []*api.EnvVar
}

var templates = []imageTemplate{
	{name: "postgres", image: "postgres:18", port: 5432, volume: "/var/lib/postgresql", env: func(pw string) []*api.EnvVar {
		return []*api.EnvVar{{Key: "POSTGRES_PASSWORD", Value: pw}}
	}},
	{name: "mariadb", image: "mariadb:11", port: 3306, volume: "/var/lib/mysql", env: func(pw string) []*api.EnvVar {
		return []*api.EnvVar{{Key: "MARIADB_ROOT_PASSWORD", Value: pw}}
	}},
	{name: "mongodb", image: "mongo:8", port: 27017, volume: "/data/db", env: func(pw string) []*api.EnvVar {
		return []*api.EnvVar{{Key: "MONGO_INITDB_ROOT_USERNAME", Value: "root"}, {Key: "MONGO_INITDB_ROOT_PASSWORD", Value: pw}}
	}},
	{name: "redis", image: "redis:8", port: 6379, volume: "/data", command: "redis-server --requirepass $REDIS_PASSWORD", env: func(pw string) []*api.EnvVar {
		return []*api.EnvVar{{Key: "REDIS_PASSWORD", Value: pw}}
	}},
	{name: "rustfs", image: "rustfs/rustfs:latest", port: 9000, volume: "/data", env: func(pw string) []*api.EnvVar {
		return []*api.EnvVar{{Key: "RUSTFS_ACCESS_KEY", Value: "admin"}, {Key: "RUSTFS_SECRET_KEY", Value: pw}}
	}},
}

func (t *imageTemplate) service(name, password string) *api.Service {
	return &api.Service{
		Name: name, Source: "image", Image: t.image, Port: t.port, Command: t.command, AutoDeploy: true,
		Env:     t.env(password),
		Volumes: []*api.Volume{{Name: "data", Path: t.volume}},
	}
}

func password() string {
	const chars = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 24)
	rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// connectionURL is the URL other services use to reach an image service on the bumboat network.
func connectionURL(sv *api.Service, reveal bool) string {
	env := map[string]string{}
	for _, e := range sv.Env {
		env[e.Key] = e.Value
	}
	secret := func(keys ...string) string {
		for _, k := range keys {
			if v := env[k]; v != "" && !reveal {
				return "****"
			} else if v != "" {
				return v
			}
		}
		return ""
	}
	addr := fmt.Sprintf("%s:%d", sv.Name, sv.Port)
	switch image := sv.Image; {
	case strings.Contains(image, "postgres"):
		user := cmp.Or(env["POSTGRES_USER"], "postgres")
		db := cmp.Or(env["POSTGRES_DB"], user)
		return fmt.Sprintf("postgres://%s:%s@%s/%s", user, secret("POSTGRES_PASSWORD"), addr, db)
	case strings.Contains(image, "mysql"), strings.Contains(image, "mariadb"):
		return fmt.Sprintf("mysql://root:%s@%s", secret("MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD"), addr)
	case strings.Contains(image, "mongo"):
		return fmt.Sprintf("mongodb://%s:%s@%s", env["MONGO_INITDB_ROOT_USERNAME"], secret("MONGO_INITDB_ROOT_PASSWORD"), addr)
	case strings.Contains(image, "redis"):
		if pw := secret("REDIS_PASSWORD"); pw != "" {
			return fmt.Sprintf("redis://:%s@%s", pw, addr)
		}
		return "redis://" + addr
	case strings.Contains(image, "rustfs"):
		return fmt.Sprintf("http://%s (access key %s, secret key %s)", addr, env["RUSTFS_ACCESS_KEY"], secret("RUSTFS_SECRET_KEY"))
	}
	return addr
}
