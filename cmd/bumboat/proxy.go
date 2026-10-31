package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
)

type route struct {
	path    string
	service string
	proxy   *httputil.ReverseProxy
}

type proxy struct {
	d       *daemon
	table   atomic.Pointer[map[string][]route]
	cache   *certmagic.Cache
	magic   *certmagic.Config
	issuer  *certmagic.ACMEIssuer
	mu      sync.Mutex
	managed map[string]bool
	certs   sync.Map
}

func newProxy(d *daemon) *proxy {
	p := &proxy{d: d, managed: map[string]bool{}}
	p.cache = certmagic.NewCache(certmagic.CacheOptions{GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return p.magic, nil }})
	p.magic = certmagic.New(p.cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: filepath.Join(d.dir, "certs")},
		OnEvent: func(_ context.Context, event string, data map[string]any) error {
			host, _ := data["identifier"].(string)
			switch event {
			case "cert_obtaining":
				p.certs.Store(host, "issuing")
			case "cert_failed":
				p.certs.Store(host, fmt.Sprintf("failed: %v", data["error"]))
			case "cert_obtained":
				p.certs.Delete(host)
			}
			d.changed()
			return nil
		},
	})
	p.issuer = certmagic.NewACMEIssuer(p.magic, certmagic.ACMEIssuer{
		CA:     certmagic.LetsEncryptProductionCA,
		Email:  d.st.setting("acme_email"),
		Agreed: true,
	})
	p.magic.Issuers = []certmagic.Issuer{p.issuer}
	p.table.Store(&map[string][]route{})
	return p
}

func (p *proxy) listen() error {
	plain, err := net.Listen("tcp", ":80")
	if err != nil {
		return err
	}
	tlsConfig := p.magic.TLSConfig()
	tlsConfig.NextProtos = append([]string{"h2", "http/1.1"}, tlsConfig.NextProtos...)
	secure, err := tls.Listen("tcp", ":443", tlsConfig)
	if err != nil {
		return err
	}
	go (&http.Server{Handler: http.HandlerFunc(p.serveHTTP), ReadHeaderTimeout: 10 * time.Second}).Serve(plain)
	go (&http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 10 * time.Second}).Serve(secure)
	return nil
}

func (p *proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if p.issuer.HandleHTTPChallenge(w, r) {
		return
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if certmagic.SubjectQualifiesForPublicCert(host) {
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
		return
	}
	p.serve(w, r)
}

func (p *proxy) serve(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	host = strings.ToLower(host)
	if host == p.d.st.setting("webhook_host") && (strings.HasPrefix(r.URL.Path, "/setup/github") || r.URL.Path == "/webhooks/github") {
		p.d.githubHTTP(w, r)
		return
	}
	for _, rt := range (*p.table.Load())[host] {
		if rt.path == "" || r.URL.Path == rt.path || strings.HasPrefix(r.URL.Path, rt.path+"/") {
			if rt.proxy == nil {
				http.Error(w, rt.service+" is not running", http.StatusServiceUnavailable)
				return
			}
			rt.proxy.ServeHTTP(w, r)
			return
		}
	}
	http.Error(w, "no service for "+host, http.StatusNotFound)
}

func (d *daemon) routes() {
	list, _, err := d.st.services("")
	if err != nil {
		log.Printf("routes: %v", err)
		return
	}
	table := map[string][]route{}
	var hosts []string
	d.mu.Lock()
	for _, sv := range list {
		l := d.live[sv.Name]
		for _, dom := range sv.Domains {
			rt := route{path: dom.Path, service: sv.Name}
			if l != nil && !d.st.stopped(sv.Name) {
				addr := net.JoinHostPort(l.ip, strconv.Itoa(int(l.port)))
				rt.proxy = &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
					pr.SetURL(&url.URL{Scheme: "http", Host: addr})
					pr.Out.Host = pr.In.Host
					pr.SetXForwarded()
				}}
			}
			table[dom.Host] = append(table[dom.Host], rt)
			hosts = append(hosts, dom.Host)
		}
	}
	d.mu.Unlock()
	for _, rts := range table {
		slices.SortFunc(rts, func(a, b route) int { return len(b.path) - len(a.path) })
	}
	d.px.table.Store(&table)

	if h := d.st.setting("webhook_host"); h != "" {
		hosts = append(hosts, h)
	}
	var fresh []string
	d.px.mu.Lock()
	for _, h := range hosts {
		if !d.px.managed[h] && certmagic.SubjectQualifiesForPublicCert(h) {
			d.px.managed[h] = true
			fresh = append(fresh, h)
		}
	}
	d.px.mu.Unlock()
	if len(fresh) > 0 {
		if err := d.px.magic.ManageAsync(context.Background(), fresh); err != nil {
			log.Printf("certificates: %v", err)
		}
	}
}

func (p *proxy) certState(host string) string {
	if !certmagic.SubjectQualifiesForPublicCert(host) {
		return "plain http (local name)"
	}
	for _, c := range p.cache.AllMatchingCertificates(host) {
		if c.Leaf != nil {
			return "valid until " + c.Leaf.NotAfter.Format("2006-01-02")
		}
	}
	if s, ok := p.certs.Load(host); ok {
		return s.(string)
	}
	return "pending DNS"
}
