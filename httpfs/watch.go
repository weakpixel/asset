package httpfs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Reconnect backoff of WatchChanges: it starts at watchMinBackoff, doubles
// per failed attempt and stops at watchMaxBackoff. A connection that
// delivered an event starts over at the minimum.
const (
	watchMinBackoff = 250 * time.Millisecond
	watchMaxBackoff = 5 * time.Second
)

// WatchChanges listens to the server-sent event stream at url (a development
// server that knows when asset files change) and returns a channel of changed
// asset keys: each event whose data is {"paths": ["hero/hero.png", ...]}
// becomes one batch, in order. Pass each batch to asset.Server.Changed on the
// game goroutine for hot reload in a browser build.
//
// It reads the stream on its own goroutine with net/http (body streaming
// works on js/wasm) and never blocks the caller. When the connection drops or
// cannot be made it reconnects with a capped backoff, so a restarted dev
// server is picked up. An event that does not parse is logged and dropped.
// The goroutine runs for the life of the process and the channel is never
// closed; the buffer holds a few batches, after which the reader goroutine
// waits for the game to drain it.
func WatchChanges(url string) <-chan []string {
	return watchChanges(context.Background(), http.DefaultClient, url, watchMinBackoff, watchMaxBackoff)
}

func watchChanges(ctx context.Context, client *http.Client, url string, minBackoff, maxBackoff time.Duration) <-chan []string {
	out := make(chan []string, 16)
	go func() {
		backoff := minBackoff
		for ctx.Err() == nil {
			if streamEvents(ctx, client, url, out) {
				backoff = minBackoff
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
		}
	}()
	return out
}

// streamEvents reads one connection until it ends and reports whether it
// delivered at least one event.
func streamEvents(ctx context.Context, client *http.Client, url string, out chan<- []string) (got bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("httpfs: watch %s: %v", url, err)
		return false
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false
	}
	var data []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) == 0 {
				continue
			}
			paths, err := parseEvent(strings.Join(data, "\n"))
			data = data[:0]
			if err != nil {
				log.Printf("httpfs: watch %s: dropped event: %v", url, err)
				continue
			}
			got = true
			select {
			case out <- paths:
			case <-ctx.Done():
				return got
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// Comments (":" lines), "event:" and "id:" fields are ignored.
	}
	return got
}

// parseEvent decodes the data of one event.
func parseEvent(data string) ([]string, error) {
	var ev struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, fmt.Errorf("bad event %q: %w", data, err)
	}
	if len(ev.Paths) == 0 {
		return nil, fmt.Errorf("event %q has no paths", data)
	}
	return ev.Paths, nil
}
