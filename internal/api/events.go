package api

// Live UI updates: a WebSocket feed that tells every connected browser
// "a TestCase/TestRun/AuthentikDeployment/TestCaseTemplate/TenantPreset/
// WorkloadCluster changed", so pages can refetch immediately instead of
// waiting for their own poll interval. Deliberately a thin invalidation
// signal, not a second copy of cluster state - the payload is just
// {kind, name, type}; the client already knows how to GET the real thing.
//
// Watching: one goroutine per resourceKind calls s.client.Watch (the
// management cluster's real watch endpoint, via the same client every REST
// handler uses) and fans every event out to every connected client through
// eventHub. The apiserver closes idle/long-running watches periodically -
// that is expected, not an error, so each goroutine just reconnects in a
// loop until the process is shutting down.
//
// Auth follows shell.go's own precedent exactly (see its doc comment for
// the full rationale): a browser WebSocket can't carry an Authorization
// header, so the handshake carries a single-use, short-lived ticket minted
// by an ordinary bearer-authenticated request instead.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// eventsTicketTTL bounds how long a minted ticket can wait to be
	// redeemed - the SPA opens the socket immediately after minting.
	eventsTicketTTL = 10 * time.Second
	// eventsPingInterval detects a peer that vanished without a close frame.
	eventsPingInterval = 30 * time.Second
	// eventClientBuffer is how many unread events a slow client can fall
	// behind by before new ones are dropped for it - generous given these
	// are small, infrequent invalidation signals, not a data feed.
	eventClientBuffer = 32
)

// ResourceEvent is pushed to every connected /api/v1/events/connect client.
type ResourceEvent struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Type mirrors k8s.io/apimachinery/pkg/watch.EventType: ADDED, MODIFIED,
	// DELETED (or ERROR/BOOKMARK, which callers ignore).
	Type string `json:"type"`
}

// eventHub fans ResourceEvents out to every connected WebSocket client.
type eventHub struct {
	mu      sync.Mutex
	clients map[chan ResourceEvent]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{clients: map[chan ResourceEvent]struct{}{}}
}

func (h *eventHub) subscribe() chan ResourceEvent {
	ch := make(chan ResourceEvent, eventClientBuffer)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan ResourceEvent) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

// broadcast fans ev out to every subscriber, dropping it for anyone whose
// buffer is already full rather than blocking the watch goroutine feeding
// this hub on one slow/stuck client.
func (h *eventHub) broadcast(ev ResourceEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- ev:
		default:
		}
	}
}

// eventsState mints and redeems the WebSocket's single-use connection
// tickets - the same shape as shell.go's shellTicket, minus the bound
// context (an events connection isn't scoped to anything).
type eventsState struct {
	mu      sync.Mutex
	tickets map[string]time.Time
}

func newEventsState() *eventsState {
	return &eventsState{tickets: map[string]time.Time{}}
}

func (st *eventsState) mint(now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	st.mu.Lock()
	defer st.mu.Unlock()
	for t, expires := range st.tickets {
		if now.After(expires) {
			delete(st.tickets, t)
		}
	}
	st.tickets[ticket] = now.Add(eventsTicketTTL)
	return ticket, nil
}

// redeem consumes a ticket. It is deleted whether or not it was still
// valid, so a ticket can never be used twice.
func (st *eventsState) redeem(ticket string, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	expires, ok := st.tickets[ticket]
	delete(st.tickets, ticket)
	return ok && !now.After(expires)
}

func (s *Server) createEventsSession(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	ticket, err := s.eventsTickets.mint(now)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{
		"ticket":     ticket,
		expiresAtKey: now.Add(eventsTicketTTL).UTC().Format(time.RFC3339),
	})
}

// connectEvents is registered outside requireToken, same reasoning as
// connectShell: the ticket is its credential, checked before the upgrade.
// The feed is server -> client only - any client frame (including a close)
// ends the connection, there is nothing to read for.
func (s *Server) connectEvents(w http.ResponseWriter, r *http.Request) {
	log := logf.FromContext(r.Context()).WithName("events")

	if !s.eventsTickets.redeem(r.URL.Query().Get("ticket"), time.Now()) {
		writeError(w, r, http.StatusUnauthorized, "missing, expired or already used events ticket")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.cfg.WSOriginPatterns})
	if err != nil {
		log.Info("Could not upgrade events connection", "error", err.Error())
		return
	}
	defer func() { _ = conn.CloseNow() }()

	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)

	// Not the request context: that one is cancelled the moment the handler
	// returns, and this connection is meant to outlive the handshake.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(eventsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		case <-ticker.C:
			pingCtx, pcancel := context.WithTimeout(ctx, eventsPingInterval/2)
			err := conn.Ping(pingCtx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}

// RunEventWatchers starts one watch per resourceKind, each reconnecting on
// its own until ctx is done. Call once, in a goroutine, after New.
func (s *Server) RunEventWatchers(ctx context.Context) {
	for _, k := range resourceKinds {
		go s.watchResourceKind(ctx, k)
	}
}

// watchResourceKind streams k's changes into s.events forever, reconnecting
// whenever the watch ends - the apiserver closing an idle/long-running
// watch periodically is expected, not an error.
func (s *Server) watchResourceKind(ctx context.Context, k resourceKind) {
	log := logf.FromContext(ctx).WithName("events").WithValues("kind", k.kind)
	for ctx.Err() == nil {
		list := k.newList()
		w, err := s.client.Watch(ctx, list, client.InNamespace(s.cfg.Namespace))
		if err != nil {
			log.Error(err, "watch failed, retrying")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		s.drainWatch(ctx, k, w)
		w.Stop()
	}
}

func (s *Server) drainWatch(ctx context.Context, k resourceKind, w watch.Interface) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.ResultChan():
			if !ok {
				return // Closed server-side - the caller reconnects.
			}
			// event.Object's concrete type isn't guaranteed to be
			// *unstructured.Unstructured just because k.newList() built an
			// unstructured list - confirmed live against the fake client
			// (used in tests), which hands back the scheme's own typed
			// object instead. meta.Accessor works for either.
			obj, err := apimeta.Accessor(event.Object)
			if err != nil {
				continue
			}
			s.events.broadcast(ResourceEvent{Kind: k.kind, Name: obj.GetName(), Type: string(event.Type)})
		}
	}
}
