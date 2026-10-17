package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// probeSize is what each check moves down (stream) and up (sink): 16 KiB each way, ~47 MB a day.
const probeSize = 16 << 10

// prober checks boot.watch the way a Mac does, through its public address.
type prober struct {
	target  string // https://boot.watch
	control string // a URL that answers when the checker's own network works
}

func newProber(target, control string) *prober {
	return &prober{target: strings.TrimRight(target, "/"), control: control}
}

// session is one check's HTTP client, set up as OmaBoot? Live's client sets up one test
// (boot-live's internal/bootwatch): a fresh connection per host, HTTP/1.1, the default dialing
// (IPv6 first when there is one), closed when the check ends. A connection kept from one check
// to the next went stale behind Cloudflare and failed the next check's first request, and a slow
// path stayed with it until it was replaced: the checker measured its own connection.
var session = newSession

func newSession() (*http.Client, *http.Transport) {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true, // the bytes as they are
		MaxConnsPerHost:     1,    // one connection, reused within the check
		IdleConnTimeout:     30 * time.Second,
		Protocols:           new(http.Protocols),
	}
	tr.Protocols.SetHTTP1(true)
	return &http.Client{
		Transport:     tr,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, tr
}

// check runs one check: the control first (when it fails, every endpoint is no data), then
// echo, stream and sink, each verified, then the counters.
func (p *prober) check(ctx context.Context, at time.Time) Check {
	hc, tr := session()
	defer tr.CloseIdleConnections()
	c := Check{At: at, Results: map[string]Result{}}
	if err := p.controlCheck(ctx, hc); err != nil {
		for _, e := range endpoints {
			c.Results[e.Path] = Result{State: NoData, Err: clip("control: "+err.Error(), maxErr)}
		}
		return c
	}
	c.Results["/v1/echo"] = p.echo(ctx, hc)
	c.Results["/v1/stream"] = p.stream(ctx, hc)
	c.Results["/v1/sink"] = p.sink(ctx, hc)
	for path, r := range c.Results {
		r.Err = clip(r.Err, maxErr)
		c.Results[path] = r
	}
	c.Health = p.health(ctx, hc)
	return c
}

// do sends one request; family is "4" or "6" for the connection it went over.
func do(ctx context.Context, hc *http.Client, method, u string, body []byte) (res *http.Response, family string, err error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		if a, ok := i.Conn.RemoteAddr().(*net.TCPAddr); ok {
			family = "6"
			if a.IP.To4() != nil {
				family = "4"
			}
		}
	}})
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "bootwatcherd")
	res, err = hc.Do(req)
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // the reason, without the URL
	}
	return res, family, err
}

func (p *prober) controlCheck(ctx context.Context, hc *http.Client) error {
	res, _, err := do(ctx, hc, http.MethodGet, p.control, nil)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

// maxErr is how much of a failure's reason is kept: a server's answer (a Location header) can
// be long, and it goes into every check's record and into pushes.
const maxErr = 200

// clip is s on one line, cut to at most n bytes.
func clip(s string, n int) string {
	return cut(strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s), n)
}

// cut is s cut to at most n bytes (on a character boundary), ending "…" when cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func failed(family, format string, a ...any) Result {
	return Result{State: Failed, Family: family, Err: fmt.Sprintf(format, a...)}
}

func status(res *http.Response, family string) Result {
	s := fmt.Sprintf("HTTP %d", res.StatusCode)
	if to := res.Header.Get("Location"); to != "" {
		s += " to " + to
	}
	return Result{State: Failed, Family: family, Err: s}
}

func (p *prober) echo(ctx context.Context, hc *http.Client) Result {
	n := newSeed()
	t0 := time.Now()
	res, fam, err := do(ctx, hc, http.MethodGet, p.target+"/v1/echo?n="+n.String(), nil)
	if err != nil {
		return failed(fam, "%v", err)
	}
	got, err := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	res.Body.Close()
	switch {
	case err != nil:
		return failed(fam, "%v", err)
	case res.StatusCode != http.StatusOK:
		return status(res, fam)
	case string(got) != n.String():
		return failed(fam, "the echo came back wrong")
	}
	return Result{State: Good, RTT: time.Since(t0), Family: fam}
}

func (p *prober) stream(ctx context.Context, hc *http.Client) Result {
	s := newSeed()
	t0 := time.Now()
	res, fam, err := do(ctx, hc, http.MethodGet, fmt.Sprintf("%s/v1/stream?seed=%s&n=%d", p.target, s, probeSize), nil)
	if err != nil {
		return failed(fam, "%v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return status(res, fam)
	}
	n, ok, err := match(io.LimitReader(res.Body, probeSize+1), s)
	switch {
	case ok < n:
		return failed(fam, "byte %d was wrong", ok)
	case err != nil:
		return failed(fam, "broke off after %d bytes: %v", n, err)
	case n != probeSize:
		return failed(fam, "came with %d of %d bytes", n, probeSize)
	}
	return Result{State: Good, RTT: time.Since(t0), Family: fam}
}

func (p *prober) sink(ctx context.Context, hc *http.Client) Result {
	s := newSeed()
	body := make([]byte, probeSize)
	stream(s).Read(body)
	t0 := time.Now()
	res, fam, err := do(ctx, hc, http.MethodPost, p.target+"/v1/sink?seed="+s.String(), body)
	if err != nil {
		return failed(fam, "%v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return status(res, fam)
	}
	var a struct{ N, Match int64 }
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<10)).Decode(&a); err != nil {
		return failed(fam, "the answer wasn't bootwatchd's")
	}
	if a.N != probeSize || a.Match != probeSize {
		return failed(fam, "got %d of %d bytes, %d of them right", a.N, probeSize, a.Match)
	}
	return Result{State: Good, RTT: time.Since(t0), Family: fam}
}

// health reads bootwatchd's counters for the budget line and its restarts; nil when they don't
// come.
func (p *prober) health(ctx context.Context, hc *http.Client) *Health {
	res, _, err := do(ctx, hc, http.MethodGet, p.target+"/v1/health", nil)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	var h Health
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&h) != nil || h.DailyBytes == 0 {
		return nil
	}
	return &h
}
