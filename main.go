// bootwatcherd watches boot.watch: every minute it checks bootwatchd's endpoints the way a Mac
// does, through boot.watch's public address, keeps 60 days of results in its own files, pushes
// an alert through Pushover when an endpoint stays down, and serves the public status page
// (status.boot.watch). It logs state changes only, never a line per check, and nothing about
// the page's visitors.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is the build, set by make: `git describe --tags --always --dirty` (e.g. v0.1.0).
var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:8471", "where the page listens")
	data := flag.String("data", dataDir(), "the directory for the checks and state (created if missing)")
	target := flag.String("target", "https://boot.watch", "the bootwatchd to watch")
	control := flag.String("control", "https://www.cloudflare.com/cdn-cgi/trace", "a URL that answers when the checker's own network works")
	push := flag.String("pushover", inDir("CREDENTIALS_DIRECTORY", "pushover.env"), "PUSHOVER_TOKEN= and PUSHOVER_USER= lines; without the file, no pushes")
	page := flag.String("page", "https://status.boot.watch", "the status page's address, linked from pushes")
	test := flag.Bool("test-push", false, "send one push to show pushes arrive, then exit")
	flag.Parse()

	if *test {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := testPush(ctx, *push, *page); err != nil {
			log.Fatalf("bootwatcherd: test push: %v", err)
		}
		fmt.Println("bootwatcherd: test push delivered")
		return
	}

	st, err := openStore(*data, time.Now())
	if err != nil {
		log.Fatalf("bootwatcherd: %s: %v", *data, err)
	}
	defer st.Close()
	var po *pushover
	if p, err := loadPushover(*push, *page); err == nil {
		po = p
	} else {
		log.Printf("bootwatcherd: no pushes: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{
		Addr:              *listen,
		Handler:           routes(st, *page),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0), // its lines name visitors' addresses
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	watching := make(chan struct{})
	go func() {
		watch(ctx, st, newProber(*target, *control), po)
		close(watching)
	}()
	log.Printf("bootwatcherd %s: watching %s, page on %s, data in %s", version, *target, *listen, *data)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("bootwatcherd: %v", err)
	}
	<-watching // the check under way ends before the store closes
}

// dataDir is the directory systemd gives the service ($STATE_DIRECTORY), or bootwatcherd-data.
func dataDir() string {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		return d
	}
	return "bootwatcherd-data"
}

// inDir is name in the directory systemd names in env, or name alone.
func inDir(env, name string) string {
	if d := os.Getenv(env); d != "" {
		return filepath.Join(d, name)
	}
	return name
}

// watch checks at second 30 of every minute until ctx ends.
func watch(ctx context.Context, st *store, p *prober, po *pushover) {
	w := &watcher{st: st, p: p, po: po}
	for {
		now := time.Now()
		next := now.Truncate(time.Minute).Add(30 * time.Second)
		if !next.After(now) {
			next = next.Add(time.Minute)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		w.once(ctx, next.UTC())
	}
}

type watcher struct {
	st        *store
	p         *prober
	po        *pushover
	noData    bool // the control check is failing (logged once)
	lastPrune time.Time
}

// once runs one check, saves it and moves the alerts on.
func (w *watcher) once(ctx context.Context, at time.Time) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	c := w.p.check(cctx, at)
	cancel()
	if err := w.st.save(c); err != nil {
		log.Printf("bootwatcherd: saving the check: %v", err)
	}
	nd := c.Results[endpoints[0].Path].State == NoData
	if nd != w.noData {
		if nd {
			log.Printf("bootwatcherd: no data: %s", c.Results[endpoints[0].Path].Err)
		} else {
			log.Printf("bootwatcherd: the control answers again")
		}
		w.noData = nd
	}
	w.alerts(ctx, c, at)
	if at.Sub(w.lastPrune) >= time.Hour {
		if err := w.st.prune(at); err != nil {
			log.Printf("bootwatcherd: pruning: %v", err)
		}
		w.lastPrune = at
	}
}

// alerts moves every endpoint's alert state, and the checker's own, on by this check and sends
// one push per state change: what went down together in one push, what came back together in
// another. A down push marks its endpoints pushed only once delivered (else it's retried next
// check).
func (w *watcher) alerts(ctx context.Context, c Check, at time.Time) {
	states, blind := w.st.alertStates()
	var evs []event
	for _, e := range endpoints {
		r := c.Results[e.Path]
		s := states[e.Path]
		n, ev := step(s, e.Path, r, at)
		if s.Fails == 0 && n.Fails == 1 {
			log.Printf("bootwatcherd: %s failing: %s", e.Path, r.Err)
		}
		if s.Fails > 0 && n.Fails == 0 {
			log.Printf("bootwatcherd: %s answering again", e.Path)
		}
		if ev != nil {
			evs = append(evs, *ev)
		}
		states[e.Path] = n
	}
	for _, m := range combine(evs) {
		if w.deliver(ctx, m) && m.Down {
			for _, p := range m.Endpoints {
				s := states[p]
				s.Pushed = true
				states[p] = s
			}
		}
	}
	blind, m := blindStep(blind, c.Results[endpoints[0].Path], at)
	if m != nil && w.deliver(ctx, *m) && m.Down {
		blind.Pushed = true
	}
	if err := w.st.setAlertStates(states, blind); err != nil {
		log.Printf("bootwatcherd: alert state: %v", err)
	}
	if b, m := bugStep(w.st.bugAlert(), c.Health, at); m == nil || w.deliver(ctx, *m) {
		if err := w.st.setBugAlert(b); err != nil {
			log.Printf("bootwatcherd: bug alert state: %v", err)
		}
	}
}

// deliver sends m, telling whether it went out.
func (w *watcher) deliver(ctx context.Context, m message) bool {
	if w.po == nil {
		log.Printf("bootwatcherd: would push (no Pushover): %s: %s", m.Title, m.Body)
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := w.po.send(pctx, m); err != nil {
		log.Printf("bootwatcherd: push failed (retried next check): %v", err)
		return false
	}
	log.Printf("bootwatcherd: pushed: %s", m.Title)
	return true
}

// csp: no scripts, nothing from anywhere else, and only the page's own stylesheet (by its hash).
var csp = func() string {
	b, _ := assetFS.ReadFile("assets/page.html")
	_, css, _ := strings.Cut(string(b), "<style>")
	css, _, _ = strings.Cut(css, "</style>")
	sum := sha256.Sum256([]byte(css))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; font-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

// pages holds the page and /status.json as rendered: again after a check lands, and every
// 30 s for the headline's staleness and the bars' dates. Every request in between gets the same
// bytes, so visitors cost the checker next to nothing.
type pages struct {
	st   *store
	mu   sync.Mutex
	gen  uint64
	at   time.Time
	html []byte
	json []byte
}

func (p *pages) get(now time.Time) (html, json []byte, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if g := p.st.generation(); p.html == nil || g != p.gen || now.Sub(p.at) >= 30*time.Second {
		v := p.st.view(now)
		var b bytes.Buffer
		if err := renderPage(&b, v); err != nil {
			return nil, nil, err
		}
		p.html, p.json, p.gen, p.at = b.Bytes(), statusJSON(v), g, now
	}
	return p.html, p.json, nil
}

// secure sends every answer with its security headers, and a plain-HTTP visit (Cloudflare says
// so in X-Forwarded-Proto; only the tunnel reaches the page) to the HTTPS page.
func secure(h http.Handler, page string) http.Handler {
	base := strings.TrimRight(page, "/")
	tls := strings.HasPrefix(base, "https://")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		if tls {
			if r.Header.Get("X-Forwarded-Proto") == "http" {
				code := http.StatusMovedPermanently
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					code = http.StatusPermanentRedirect
				}
				http.Redirect(w, r, base+r.URL.RequestURI(), code)
				return
			}
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.ServeHTTP(w, r)
	})
}

func routes(st *store, page string) http.Handler {
	pg := &pages{st: st}
	mux := http.NewServeMux()
	a := assets()
	mux.Handle("GET /fonts/", a)
	mux.Handle("GET /favicon.svg", a)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	// /fresh is for a monitor elsewhere: 200 while the checks keep coming, 503 when they stop
	// (bootwatcherd stuck, its server down, the tunnel down: anything but a 200 should page).
	mux.HandleFunc("GET /fresh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		last, now := st.lastAt(), time.Now()
		switch {
		case last.IsZero():
			http.Error(w, "no checks yet", http.StatusServiceUnavailable)
		case now.Sub(last) > staleAfter:
			http.Error(w, "last check "+ago(now.Sub(last))+" ago", http.StatusServiceUnavailable)
		default:
			io.WriteString(w, "ok\n")
		}
	})
	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		_, js, err := pg.get(time.Now().UTC())
		if err != nil {
			http.Error(w, "the status isn't available just now", http.StatusInternalServerError)
			log.Printf("bootwatcherd: page: %v", err)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-cache")
		h.Set("Access-Control-Allow-Origin", "*")
		w.Write(js)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		html, _, err := pg.get(time.Now().UTC())
		if err != nil {
			http.Error(w, "the status isn't available just now", http.StatusInternalServerError)
			log.Printf("bootwatcherd: page: %v", err)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", csp)
		w.Write(html)
	})
	return secure(mux, page)
}
