package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

var pageNow = time.Date(2026, 10, 13, 14, 3, 30, 0, time.UTC)

// testView is 60 days of every endpoint answering, the last check 30 s ago; dayFn may change days.
func testView(dayFn func(path string, i int, d *Day)) View {
	v := View{
		Now:    pageNow,
		Last:   &Check{At: pageNow.Add(-30 * time.Second), Results: map[string]Result{}},
		Health: &Health{DayBytes: 1_400_000, DailyBytes: 4 << 30},
	}
	for _, e := range endpoints {
		v.Last.Results[e.Path] = Result{State: Good, RTT: 41 * time.Millisecond}
		r := Row{Endpoint: e, Days: make([]Day, days)}
		good, total := 0, 0
		for i := range r.Days {
			d := Day{Date: pageNow.AddDate(0, 0, i-days+1).Format(time.DateOnly), Good: 1440, MedianMS: 41, P95MS: 60}
			if dayFn != nil {
				dayFn(e.Path, i, &d)
			}
			r.Days[i] = d
			good += d.Good
			total += d.Good + d.Failed
		}
		r.Uptime = -1
		if total > 0 {
			r.Uptime = float64(good) / float64(total) * 100
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}

func render(t *testing.T, v View) string {
	t.Helper()
	var b bytes.Buffer
	if err := renderPage(&b, v); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	checkBalanced(t, s)
	return s
}

func contains(t *testing.T, page string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(page, w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

func lacks(t *testing.T, page string, bad ...string) {
	t.Helper()
	for _, w := range bad {
		if strings.Contains(page, w) {
			t.Errorf("page has %q", w)
		}
	}
}

func count(t *testing.T, page, s string, want int) {
	t.Helper()
	if n := strings.Count(page, s); n != want {
		t.Errorf("%q: %d times, want %d", s, n, want)
	}
}

var (
	styleBlock = regexp.MustCompile(`(?s)<style>.*?</style>`)
	tagRE      = regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9]*)[^>]*?(/?)>`)
	voidTags   = map[string]bool{"meta": true, "link": true, "br": true, "hr": true, "img": true, "input": true}
)

// checkBalanced checks every element is closed in order.
func checkBalanced(t *testing.T, page string) {
	t.Helper()
	var stack []string
	for _, m := range tagRE.FindAllStringSubmatch(styleBlock.ReplaceAllString(page, "<style></style>"), -1) {
		closing, name, self := m[1] == "/", strings.ToLower(m[2]), m[3] == "/"
		switch {
		case self || voidTags[name] && !closing:
		case !closing:
			stack = append(stack, name)
		case len(stack) == 0 || stack[len(stack)-1] != name:
			t.Fatalf("unbalanced </%s>, open: %v", name, stack)
		default:
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		t.Fatalf("unclosed: %v", stack)
	}
}

func TestPageAllTeal(t *testing.T) {
	p := render(t, testView(nil))
	contains(t, p,
		`<meta http-equiv="refresh" content="60">`,
		`<link rel="icon" href="/favicon.svg" type="image/svg+xml">`,
		`url("/fonts/JetBrainsMono-Regular.woff2")`, `url("/fonts/JetBrainsMono-SemiBold.woff2")`, "font-display:swap",
		`class="m-teal"`, "<h1>Every endpoint answering</h1>",
		"Checked every minute · last check 14:03 UTC",
		"Endpoint · last 60 days",
		"/v1/echo", "nonce back, round trip", "/v1/stream", "/v1/sink",
		`title="2026-10-12 · 1440 of 1440 good · median 41 ms · p95 60 ms"`,
		`title="2026-10-13 · 1440 of 1440 good · median 41 ms · p95 60 ms"`,
		"phone relay", `<div class="what">planned</div>`, "not yet",
		"answering", "slow or partly down", "not answering", "no data",
		"Today: 1.4 MB of the 4 GiB daily budget",
		"bootwatchd keeps no request log and no addresses on disk.",
		`Part of <a href="https://omaboot.live/">OmaBoot? Live</a>`,
		// styled like omaboot.live's own DoesItOmarchy.com link: purple, with the arrow
		`<a class="out" href="https://omaboot.live/">OmaBoot.live<span aria-hidden="true"> ↗</span></a>`,
		`<a href="https://omaboot.live/docs/#bootwatch">Protocol</a>`,
	)
	// the protocol is documented on omaboot.live only (owner, 2026-10-17)
	lacks(t, p, "<details", "Protocol Details", `id="protocol"`, "Test vectors")
	lacks(t, p, "Sample data", "<script", "javascript:", "@import", "style=\"")
	count(t, p, `<div class="uptime">100.00%</div>`, 3)
	count(t, p, `<div class="c-teal" title=`, 3*days)
	// The page's only absolute URLs are omaboot.live links: nothing third-party is loaded.
	count(t, p, "://", 3)
	count(t, p, `"https://omaboot.live/`, 3)
	// Phone relay: 60 bars with no data.
	count(t, p, "<div></div>", days)
}

func TestPageAmberRed(t *testing.T) {
	v := testView(func(path string, i int, d *Day) {
		switch {
		case path == "/v1/stream" && i == 23:
			d.P95MS = 450 // slow
		case path == "/v1/sink" && i == 42:
			d.Good, d.Failed = 1300, 140
		case path == "/v1/sink" && i == 23:
			d.Good, d.Failed = 1430, 10
		}
	})
	v.Last.Results["/v1/sink"] = Result{State: Failed, Err: "timeout"}
	p := render(t, v)
	contains(t, p,
		`class="m-amber"`, "<h1>Some endpoints failing</h1>",
		`<div class="c-amber" title="2026-09-07 · 1440 of 1440 good · median 41 ms · p95 450 ms">`,
		`<div class="c-amber" title="2026-09-07 · 1430 of 1440 good · median 41 ms · p95 60 ms">`,
		`<div class="c-red" title="2026-09-26 · 1300 of 1440 good · median 41 ms · p95 60 ms">`,
		`<div class="uptime">99.82%</div>`, // 86250/86400 = 99.826…, rounded down
	)
	count(t, p, `<div class="c-amber" title=`, 2)
	count(t, p, `<div class="c-red" title=`, 1)
	count(t, p, `<div class="c-teal" title=`, 3*days-3)
	count(t, p, `<div class="uptime">100.00%</div>`, 2)
}

func TestPageStale(t *testing.T) {
	v := testView(nil)
	v.Last.At = pageNow.Add(-10 * time.Minute)
	p := render(t, v)
	contains(t, p, `class="m-grey"`, "<h1>Status unknown: last check 10 min ago</h1>", "last check 13:53 UTC")
}

func TestPageNoChecks(t *testing.T) {
	v := testView(func(_ string, _ int, d *Day) { *d = Day{} })
	v.Last, v.Health = nil, nil
	p := render(t, v)
	contains(t, p,
		`class="m-grey"`, "<h1>Status unknown: no checks yet</h1>",
		"Checked every minute · last check —",
		`<div class="c-grey" title="2026-08-15 · no checks">`,
		`<div class="c-grey" title="2026-10-13 · no checks">`,
	)
	count(t, p, `<div class="uptime">—</div>`, 3)
	count(t, p, `<div class="c-grey" title=`, 3*days)
	lacks(t, p, "c-teal\" title", "daily budget")
}

func TestPageNoHealth(t *testing.T) {
	v := testView(nil)
	v.Health = nil
	lacks(t, render(t, v), "Today:", "daily budget")
}

// When bootwatchd's count of the day started after midnight (it started today without one), the
// line says since when; the day's restarts show in its bars' tooltips.
func TestPageRestart(t *testing.T) {
	v := testView(nil)
	v.Health.Since = time.Date(2026, 10, 13, 9, 30, 2, 0, time.UTC)
	v.Health.DayBytesSince = v.Health.Since
	v.Restarts = []time.Time{time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC), v.Health.Since}
	p := render(t, v)
	contains(t, p,
		"Today since 09:30 UTC: 1.4 MB of the 4 GiB daily budget",
		`title="2026-10-12 · 1440 of 1440 good · median 41 ms · p95 60 ms · bootwatchd restarted 04:00 UTC"`,
		`title="2026-10-13 · 1440 of 1440 good · median 41 ms · p95 60 ms · bootwatchd restarted 09:30 UTC"`,
	)
	count(t, p, "bootwatchd restarted", 6)
	v.Health.DayBytesSince = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) // counted from midnight
	contains(t, render(t, v), "Today: 1.4 MB of the 4 GiB daily budget")
	v.Health.DayBytesSince = time.Time{} // a bootwatchd from before day_bytes_since
	contains(t, render(t, v), "Today: 1.4 MB of the 4 GiB daily budget")
}

// Why a check failed carries the checker's own address (Go's network errors name both ends),
// and doiomad's server is behind its tunnel: the reasons never reach the page or status.json.
func TestNoReasonsInPublic(t *testing.T) {
	v := testView(nil)
	v.Last.Results["/v1/echo"] = Result{State: Failed, Err: "read tcp [2001:db8::1]:38716->[2001:db8::2]:443: read: connection reset by peer"}
	for name, out := range map[string]string{"page": render(t, v), "status.json": string(statusJSON(v))} {
		if strings.Contains(out, "2001:db8") || strings.Contains(out, "connection reset") {
			t.Errorf("%s shows a failure's reason", name)
		}
	}
}

// The CSP allows the page's stylesheet by its hash: the rendered <style> must hash to it.
func TestCSPHash(t *testing.T) {
	p := render(t, testView(nil))
	_, css, _ := strings.Cut(p, "<style>")
	css, _, _ = strings.Cut(css, "</style>")
	sum := sha256.Sum256([]byte(css))
	if want := "style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
		t.Fatalf("csp %q lacks %q", csp, want)
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "data:") {
		t.Fatalf("csp %q", csp)
	}
}

func TestRoutes(t *testing.T) {
	now := time.Now().UTC()
	st, err := openStore(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := routes(st, "https://status.boot.watch")
	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for i := 0; i < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("/fresh"); w.Code != http.StatusServiceUnavailable || w.Body.String() != "no checks yet\n" {
		t.Fatalf("/fresh with no checks: %d %q", w.Code, w.Body)
	}
	good := Check{At: now.Add(-4 * time.Minute), Results: map[string]Result{}}
	for _, e := range endpoints {
		good.Results[e.Path] = Result{State: Good, RTT: 20 * time.Millisecond}
	}
	st.save(good)
	if w := get("/fresh"); w.Code != http.StatusServiceUnavailable || w.Body.String() != "last check 4 min ago\n" {
		t.Fatalf("/fresh, stale: %d %q", w.Code, w.Body)
	}
	good.At = now.Add(-time.Minute)
	st.save(good)
	if w := get("/fresh"); w.Code != http.StatusOK {
		t.Fatalf("/fresh: %d %q", w.Code, w.Body)
	}
	a, b := get("/"), get("/")
	if a.Code != http.StatusOK || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) || !strings.Contains(a.Body.String(), "Every endpoint answering") {
		t.Fatalf("page: %d, same twice %v", a.Code, bytes.Equal(a.Body.Bytes(), b.Body.Bytes()))
	}
	for _, path := range []string{"/", "/status.json", "/favicon.svg", "/fresh"} {
		w := get(path)
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Strict-Transport-Security": "max-age=31536000", "Referrer-Policy": "no-referrer"} {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s: %s %q", path, k, got)
			}
		}
	}
	if got := get("/").Header().Get("Content-Security-Policy"); got != csp {
		t.Errorf("CSP %q", got)
	}
	// plain HTTP through Cloudflare goes to the HTTPS page
	w := get("/status.json?x=1", "X-Forwarded-Proto", "http")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "https://status.boot.watch/status.json?x=1" {
		t.Fatalf("http: %d %q", w.Code, w.Header().Get("Location"))
	}
	body, _ := io.ReadAll(get("/status.json").Body)
	if !strings.Contains(string(body), `"p95_ms":20`) || !strings.Contains(string(body), `"restarts":[]`) {
		t.Fatalf("status.json: %s", body)
	}
}

func TestStatusJSON(t *testing.T) {
	v := testView(func(path string, i int, d *Day) {
		if path == "/v1/sink" && i == 42 {
			d.Good, d.Failed = 1300, 140
		}
	})
	var s struct {
		Headline  string  `json:"headline"`
		Colour    Colour  `json:"colour"`
		LastCheck *string `json:"last_check"`
		Endpoints []struct {
			Path   string   `json:"path"`
			Uptime *float64 `json:"uptime"`
			Days   []struct {
				Date     string `json:"date"`
				Good     int    `json:"good"`
				Failed   int    `json:"failed"`
				MedianMS int    `json:"median_ms"`
				Colour   Colour `json:"colour"`
			} `json:"days"`
		} `json:"endpoints"`
		Budget *Health `json:"budget"`
	}
	if err := json.Unmarshal(statusJSON(v), &s); err != nil {
		t.Fatal(err)
	}
	if s.Headline != "Every endpoint answering" || s.Colour != Teal || s.LastCheck == nil || *s.LastCheck != "2026-10-13T14:03:00Z" {
		t.Errorf("head: %q %q %v", s.Headline, s.Colour, s.LastCheck)
	}
	if s.Budget == nil || *s.Budget != *v.Health {
		t.Errorf("budget: %v", s.Budget)
	}
	if len(s.Endpoints) != 3 || s.Endpoints[2].Path != "/v1/sink" || len(s.Endpoints[2].Days) != days {
		t.Fatalf("endpoints: %+v", s.Endpoints)
	}
	d := s.Endpoints[2].Days[42]
	if d.Date != "2026-09-26" || d.Good != 1300 || d.Failed != 140 || d.MedianMS != 41 || d.Colour != Red {
		t.Errorf("day: %+v", d)
	}
	if u := s.Endpoints[2].Uptime; u == nil || *u != v.Rows[2].Uptime {
		t.Errorf("uptime: %v", u)
	}

	v = testView(func(_ string, _ int, d *Day) { *d = Day{} })
	v.Last, v.Health = nil, nil
	raw := string(statusJSON(v))
	for _, w := range []string{`"last_check":null`, `"uptime":null`, `"budget":null`, `"colour":"grey"`, `"date":"2026-10-13"`} {
		if !strings.Contains(raw, w) {
			t.Errorf("json lacks %s", w)
		}
	}
}

func TestUptimeText(t *testing.T) {
	for u, want := range map[float64]string{-1: "—", 100: "100.00%", 99.97: "99.97%", 99.9999: "99.99%", 0: "0.00%"} {
		if got := uptimeText(u); got != want {
			t.Errorf("uptimeText(%v) = %q, want %q", u, got, want)
		}
	}
}

func TestBytes(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{decimalBytes(0), "0 B"},
		{decimalBytes(999), "999 B"},
		{decimalBytes(1_400_000), "1.4 MB"},
		{decimalBytes(46_000_000), "46 MB"},
		{decimalBytes(2_345_000_000), "2.3 GB"},
		{binaryBytes(4 << 30), "4 GiB"},
		{binaryBytes(1536 << 20), "1.5 GiB"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestAssets(t *testing.T) {
	h := assets()
	for _, c := range []struct {
		method, path string
		code         int
		ctype        string
	}{
		{"GET", "/favicon.svg", 200, "image/svg+xml"},
		{"GET", "/fonts/JetBrainsMono-Regular.woff2", 200, "font/woff2"},
		{"HEAD", "/fonts/JetBrainsMono-SemiBold.woff2", 200, "font/woff2"},
		{"POST", "/favicon.svg", 405, ""},
		{"GET", "/fonts/OFL.txt", 404, ""},
		{"GET", "/fonts/Nope.woff2", 404, ""},
		{"GET", "/fonts/", 404, ""},
		{"GET", "/page.html", 404, ""},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != c.code {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, w.Code, c.code)
			continue
		}
		if c.code != http.StatusOK {
			continue
		}
		if got := w.Header().Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type %q", c.path, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=604800" {
			t.Errorf("%s: Cache-Control %q", c.path, got)
		}
		if c.method == "GET" && w.Body.Len() < 100 {
			t.Errorf("%s: %d bytes", c.path, w.Body.Len())
		}
	}
}
