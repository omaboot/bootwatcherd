package main

import (
	"strconv"
	"time"
)

// Endpoint is one of bootwatchd's endpoints the page has a row for.
type Endpoint struct {
	Path string // "/v1/echo"
	What string // "nonce back, round trip"
}

// endpoints are checked every minute and shown in this order.
var endpoints = []Endpoint{
	{"/v1/echo", "nonce back, round trip"},
	{"/v1/stream", "seeded bytes down"},
	{"/v1/sink", "seeded bytes up"},
}

// State is one endpoint's result in one check.
type State int

const (
	NoData State = iota // the control check failed too: the checker's own network, not boot.watch
	Good
	Failed
)

// Result is one endpoint's outcome in one check.
type Result struct {
	State  State
	RTT    time.Duration // the request's round trip (good results)
	Family string        // "4" or "6": the address family the request went over, when known
	Err    string        // why it failed
}

// Check is one minute's check of every endpoint.
type Check struct {
	At      time.Time
	Results map[string]Result // by endpoint path
	Health  *Health           // bootwatchd's counters, when /v1/health answered
}

// Health is what the page uses from bootwatchd's /v1/health.
type Health struct {
	DayBytes      int64     `json:"day_bytes"`
	DailyBytes    int64     `json:"daily_bytes"`
	Since         time.Time `json:"since,omitzero"`           // when bootwatchd started: a change is a restart
	DayBytesSince time.Time `json:"day_bytes_since,omitzero"` // when day_bytes started counting (bootwatchd v0.2.0 on)
}

// Colour is a day bar's colour (and the headline mark's).
type Colour string

const (
	Teal  Colour = "teal"  // every check good
	Amber Colour = "amber" // some failed (under 5%), or slow
	Red   Colour = "red"   // 5% or more failed
	Grey  Colour = "grey"  // no checks
)

// Day is one endpoint's UTC day.
type Day struct {
	Date     string // "2026-10-13"
	Good     int
	Failed   int
	MedianMS int // the good checks' median round trip
	P95MS    int // the good checks' 95th percentile round trip (nearest rank)
}

// slowP95MS is a day's 95th percentile round trip above which the day is amber: one good check
// in 20 slower than this (owner, 2026-10-17; the median over 1 s it replaces never tripped).
const slowP95MS = 300

// Colour is the day's bar by the strict rules (owner, 2026-10-13, 2026-10-17): teal when every
// check was good and the 95th percentile round trip at most 300 ms; red when 5% or more failed;
// amber otherwise; grey with no checks (no-data minutes count for nothing).
func (d Day) Colour() Colour {
	n := d.Good + d.Failed
	switch {
	case n == 0:
		return Grey
	case d.Failed*20 >= n:
		return Red
	case d.Failed > 0 || d.P95MS > slowP95MS:
		return Amber
	}
	return Teal
}

// days is how many day bars a row has.
const days = 60

// Row is one endpoint's line on the page.
type Row struct {
	Endpoint
	Days   []Day   // days entries, oldest first, ending today (UTC); days without checks are zero
	Uptime float64 // good / (good + failed) over Days, as a percentage; -1 with no checks
}

// View is everything the page shows.
type View struct {
	Now      time.Time
	Last     *Check // the newest check, or nil
	Rows     []Row  // one per endpoint, in endpoints' order
	Health   *Health
	Restarts []time.Time // bootwatchd's restarts seen in the days shown, oldest first
}

// staleAfter: an older last check means the checker isn't running.
const staleAfter = 3 * time.Minute

// Headline is the page's title line and its mark's colour.
func (v View) Headline() (string, Colour) {
	if v.Last == nil || v.Now.Sub(v.Last.At) > staleAfter {
		if v.Last == nil {
			return "Status unknown: no checks yet", Grey
		}
		return "Status unknown: last check " + ago(v.Now.Sub(v.Last.At)) + " ago", Grey
	}
	good, failed := 0, 0
	for _, e := range endpoints {
		switch v.Last.Results[e.Path].State {
		case Good:
			good++
		case Failed:
			failed++
		}
	}
	switch {
	case good == len(endpoints):
		return "Every endpoint answering", Teal
	case failed == len(endpoints):
		return "boot.watch not answering", Red
	case failed > 0:
		return "Some endpoints failing", Amber
	}
	return "Status unknown: the checker can't reach the internet", Grey
}

// ago is a duration for the headline: "4 min", "2 h", "3 days".
func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + " days"
}
