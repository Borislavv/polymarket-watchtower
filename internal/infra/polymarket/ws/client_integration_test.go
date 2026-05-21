package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeWSServer is a minimal in-process echo + scripted WS server
// used by the integration tests below. It records the subscribe
// payload + lets the test push canned messages.
type fakeWSServer struct {
	t            *testing.T
	srv          *httptest.Server
	mu           sync.Mutex
	subscribeRaw [][]byte
	conns        []*websocket.Conn
	upgrader     websocket.Upgrader
	onConn       func(*websocket.Conn)
}

func newFakeServer(t *testing.T, onConn func(*websocket.Conn)) *fakeWSServer {
	t.Helper()
	f := &fakeWSServer{t: t, upgrader: websocket.Upgrader{}, onConn: onConn}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeWSServer) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conns = append(f.conns, conn)
	f.mu.Unlock()
	// Read the subscribe message (the first frame the client sends).
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.subscribeRaw = append(f.subscribeRaw, append([]byte(nil), raw...))
	f.mu.Unlock()
	if f.onConn != nil {
		f.onConn(conn)
	}
	// Drain client → server (PING etc) until the connection drops.
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

// wsURL returns the ws://… form of the test server's URL.
func (f *fakeWSServer) wsURL() string {
	u, _ := url.Parse(f.srv.URL)
	return "ws://" + u.Host + "/"
}

func (f *fakeWSServer) close() { f.srv.Close() }

// TestClient_ConnectSubscribeReceive pins the happy path: client
// dials, sends subscribe payload, receives a book message, and the
// decoded Event lands in the output channel.
func TestClient_ConnectSubscribeReceive(t *testing.T) {
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(bookSample))
	})
	defer srv.close()

	c := New(Config{
		Endpoint:     srv.wsURL(),
		PingInterval: 100 * time.Millisecond,
		ReadTimeout:  5 * time.Second,
		EventBuffer:  16,
	}, nil, nil)
	c.Subscribe(SubscriptionSet{Markets: []MarketSubscription{{
		ConditionID:    "0xbd31dc8a20211944f6b70f31557f1001557b59905b7738480ca09bd4532f84af",
		CLOBTokenIDs:   []string{"65818619657568813474341868652308942079804919287380422192892211131408793125422"},
		EventSlug:      "tx",
		OutcomeByToken: map[string]string{"65818619657568813474341868652308942079804919287380422192892211131408793125422": "Yes"},
	}}})
	out := make(chan Event, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_ = c.Run(ctx, out)
	}()

	select {
	case ev := <-out:
		if ev.Type != EventTypeBook {
			t.Errorf("type: got %q want book", ev.Type)
		}
		if ev.EventSlug != "tx" {
			t.Errorf("event_slug: got %q", ev.EventSlug)
		}
		if ev.Outcome != "Yes" {
			t.Errorf("outcome: got %q", ev.Outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received")
	}

	srv.mu.Lock()
	got := string(srv.subscribeRaw[0])
	srv.mu.Unlock()
	if !strings.Contains(got, `"type":"market"`) {
		t.Errorf("subscribe payload missing type:market — got %s", got)
	}
	if !strings.Contains(got, `"assets_ids"`) {
		t.Errorf("subscribe payload missing assets_ids — got %s", got)
	}
}

// TestClient_DropsWhenBufferFull pins the load-bearing backpressure
// rule: with EventBuffer=1, a flood of messages must drop excess +
// stay connected.
func TestClient_DropsWhenBufferFull(t *testing.T) {
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		for i := 0; i < 50; i++ {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(bookSample))
			time.Sleep(5 * time.Millisecond)
		}
	})
	defer srv.close()

	c := New(Config{
		Endpoint:    srv.wsURL(),
		EventBuffer: 1,
		ReadTimeout: 2 * time.Second,
	}, nil, nil)
	c.Subscribe(SubscriptionSet{Markets: []MarketSubscription{{
		ConditionID:  "x",
		CLOBTokenIDs: []string{"t1"},
	}}})
	out := make(chan Event, 1) // intentionally tiny
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = c.Run(ctx, out)
		close(done)
	}()
	// Consume only the first event; everything else should drop.
	select {
	case <-out:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("no event received at all")
	}
	<-done
}

// TestClient_PongHeartbeatKeepsConnectionAlive — the server sends
// "PONG" text frames; the client must treat them as heartbeats
// (EventTypeHeartbeat) and keep reading.
func TestClient_PongHeartbeatKeepsConnectionAlive(t *testing.T) {
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("PONG"))
		time.Sleep(100 * time.Millisecond)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(bookSample))
	})
	defer srv.close()
	c := New(Config{Endpoint: srv.wsURL(), EventBuffer: 16, ReadTimeout: 2 * time.Second}, nil, nil)
	c.Subscribe(SubscriptionSet{Markets: []MarketSubscription{{ConditionID: "x", CLOBTokenIDs: []string{"t1"}}}})
	out := make(chan Event, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx, out) }()
	gotHeartbeat, gotBook := false, false
	for i := 0; i < 2; i++ {
		select {
		case ev := <-out:
			if ev.Type == EventTypeHeartbeat {
				gotHeartbeat = true
			}
			if ev.Type == EventTypeBook {
				gotBook = true
			}
		case <-time.After(2 * time.Second):
			break
		}
	}
	if !gotHeartbeat {
		t.Error("expected at least one heartbeat event from the PONG frame")
	}
	if !gotBook {
		t.Error("expected the book event after the heartbeat")
	}
}

// TestClient_NoSubscriptionsErrors pins the safety belt: starting
// the client with an empty subscription set must NOT silently dial
// the upstream and burn a connection.
func TestClient_NoSubscriptionsErrors(t *testing.T) {
	c := New(Config{Endpoint: "ws://localhost:0"}, nil, nil)
	err := c.Run(context.Background(), make(chan Event, 1))
	if err == nil || !strings.Contains(err.Error(), "no subscriptions") {
		t.Errorf("expected no-subscriptions error; got %v", err)
	}
}
