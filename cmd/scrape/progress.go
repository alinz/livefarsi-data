package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Progress renders a live multi-line status block on stderr: overall bars for
// shows and episodes, throughput, and one line per worker. Log messages are
// printed above the block. When stderr isn't a terminal it prints a summary
// line every few seconds instead.
type Progress struct {
	mu      sync.Mutex
	out     *os.File
	tty     bool
	fetcher *Fetcher
	start   time.Time
	lines   int // lines drawn by the last render, to move back over them

	showsTotal, showsDone, showsSkipped, showsFailed int
	showsParsed                                      int // shows whose episode list is known
	epsFound, epsDone, epsMissing                    int

	workers []workerState

	samples []sample // recent request counts, for a sliding-window rate
	rate    float64  // requests/s

	stop chan struct{}
	done chan struct{}
}

type sample struct {
	at   time.Time
	reqs int64
}

type workerState struct {
	slug        string
	done, total int
	fetchingIdx bool // fetching the show page, episode count not known yet
}

func NewProgress(fetcher *Fetcher, workers int) *Progress {
	fi, _ := os.Stderr.Stat()
	return &Progress{
		out:     os.Stderr,
		tty:     fi != nil && fi.Mode()&os.ModeCharDevice != 0,
		fetcher: fetcher,
		start:   time.Now(),
		samples: []sample{{time.Now(), fetcher.Requests.Load()}},
		workers: make([]workerState, workers),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Run redraws until Stop is called.
func (p *Progress) Run() {
	defer close(p.done)
	every := 150 * time.Millisecond
	if !p.tty {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.mu.Lock()
			p.render()
			p.mu.Unlock()
		case <-p.stop:
			p.mu.Lock()
			p.render()
			p.mu.Unlock()
			return
		}
	}
}

func (p *Progress) Stop() {
	close(p.stop)
	<-p.done
}

func (p *Progress) Logf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	fmt.Fprintf(p.out, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	if p.tty {
		p.draw()
	}
}

func (p *Progress) SetTotal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.showsTotal = n
}

func (p *Progress) StartShow(w int, slug string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workers[w] = workerState{slug: slug, fetchingIdx: true}
}

func (p *Progress) SetEpisodes(w, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workers[w].total, p.workers[w].fetchingIdx = n, false
	p.epsFound += n
	p.showsParsed++
}

func (p *Progress) EpisodeDone(w int, hasPlayer bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workers[w].done++
	p.epsDone++
	if !hasPlayer {
		p.epsMissing++
	}
}

// FinishShow marks worker w idle. status is "done", "skipped", "failed" or
// "interrupted" (cut off by Ctrl+C; not counted, it'll be redone next run).
func (p *Progress) FinishShow(w int, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workers[w] = workerState{}
	switch status {
	case "interrupted":
		return
	case "skipped":
		p.showsSkipped++
	case "failed":
		p.showsFailed++
	}
	p.showsDone++
}

// clear erases the previously drawn block so the cursor is where it started.
func (p *Progress) clear() {
	if p.tty && p.lines > 0 {
		fmt.Fprintf(p.out, "\x1b[%dF\x1b[J", p.lines)
		p.lines = 0
	}
}

func (p *Progress) render() {
	now := time.Now()
	p.samples = append(p.samples, sample{now, p.fetcher.Requests.Load()})
	for len(p.samples) > 2 && now.Sub(p.samples[1].at) >= 3*time.Second {
		p.samples = p.samples[1:]
	}
	first, last := p.samples[0], p.samples[len(p.samples)-1]
	if dt := last.at.Sub(first.at).Seconds(); dt > 0 {
		p.rate = float64(last.reqs-first.reqs) / dt
	}
	if !p.tty {
		fmt.Fprintf(p.out, "%s  shows %d/%d  episodes %d/%d  %.1f req/s  %s\n",
			now.Format("15:04:05"), p.showsDone, p.showsTotal, p.epsDone, p.epsFound, p.rate, p.eta())
		return
	}
	p.clear()
	p.draw()
}

func (p *Progress) draw() {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, "\x1b[2K"+format+"\n", args...)
		p.lines++
	}

	busy := 0
	for _, w := range p.workers {
		if w.slug != "" {
			busy++
		}
	}

	extra := []string{}
	if p.showsSkipped > 0 {
		extra = append(extra, fmt.Sprintf("%d already on disk", p.showsSkipped))
	}
	if p.showsFailed > 0 {
		extra = append(extra, fmt.Sprintf("\x1b[31m%d failed\x1b[0m", p.showsFailed))
	}
	line("")
	line("  Shows     %s %4d/%-4d %s", bar(p.showsDone, p.showsTotal, 30), p.showsDone, p.showsTotal, strings.Join(extra, ", "))
	epsTotal := p.estimatedEpisodes()
	missing := ""
	if p.epsMissing > 0 {
		missing = fmt.Sprintf(", %d without player", p.epsMissing)
	}
	line("  Episodes  %s %4d/%-4d (%d found so far%s)", bar(p.epsDone, epsTotal, 30), p.epsDone, p.epsFound, p.epsFound, missing)
	status := fmt.Sprintf("%.1f req/s", p.rate)
	if d := p.fetcher.Paused(); d > 0 {
		status = fmt.Sprintf("\x1b[33mserver pushing back, pausing %s\x1b[0m", d.Round(time.Second))
	}
	line("  %s · %d/%d workers busy · elapsed %s · %s",
		status, busy, len(p.workers), time.Since(p.start).Round(time.Second), p.eta())
	line("")
	for i, w := range p.workers {
		switch {
		case w.slug == "":
			line("  \x1b[2mw%-2d idle\x1b[0m", i+1)
		case w.fetchingIdx:
			line("  w%-2d %-26s reading episode list…", i+1, trunc(w.slug, 26))
		default:
			line("  w%-2d %-26s %s %3d/%-3d", i+1, trunc(w.slug, 26), bar(w.done, w.total, 20), w.done, w.total)
		}
	}
	fmt.Fprint(p.out, b.String())
}

// estimatedEpisodes guesses the final episode total from shows parsed so far.
func (p *Progress) estimatedEpisodes() int {
	pending := p.showsTotal - p.showsSkipped - p.showsParsed
	if p.showsParsed == 0 || pending <= 0 {
		return p.epsFound
	}
	return p.epsFound + pending*p.epsFound/p.showsParsed
}

func (p *Progress) eta() string {
	elapsed := time.Since(p.start).Seconds()
	total := p.estimatedEpisodes()
	if p.showsTotal > 0 && p.showsDone == p.showsTotal {
		return "done"
	}
	if p.epsDone < 20 || total <= p.epsDone {
		return "ETA …"
	}
	secs := float64(total-p.epsDone) / (float64(p.epsDone) / elapsed)
	return "ETA " + (time.Duration(secs) * time.Second).Round(time.Second).String()
}

func bar(done, total, width int) string {
	filled := 0
	if total > 0 {
		filled = min(done*width/total, width)
	}
	return "\x1b[32m" + strings.Repeat("█", filled) + "\x1b[2m" + strings.Repeat("░", width-filled) + "\x1b[0m"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
