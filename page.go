package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed assets/page.html assets/favicon.svg assets/fonts/*.woff2
var assetFS embed.FS

var pageTemplate = template.Must(template.ParseFS(assetFS, "assets/page.html"))

// pageData is View as the template draws it.
type pageData struct {
	Headline  string
	Colour    Colour
	LastCheck string // "14:03 UTC", or "—"
	Rows      []pageRow
	Relay     []struct{} // the phone relay's grey bars
	Budget    *pageBudget
}

type pageRow struct {
	Path, What string
	Bars       []pageBar
	Uptime     string
}

type pageBar struct {
	Colour Colour
	Title  string
}

type pageBudget struct {
	Day, Daily string
	Since      string // "09:30 UTC" when bootwatchd's count of the day started after midnight
}

// renderPage writes the status page.
func renderPage(w io.Writer, v View) error {
	headline, colour := v.Headline()
	d := pageData{
		Headline:  headline,
		Colour:    colour,
		LastCheck: "—",
		Relay:     make([]struct{}, days),
	}
	if v.Last != nil {
		d.LastCheck = v.Last.At.UTC().Format("15:04") + " UTC"
	}
	restarts := map[string][]string{} // by date: "09:30"
	for _, t := range v.Restarts {
		restarts[dayOf(t)] = append(restarts[dayOf(t)], hhmm(t))
	}
	for _, r := range v.Rows {
		pr := pageRow{Path: r.Path, What: r.What, Uptime: uptimeText(r.Uptime), Bars: make([]pageBar, len(r.Days))}
		for i, day := range r.Days {
			date := dayDate(v, r, i)
			pr.Bars[i] = pageBar{Colour: day.Colour(), Title: dayTitle(date, day, restarts[date])}
		}
		d.Rows = append(d.Rows, pr)
	}
	if h := v.Health; h != nil {
		d.Budget = &pageBudget{Day: decimalBytes(h.DayBytes), Daily: binaryBytes(h.DailyBytes)}
		// bootwatchd keeps the day's count across restarts; it starts later than midnight only
		// when bootwatchd started today without one (its first start, or a lost count)
		if t := h.DayBytesSince; dayOf(t) == dayOf(v.Now) && !t.Equal(t.Truncate(24*time.Hour)) {
			d.Budget.Since = hhmm(t) + " UTC"
		}
	}
	return pageTemplate.Execute(w, d)
}

// dayDate is row r's day i's date; a day without checks may come zero, so count back from today.
func dayDate(v View, r Row, i int) string {
	if d := r.Days[i].Date; d != "" {
		return d
	}
	return v.Now.UTC().AddDate(0, 0, i-len(r.Days)+1).Format(time.DateOnly)
}

// dayTitle is a bar's tooltip: "2026-10-12 · 1440 of 1440 good · median 41 ms · p95 60 ms",
// then " · bootwatchd restarted 09:30 UTC" on a day it restarted.
func dayTitle(date string, d Day, restarts []string) string {
	n := d.Good + d.Failed
	s := date + " · no checks"
	if n > 0 {
		s = date + " · " + strconv.Itoa(d.Good) + " of " + strconv.Itoa(n) + " good"
	}
	if d.Good > 0 {
		s += " · median " + strconv.Itoa(d.MedianMS) + " ms · p95 " + strconv.Itoa(d.P95MS) + " ms"
	}
	if len(restarts) > 0 {
		s += " · bootwatchd restarted " + strings.Join(restarts, ", ") + " UTC"
	}
	return s
}

// uptimeText is "99.97%", rounded down so failures never show as 100.00%; "—" with no checks.
func uptimeText(u float64) string {
	if u < 0 {
		return "—"
	}
	return strconv.FormatFloat(math.Floor(u*100)/100, 'f', 2, 64) + "%"
}

// decimalBytes is a byte count in SI units: "512 B", "1.4 MB", "46 MB".
func decimalBytes(n int64) string {
	return humanBytes(n, 1000, []string{"B", "KB", "MB", "GB", "TB"})
}

// binaryBytes is a byte count in IEC units: "4 GiB".
func binaryBytes(n int64) string {
	return humanBytes(n, 1024, []string{"B", "KiB", "MiB", "GiB", "TiB"})
}

func humanBytes(n int64, base float64, units []string) string {
	f, u := float64(n), 0
	for f >= base && u < len(units)-1 {
		f /= base
		u++
	}
	if u == 0 {
		return strconv.FormatInt(n, 10) + " B"
	}
	s := strconv.FormatFloat(f, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + " " + units[u]
}

type jsonStatus struct {
	Version   string         `json:"version"` // bootwatcherd's build
	Headline  string         `json:"headline"`
	Colour    Colour         `json:"colour"`
	LastCheck *string        `json:"last_check"`
	Endpoints []jsonEndpoint `json:"endpoints"`
	Budget    *Health        `json:"budget"`
	Restarts  []string       `json:"restarts"` // bootwatchd's, in the days shown
}

type jsonEndpoint struct {
	Path   string    `json:"path"`
	Uptime *float64  `json:"uptime"`
	Days   []jsonDay `json:"days"`
}

type jsonDay struct {
	Date     string `json:"date"`
	Good     int    `json:"good"`
	Failed   int    `json:"failed"`
	MedianMS int    `json:"median_ms"`
	P95MS    int    `json:"p95_ms"`
	Colour   Colour `json:"colour"`
}

// statusJSON is the page's data for /status.json.
func statusJSON(v View) []byte {
	s := jsonStatus{Version: version, Endpoints: make([]jsonEndpoint, 0, len(v.Rows)), Budget: v.Health, Restarts: []string{}}
	for _, t := range v.Restarts {
		s.Restarts = append(s.Restarts, t.UTC().Format(time.RFC3339))
	}
	s.Headline, s.Colour = v.Headline()
	if v.Last != nil {
		t := v.Last.At.UTC().Format(time.RFC3339)
		s.LastCheck = &t
	}
	for _, r := range v.Rows {
		e := jsonEndpoint{Path: r.Path, Days: make([]jsonDay, len(r.Days))}
		if r.Uptime >= 0 {
			u := r.Uptime
			e.Uptime = &u
		}
		for i, d := range r.Days {
			e.Days[i] = jsonDay{Date: dayDate(v, r, i), Good: d.Good, Failed: d.Failed, MedianMS: d.MedianMS, P95MS: d.P95MS, Colour: d.Colour()}
		}
		s.Endpoints = append(s.Endpoints, e)
	}
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // only plain types: can't fail
	}
	return b
}

// assets serves /fonts/<file>.woff2 and /favicon.svg from the embedded files.
func assets() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var name, ctype string
		switch p := r.URL.Path; {
		case p == "/favicon.svg":
			name, ctype = "assets/favicon.svg", "image/svg+xml"
		case path.Dir(p) == "/fonts" && path.Ext(p) == ".woff2":
			name, ctype = "assets/fonts/"+path.Base(p), "font/woff2"
		default:
			http.NotFound(w, r)
			return
		}
		b, err := assetFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "public, max-age=604800")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	})
}
