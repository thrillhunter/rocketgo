package rocket

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient() *Client {
	return &Client{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		closed: make(chan struct{}),
	}
}

func TestAddHandlerDispatch(t *testing.T) {
	client := newTestClient()
	received := make(chan *MessageEvent, 1)

	client.AddHandler(func(c *Client, event *MessageEvent) {
		received <- event
	})

	client.dispatchEvent(&MessageEvent{Message: &Message{Text: "hello"}})

	select {
	case event := <-received:
		if event == nil || event.Message == nil || event.Message.Text != "hello" {
			t.Fatalf("unexpected event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for handler")
	}
}

func TestAddHandlerOnce(t *testing.T) {
	client := newTestClient()
	var calls atomic.Int32

	client.AddHandlerOnce(func(c *Client, event *MessageEvent) {
		calls.Add(1)
	})

	client.dispatchEvent(&MessageEvent{Message: &Message{Text: "first"}})
	client.dispatchEvent(&MessageEvent{Message: &Message{Text: "second"}})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected one call, got %d", calls.Load())
}

func TestSyncEvents(t *testing.T) {
	client := newTestClient()
	client.SyncEvents = true

	called := false
	client.AddHandler(func(c *Client, event *MessageEvent) {
		called = true
	})

	client.dispatchEvent(&MessageEvent{Message: &Message{Text: "sync"}})

	if !called {
		t.Fatal("expected synchronous handler to run before dispatch returns")
	}
}
