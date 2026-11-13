package httpfs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan []string) []string {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no batch")
		return nil
	}
}

func TestWatchChangesBatchesInOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": hello\n\n")
		fmt.Fprint(w, "data: {\"paths\":[\"a.png\",\"b/c.kage\"]}\n\n")
		fmt.Fprint(w, "data: not json\n\n")
		fmt.Fprint(w, "data: {\"paths\":[]}\n\n")
		fmt.Fprint(w, "data: {\"paths\":[\"d.png\"]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := watchChanges(ctx, srv.Client(), srv.URL, time.Millisecond, 5*time.Millisecond)
	if got := recv(t, ch); !reflect.DeepEqual(got, []string{"a.png", "b/c.kage"}) {
		t.Fatalf("first batch %v", got)
	}
	if got := recv(t, ch); !reflect.DeepEqual(got, []string{"d.png"}) {
		t.Fatalf("second batch %v (malformed events must be dropped)", got)
	}
}

func TestWatchChangesReconnects(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := conns.Add(1)
		if n == 1 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "data: {\"paths\":[\"conn%d\"]}\n\n", n)
		// Returning ends the stream; the client must reconnect.
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := watchChanges(ctx, srv.Client(), srv.URL, time.Millisecond, 5*time.Millisecond)
	if got := recv(t, ch); !reflect.DeepEqual(got, []string{"conn2"}) {
		t.Fatalf("after a failed connect: %v", got)
	}
	if got := recv(t, ch); !reflect.DeepEqual(got, []string{"conn3"}) {
		t.Fatalf("after a dropped stream: %v", got)
	}
}
