package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The stream must be bootwatchd's byte for byte: the test vectors in its README.md.
func TestStreamVectors(t *testing.T) {
	for _, c := range []struct {
		seed seed
		want string
	}{
		{seed{}, "66e94bd4ef8a2c3b884cfa59ca342b2e58e2fccefa7e3061367f1d57a4e7455a"},
		{seed{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, "c6a13b37878f5b826f4f8162a1c8d8797346139595c0b41e497bbde365f42d0a"},
	} {
		got := make([]byte, 32)
		stream(c.seed).Read(got)
		if h := hex.EncodeToString(got); h != c.want {
			t.Errorf("seed %s: %s", c.seed, h)
		}
	}
}

// watchd answers as bootwatchd does; mod may break one endpoint.
func watchd(mod func(w http.ResponseWriter, r *http.Request) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mod != nil && mod(w, r) {
			return
		}
		q := r.URL.Query()
		sd := func() seed {
			var s seed
			hex.Decode(s[:], []byte(q.Get("seed")))
			return s
		}
		switch r.URL.Path {
		case "/v1/echo":
			io.WriteString(w, q.Get("n"))
		case "/v1/stream":
			n, _ := strconv.ParseInt(q.Get("n"), 10, 64)
			io.CopyN(w, stream(sd()), n)
		case "/v1/sink":
			n, ok, _ := match(r.Body, sd())
			json.NewEncoder(w).Encode(map[string]int64{"n": n, "match": ok})
		case "/v1/health":
			io.WriteString(w, `{"day_bytes":1441792,"daily_bytes":4294967296}`)
		case "/cdn-cgi/trace":
			io.WriteString(w, "colo=YYZ\n")
		}
	}
}

func TestProbe(t *testing.T) {
	cases := []struct {
		name string
		mod  func(w http.ResponseWriter, r *http.Request) bool
		want map[string]State
		err  string
	}{
		{"all good", nil, map[string]State{"/v1/echo": Good, "/v1/stream": Good, "/v1/sink": Good}, ""},
		{"a wrong byte down", func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/v1/stream" {
				return false
			}
			b := make([]byte, probeSize)
			var s seed
			hex.Decode(s[:], []byte(r.URL.Query().Get("seed")))
			stream(s).Read(b)
			b[4096] ^= 1
			w.Write(b)
			return true
		}, map[string]State{"/v1/echo": Good, "/v1/stream": Failed, "/v1/sink": Good}, "byte 4096 was wrong"},
		{"the sink lies", func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/v1/sink" {
				return false
			}
			io.WriteString(w, `{"n":16384,"match":10}`)
			return true
		}, map[string]State{"/v1/echo": Good, "/v1/stream": Good, "/v1/sink": Failed}, "got 16384 of 16384 bytes, 10 of them right"},
		{"bootwatchd down behind Cloudflare", func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasPrefix(r.URL.Path, "/v1/") {
				http.Error(w, "origin down", http.StatusBadGateway)
				return true
			}
			return false
		}, map[string]State{"/v1/echo": Failed, "/v1/stream": Failed, "/v1/sink": Failed}, "HTTP 502"},
		{"the checker's own network", func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/cdn-cgi/trace" {
				http.Error(w, "no", http.StatusServiceUnavailable)
				return true
			}
			return false
		}, map[string]State{"/v1/echo": NoData, "/v1/stream": NoData, "/v1/sink": NoData}, "control: HTTP 503"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(watchd(c.mod))
		p := newProber(srv.URL, srv.URL+"/cdn-cgi/trace")
		got := p.check(context.Background(), time.Now())
		srv.Close()
		var errs []string
		for path, st := range c.want {
			r := got.Results[path]
			if r.State != st {
				t.Errorf("%s: %s %v, want %v (%s)", c.name, path, r.State, st, r.Err)
			}
			errs = append(errs, r.Err)
		}
		if c.err != "" && !strings.Contains(strings.Join(errs, "|"), c.err) {
			t.Errorf("%s: errors %q, want %q", c.name, errs, c.err)
		}
		if c.mod == nil && (got.Health == nil || got.Health.DailyBytes != 4<<30) {
			t.Errorf("%s: health %+v", c.name, got.Health)
		}
	}
}

// Every check opens its own connection and speaks HTTP/1.1, as a Mac's test does: a connection
// kept between checks went stale behind Cloudflare and failed the next check's first request.
func TestProbeConnectionPerCheck(t *testing.T) {
	var conns atomic.Int32
	var proto atomic.Value
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto.Store(r.Proto)
		watchd(nil)(w, r)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	// the test server's certificate, as the system's roots vouch for boot.watch's
	defer func(old func() (*http.Client, *http.Transport)) { session = old }(session)
	session = func() (*http.Client, *http.Transport) {
		hc, tr := newSession()
		tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
		return hc, tr
	}
	p := newProber(srv.URL, srv.URL+"/cdn-cgi/trace")
	for i := range 2 {
		c := p.check(context.Background(), time.Now())
		if r := c.Results["/v1/sink"]; r.State != Good || r.Family != "4" {
			t.Fatalf("check %d: %+v", i, r)
		}
		if n := conns.Load(); n != int32(i+1) {
			t.Fatalf("after check %d: %d connections, want %d", i+1, n, i+1)
		}
	}
	if p := proto.Load(); p != "HTTP/1.1" {
		t.Fatalf("protocol %v, want HTTP/1.1", p)
	}
}

func TestClip(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"HTTP 502", "HTTP 502"},
		{"a\tb\nc\rd", "a b c d"},
		{strings.Repeat("x", 30), strings.Repeat("x", 17) + "…"},
		{strings.Repeat("é", 15), strings.Repeat("é", 8) + "…"}, // never half a character
	} {
		if got := clip(c.in, 20); got != c.want {
			t.Errorf("clip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDayColours(t *testing.T) {
	for _, c := range []struct {
		d    Day
		want Colour
	}{
		{Day{}, Grey},
		{Day{Good: 1440, MedianMS: 40, P95MS: 60}, Teal},
		{Day{Good: 1440, MedianMS: 40, P95MS: 300}, Teal},
		{Day{Good: 1440, MedianMS: 32, P95MS: 410}, Amber}, // 2026-10-14: 72 slow minutes, median fine
		{Day{Good: 1439, Failed: 1, MedianMS: 40, P95MS: 60}, Amber},
		{Day{Good: 1368, Failed: 72, MedianMS: 40, P95MS: 60}, Red}, // exactly 5%
		{Day{Good: 1369, Failed: 71, MedianMS: 40, P95MS: 60}, Amber},
		{Day{Failed: 3}, Red},
	} {
		if got := c.d.Colour(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.d, got, c.want)
		}
	}
}

func TestHeadline(t *testing.T) {
	now := time.Date(2026, 10, 13, 14, 3, 30, 0, time.UTC)
	res := func(s ...State) map[string]Result {
		m := map[string]Result{}
		for i, e := range endpoints {
			m[e.Path] = Result{State: s[i]}
		}
		return m
	}
	for _, c := range []struct {
		last *Check
		want string
		col  Colour
	}{
		{nil, "Status unknown: no checks yet", Grey},
		{&Check{At: now.Add(-time.Minute), Results: res(Good, Good, Good)}, "Every endpoint answering", Teal},
		{&Check{At: now.Add(-time.Minute), Results: res(Good, Failed, Good)}, "Some endpoints failing", Amber},
		{&Check{At: now.Add(-time.Minute), Results: res(Failed, Failed, Failed)}, "boot.watch not answering", Red},
		{&Check{At: now.Add(-time.Minute), Results: res(NoData, NoData, NoData)}, "Status unknown: the checker can't reach the internet", Grey},
		{&Check{At: now.Add(-7 * time.Minute), Results: res(Good, Good, Good)}, "Status unknown: last check 7 min ago", Grey},
	} {
		got, col := View{Now: now, Last: c.last}.Headline()
		if got != c.want || col != c.col {
			t.Errorf("%+v: %q %s, want %q %s", c.last, got, col, c.want, c.col)
		}
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 10, 12, 0, 0, 30, 0, time.UTC)
	st, err := openStore(dir, day)
	if err != nil {
		t.Fatal(err)
	}
	all := func(s State, ms int) map[string]Result {
		m := map[string]Result{}
		for _, e := range endpoints {
			m[e.Path] = Result{State: s, RTT: time.Duration(ms) * time.Millisecond, Family: "6"}
		}
		return m
	}
	// yesterday: 100 checks, 3 failed, 2 no data, 5 slow; today: 10 good
	for i := range 100 {
		r := all(Good, 40+i%5)
		switch {
		case i < 3:
			r = all(Failed, 0)
			r["/v1/sink"] = Result{State: Failed, Err: "HTTP 302 to \thttps://elsewhere\n" + strings.Repeat("x", 300)}
		case i < 5:
			r = all(NoData, 0)
		case i < 10:
			r = all(Good, 900)
		}
		if err := st.save(Check{At: day.Add(time.Duration(i) * time.Minute), Results: r}); err != nil {
			t.Fatal(err)
		}
	}
	today := day.Add(24 * time.Hour)
	for i := range 10 {
		h := &Health{DayBytes: int64(i), DailyBytes: 4 << 30, Since: today.Add(-time.Hour)}
		if i >= 5 {
			h.Since = today.Add(4 * time.Minute) // bootwatchd restarted
		}
		if err := st.save(Check{At: today.Add(time.Duration(i) * time.Minute), Results: all(Good, 30), Health: h}); err != nil {
			t.Fatal(err)
		}
	}
	now := today.Add(10 * time.Minute)
	check := func(st *store, what string) {
		t.Helper()
		v := st.view(now)
		r := v.Rows[0]
		if len(v.Rows) != len(endpoints) || len(r.Days) != days || r.Days[days-1].Date != "2026-10-13" || r.Days[days-2].Date != "2026-10-12" {
			t.Fatalf("%s: rows %d, days %d, last two %+v", what, len(v.Rows), len(r.Days), r.Days[days-2:])
		}
		y := r.Days[days-2]
		// 95 good: the median is the 48th fastest (42 ms), the p95 the 91st (900 ms: 5 were slow)
		if y.Good != 95 || y.Failed != 3 || y.MedianMS != 42 || y.P95MS != 900 || y.Colour() != Amber {
			t.Fatalf("%s: yesterday %+v %s", what, y, y.Colour())
		}
		if want := 100 * 105.0 / 108; r.Uptime < want-0.001 || r.Uptime > want+0.001 {
			t.Fatalf("%s: uptime %.3f, want %.3f", what, r.Uptime, want)
		}
		if v.Last == nil || !v.Last.At.Equal(today.Add(9*time.Minute)) || v.Last.Results["/v1/sink"].State != Good || v.Last.Results["/v1/sink"].Family != "6" {
			t.Fatalf("%s: last %+v", what, v.Last)
		}
		if len(v.Restarts) != 1 || !v.Restarts[0].Equal(today.Add(4*time.Minute)) {
			t.Fatalf("%s: restarts %v", what, v.Restarts)
		}
		if r.Days[0].Colour() != Grey {
			t.Fatalf("%s: a day without checks: %+v", what, r.Days[0])
		}
	}
	check(st, "in memory")
	if v := st.view(now); v.Health == nil || v.Health.DayBytes != 9 {
		t.Fatalf("health %+v", v.Health)
	}
	a := alertState{Fails: 3, Since: day, Pushed: true, LastErr: "HTTP 502"}
	if err := st.setAlertStates(map[string]alertState{"/v1/sink": a}, alertState{Fails: 2, Since: today}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// read back; a line a crash cut short is skipped, and the next check starts a line of its own
	f, _ := os.OpenFile(filepath.Join(dir, "checks", "2026-10-13.tsv"), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("1760313600\t/v1/ec")
	f.Close()
	st, err = openStore(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	check(st, "read back")
	if err := st.save(Check{At: now, Results: all(Good, 30)}); err != nil {
		t.Fatal(err)
	}
	if d := st.view(now).Rows[0].Days[days-1]; d.Good != 11 {
		t.Fatalf("after a cut-short line: %+v", d)
	}
	if got, blind := st.alertStates(); got["/v1/sink"] != a || got["/v1/echo"] != (alertState{}) || blind.Fails != 2 {
		t.Fatalf("alert states %+v %+v", got, blind)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "checks", "2026-10-12.tsv"))
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if len(line) > 300 || strings.Count(line, "\t") != 5 {
			t.Fatalf("a reason too long or split: %q", line)
		}
	}
	st.Close()

	// 60 days on, the first day's checks go (its summaries into days.tsv), its restart too
	later := day.Add(days * 24 * time.Hour)
	st, err = openStore(dir, later)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(filepath.Join(dir, "checks", "2026-10-12.tsv")); !os.IsNotExist(err) {
		t.Fatalf("2026-10-12.tsv: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checks", "2026-10-13.tsv")); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "days.tsv"))
	if !strings.HasPrefix(string(b), "2026-10-12\t/v1/echo\t95\t3\t42\t900\n") || strings.Count(string(b), "\n") != len(endpoints) {
		t.Fatalf("days.tsv:\n%s", b)
	}
	if v := st.view(later); len(v.Restarts) != 1 {
		t.Fatalf("restarts %v", v.Restarts) // 2026-10-13's is still shown
	}
}

func TestAlerts(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 14, 3, 30, 0, time.UTC)
	bad, good, none := Result{State: Failed, Err: "HTTP 502"}, Result{State: Good}, Result{State: NoData}
	var s alertState
	var got []string
	run := func(rs ...Result) {
		for _, r := range rs {
			var ev *event
			s, ev = step(s, "/v1/sink", r, t0)
			if ev != nil {
				got = append(got, fmt.Sprintf("%v %s", ev.Down, ev.Detail))
				if s.Fails > 0 {
					s.Pushed = true // delivered
				}
			}
			t0 = t0.Add(time.Minute)
		}
	}
	run(bad, bad, good)            // a blip: nothing
	run(bad, none, bad, none, bad) // no data neither counts nor breaks the run: the 3rd failure pages
	run(bad, bad)                  // still down: nothing more
	run(none, good, good)          // recovery, once
	want := []string{
		"true since 14:06 UTC (3 checks in a row): HTTP 502",
		"false down 8m (since 14:06 UTC)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// One push per state change (owner, 2026-10-14): a whole-service outage pages once going down
// and once coming back, not once per endpoint; endpoints that change at different checks are
// different state changes; an undelivered push is retried on the next check.
func TestCombinedPushes(t *testing.T) {
	var pushes []string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if fail {
			http.Error(w, `{"status":0,"errors":["down"]}`, http.StatusServiceUnavailable)
			return
		}
		pushes = append(pushes, r.PostForm.Get("title")+" | "+r.PostForm.Get("message"))
		io.WriteString(w, `{"status":1}`)
	}))
	defer srv.Close()
	t0 := time.Date(2026, 10, 14, 0, 56, 30, 0, time.UTC)
	st, err := openStore(t.TempDir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := &watcher{st: st, po: &pushover{token: "t", user: "u", api: srv.URL, hc: srv.Client()}}
	check := func(echo, stream, sink State) {
		c := Check{At: t0, Results: map[string]Result{}}
		for path, s := range map[string]State{"/v1/echo": echo, "/v1/stream": stream, "/v1/sink": sink} {
			r := Result{State: s}
			if s == Failed {
				r.Err = "HTTP 502"
			}
			c.Results[path] = r
		}
		w.alerts(context.Background(), c, t0)
		t0 = t0.Add(time.Minute)
	}
	F, G := Failed, Good
	check(F, F, F)
	check(F, F, F)
	check(F, F, F) // 00:58:30: one push for all three
	check(G, G, G) // 00:59:30: one push for all three
	check(G, G, F)
	check(G, G, F)
	fail = true
	check(G, G, F) // 01:02:30: Pushover down: not delivered
	fail = false
	check(G, F, F) // 01:03:30: the sink's push retried; the stream only just started failing
	check(G, F, F)
	check(G, F, F) // 01:05:30: the stream's own state change
	check(G, G, G) // 01:06:30: both back together, different durations
	want := []string{
		"boot.watch not answering | since 00:56 UTC (3 checks in a row): HTTP 502",
		"boot.watch answering again | down 3m (since 00:56 UTC)",
		"boot.watch /v1/sink failing | since 01:00 UTC (4 checks in a row): HTTP 502",
		"boot.watch /v1/stream failing | since 01:03 UTC (3 checks in a row): HTTP 502",
		"boot.watch /v1/stream, /v1/sink answering again | /v1/stream: down 3m (since 01:03 UTC)\n/v1/sink: down 6m (since 01:00 UTC)",
	}
	if strings.Join(pushes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pushes:\n%s\nwant:\n%s", strings.Join(pushes, "\n"), strings.Join(want, "\n"))
	}
}

func TestPushover(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.PostForm
		if got.Get("token") == "bad" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"status":0,"errors":["application token is invalid"]}`)
			return
		}
		io.WriteString(w, `{"status":1}`)
	}))
	defer srv.Close()
	p := &pushover{token: "t", user: "u", api: srv.URL, page: "https://status.boot.watch", hc: srv.Client()}
	if err := p.send(context.Background(), message{Title: "T", Body: "B"}); err != nil {
		t.Fatal(err)
	}
	if got.Get("user") != "u" || got.Get("title") != "T" || got.Get("message") != "B" || got.Get("url") != "https://status.boot.watch" {
		t.Fatalf("form %v", got)
	}
	// Pushover turns away messages over 1024 characters: a long one goes out cut short
	if err := p.send(context.Background(), message{Title: "T", Body: strings.Repeat("é", 800)}); err != nil || len(got.Get("message")) > 1000 {
		t.Fatalf("long message: %v, %d bytes", err, len(got.Get("message")))
	}
	p.token = "bad"
	if err := p.send(context.Background(), message{Title: "T", Body: "B"}); err == nil || !strings.Contains(err.Error(), "application token is invalid") {
		t.Fatalf("err %v", err)
	}
}

// The checker pushes when it can't check for 10 minutes, and when it can again; when the
// network itself was down and nothing could go out, one push afterwards says when it was blind.
func TestBlind(t *testing.T) {
	var pushes []string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if fail {
			http.Error(w, "", http.StatusServiceUnavailable)
			return
		}
		pushes = append(pushes, r.PostForm.Get("title")+" | "+r.PostForm.Get("message"))
		io.WriteString(w, `{"status":1}`)
	}))
	defer srv.Close()
	t0 := time.Date(2026, 10, 17, 14, 5, 30, 0, time.UTC)
	st, err := openStore(t.TempDir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := &watcher{st: st, po: &pushover{token: "t", user: "u", api: srv.URL, hc: srv.Client()}}
	check := func(n int, s State) {
		for range n {
			c := Check{At: t0, Results: map[string]Result{}}
			for _, e := range endpoints {
				c.Results[e.Path] = Result{State: s}
				if s == NoData {
					c.Results[e.Path] = Result{State: s, Err: "control: HTTP 403"}
				}
			}
			w.alerts(context.Background(), c, t0)
			t0 = t0.Add(time.Minute)
		}
	}
	check(9, NoData) // 14:05–14:13: not yet
	check(1, NoData) // 14:14: the 10th in a row
	check(5, NoData) // pushed once only
	check(2, Good)   // 14:20: back, once
	fail = true
	check(12, NoData) // 14:22–14:33: the network's down too: nothing goes out
	fail = false
	check(1, Good) // 14:34: one push afterwards
	want := []string{
		"bootwatcherd can't check boot.watch | no data since 14:05 UTC (10 checks in a row): control: HTTP 403",
		"bootwatcherd checking again | blind 15m (since 14:05 UTC)",
		"bootwatcherd was blind | 14:22–14:34 UTC (12m): control: HTTP 403",
	}
	if strings.Join(pushes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pushes:\n%s\nwant:\n%s", strings.Join(pushes, "\n"), strings.Join(want, "\n"))
	}
}

// Bug report alerts: one push when reports stop being kept, one when they're kept again, one a
// day near the cap; a bootwatchd restart starts the counts again.
func TestBugStep(t *testing.T) {
	start := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	h := &Health{Since: start, BugsPerDay: 500}
	at := start.Add(time.Minute)
	var s bugAlert
	var m *message
	next := func(kept, failed int64, today int) *message {
		h.Bugs, h.BugFailed, h.BugsToday = kept, failed, today
		at = at.Add(time.Minute)
		s, m = bugStep(s, h, at)
		return m
	}
	if next(2, 0, 2) != nil {
		t.Fatal("kept ones push nothing")
	}
	if m := next(2, 1, 2); m == nil || !m.Down || !strings.Contains(m.Title, "isn't keeping") {
		t.Fatalf("a failure: %+v", m)
	}
	if next(2, 3, 2) != nil {
		t.Fatal("more failures push nothing more")
	}
	if m := next(3, 3, 3); m == nil || m.Down || !strings.Contains(m.Title, "again") {
		t.Fatalf("kept again: %+v", m)
	}
	h.Since = at // a restart
	if next(0, 0, 3) != nil {
		t.Fatal("a restart pushes nothing")
	}
	if m := next(1, 0, 400); m == nil || !strings.Contains(m.Title, "near today's cap") {
		t.Fatalf("near the cap: %+v", m)
	}
	if next(2, 0, 450) != nil {
		t.Fatal("the near-cap push goes once a day")
	}
	if s, m := bugStep(bugAlert{}, &Health{Since: start}, at); m != nil || s.Since != (time.Time{}) {
		t.Fatal("a bootwatchd without bug reports pushes nothing")
	}
}
