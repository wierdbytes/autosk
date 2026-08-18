package rpcclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Notification is one server→client project notification frame. Params stays
// raw so each consumer only decodes the notification kinds it understands.
type Notification struct {
	Method string
	Params json.RawMessage
}

type noteSubscription struct {
	subscribe   string
	unsubscribe string
}

// NoteStream is an acknowledged notification tail over one dedicated
// connection. Close sends the matching unsubscribe requests and closes the
// connection; Errors reports a daemon disconnect or malformed stream.
type NoteStream struct {
	events       <-chan Notification
	errs         <-chan error
	conn         net.Conn
	selector     map[string]any
	unsubscribes []string
	allowed      map[string]struct{}
	closeOnce    sync.Once
	closed       chan struct{}
}

func (s *NoteStream) Events() <-chan Notification { return s.events }
func (s *NoteStream) Errors() <-chan error        { return s.errs }

// Close terminates the subscription. It is safe to call repeatedly.
func (s *NoteStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		enc := json.NewEncoder(s.conn)
		for _, method := range s.unsubscribes {
			_ = enc.Encode(rpcRequest{ID: 0, Method: method, Params: s.selector})
		}
		close(s.closed)
		_ = s.conn.Close()
	})
	return nil
}

// Subscribe opens the broad project notification stream used by the TUI.
func (c *Client) Subscribe(ctx context.Context) (*NoteStream, error) {
	return c.subscribeNotifications(ctx, []noteSubscription{
		{subscribe: "task.subscribe", unsubscribe: "task.unsubscribe"},
		{subscribe: "project.subscribe", unsubscribe: "project.unsubscribe"},
		{subscribe: "session.subscribeProject", unsubscribe: "session.unsubscribeProject"},
	}, "task-changed", "project-changed", "session-changed", "registry-changed")
}

// SubscribeTaskProgress opens only the existing task and session lifecycle
// channels needed by `autosk watch`. The daemon receives no new watch-specific
// RPC or subscription state.
func (c *Client) SubscribeTaskProgress(ctx context.Context) (*NoteStream, error) {
	return c.subscribeNotifications(ctx, []noteSubscription{
		{subscribe: "task.subscribe", unsubscribe: "task.unsubscribe"},
		{subscribe: "session.subscribeProject", unsubscribe: "session.unsubscribeProject"},
	}, "task-changed", "session-changed")
}

type rpcNoteFrame struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Error  *RPCError       `json:"error"`
}

// subscribeNotifications generalises the persistent subscription pattern used
// by task/project/session notifications. It waits for every acknowledgement and
// buffers notifications received during the handshake. A caller can therefore
// fetch its opening snapshot after this returns without losing an intervening
// change.
func (c *Client) subscribeNotifications(
	ctx context.Context,
	subscriptions []noteSubscription,
	methods ...string,
) (*NoteStream, error) {
	conn, err := c.conn.Dial(ctx)
	if err != nil {
		return nil, err
	}
	selector := c.selector(nil)
	pending := make(map[uint64]string, len(subscriptions))
	enc := json.NewEncoder(conn)
	for _, sub := range subscriptions {
		id := c.id.Add(1)
		pending[id] = sub.subscribe
		if err := enc.Encode(rpcRequest{ID: id, Method: sub.subscribe, Params: selector}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("autoskd %s: write: %w", sub.subscribe, err)
		}
	}

	allowed := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		allowed[method] = struct{}{}
	}
	dec := json.NewDecoder(conn)
	buffered := make([]Notification, 0)
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-handshakeDone:
		}
	}()
	defer close(handshakeDone)

	for len(pending) > 0 {
		var frame rpcNoteFrame
		if err := dec.Decode(&frame); err != nil {
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("autoskd notification subscribe: read acknowledgement: %w", err)
		}
		if _, ok := allowed[frame.Method]; ok {
			buffered = append(buffered, Notification{Method: frame.Method, Params: frame.Params})
			continue
		}
		method, ok := pending[frame.ID]
		if frame.Method != "" || !ok {
			continue
		}
		if frame.Error != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("autoskd %s: %w", method, frame.Error)
		}
		delete(pending, frame.ID)
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}

	eventCh := make(chan Notification, max(64, len(buffered)))
	errCh := make(chan error, 1)
	unsubscribes := make([]string, 0, len(subscriptions))
	for _, sub := range subscriptions {
		unsubscribes = append(unsubscribes, sub.unsubscribe)
	}
	stream := &NoteStream{
		events:       eventCh,
		errs:         errCh,
		conn:         conn,
		selector:     selector,
		unsubscribes: unsubscribes,
		allowed:      allowed,
		closed:       make(chan struct{}),
	}
	for _, note := range buffered {
		eventCh <- note
	}
	go stream.readLoop(dec, eventCh, errCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = stream.Close()
		case <-stream.closed:
		}
	}()
	return stream, nil
}

func (s *NoteStream) readLoop(dec *json.Decoder, events chan<- Notification, errs chan<- error) {
	defer close(events)
	defer close(errs)
	defer func() { _ = s.Close() }()
	for {
		var frame rpcNoteFrame
		if err := dec.Decode(&frame); err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			if errors.Is(err, io.EOF) {
				err = errors.New("lost connection to autoskd")
			} else {
				err = fmt.Errorf("autoskd notification stream: %w", err)
			}
			select {
			case errs <- err:
			case <-s.closed:
			}
			return
		}
		if _, ok := s.allowed[frame.Method]; !ok {
			continue
		}
		select {
		case events <- Notification{Method: frame.Method, Params: frame.Params}:
		case <-s.closed:
			return
		}
	}
}
