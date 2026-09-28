package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// errNotFound is returned for pages that don't exist; they are not retried.
var errNotFound = errors.New("not found")

// Fetcher is a polite HTTP client shared by all workers: it spaces requests
// out to at most rps per second, and when the server pushes back (429 or 5xx)
// every worker pauses, not just the one that got the error.
type Fetcher struct {
	client  *http.Client
	tick    *time.Ticker
	retries int

	mu         sync.Mutex
	pauseUntil time.Time

	Requests atomic.Int64 // completed HTTP requests, for the progress display
}

func NewFetcher(rps float64, retries int) *Fetcher {
	return &Fetcher{
		client:  &http.Client{Timeout: 30 * time.Second},
		tick:    time.NewTicker(time.Duration(float64(time.Second) / rps)),
		retries: retries,
	}
}

func (f *Fetcher) Close() { f.tick.Stop() }

// Paused reports how long the shared back-off still lasts.
func (f *Fetcher) Paused() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return max(time.Until(f.pauseUntil), 0)
}

func (f *Fetcher) pause(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if until := time.Now().Add(d); until.After(f.pauseUntil) {
		f.pauseUntil = until
	}
}

// wait blocks until this request is allowed to go out.
func (f *Fetcher) wait(ctx context.Context) error {
	for d := f.Paused(); d > 0; d = f.Paused() {
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
	select {
	case <-f.tick.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Get fetches url, retrying with exponential back-off on failures.
func (f *Fetcher) Get(ctx context.Context, url string) (string, error) {
	var lastErr error
	for attempt := range f.retries + 1 {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second // 2s, 4s, 8s, ...
			if err := sleep(ctx, backoff); err != nil {
				return "", err
			}
		}
		if err := f.wait(ctx); err != nil {
			return "", err
		}

		body, status, retryAfter, err := f.do(ctx, url)
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			lastErr = err
		case status == http.StatusOK:
			return body, nil
		case status == http.StatusNotFound:
			return "", fmt.Errorf("GET %s: %w", url, errNotFound)
		case status == http.StatusTooManyRequests || status >= 500:
			// The server is struggling: slow everyone down.
			lastErr = fmt.Errorf("GET %s: HTTP %d", url, status)
			f.pause(max(retryAfter, time.Duration(5<<attempt)*time.Second))
		default:
			return "", fmt.Errorf("GET %s: HTTP %d", url, status)
		}
	}
	return "", lastErr
}

func (f *Fetcher) do(ctx context.Context, url string) (body string, status int, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	f.Requests.Add(1)
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		retryAfter = time.Duration(secs) * time.Second
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, retryAfter, err
}
