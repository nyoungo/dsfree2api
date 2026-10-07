package turnstile

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// cdpEventHook, when set (tests / troubleshooting), receives every protocol
// event. Production leaves it nil and events are dropped.
var cdpEventHook func(session, method string, params json.RawMessage)

// subTargetEvent is a Target.attachedToTarget / Target.detachedFromTarget
// notification routed to a subscriber (the headless sub-frame screen fix).
type subTargetEvent struct {
	session string
	method  string
	params  json.RawMessage
}

// cdp is a minimal Chrome DevTools Protocol client: JSON request/response over
// a single WebSocket. The solver only issues calls and polls, which keeps the
// client small and race-free.
type cdp struct {
	conn    *websocket.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	seq     int64
	pending map[int64]chan cdpResult
	err     error
	subCh   chan subTargetEvent
}

type cdpResult struct {
	Result json.RawMessage
	Error  *cdpCallError
}

type cdpCallError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpCallError) Error() string {
	return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message)
}

// dialCDP opens the DevTools WebSocket (origin must match the browser host,
// otherwise Chrome rejects the handshake).
func dialCDP(ctx context.Context, wsURL, origin string) (*cdp, error) {
	cfg, err := websocket.NewConfig(wsURL, origin)
	if err != nil {
		return nil, err
	}
	conn, err := cfg.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	c := &cdp{conn: conn, pending: map[int64]chan cdpResult{}}
	go c.readLoop()
	return c, nil
}

func (c *cdp) readLoop() {
	for {
		var raw string
		if err := websocket.Message.Receive(c.conn, &raw); err != nil {
			c.fail(fmt.Errorf("cdp connection lost: %w", err))
			return
		}
		var msg struct {
			ID        int64           `json:"id"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
			Result    json.RawMessage `json:"result"`
			Error     *cdpCallError   `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			continue
		}
		if msg.ID == 0 {
			if msg.Method != "" {
				if h := cdpEventHook; h != nil {
					h(msg.SessionID, msg.Method, msg.Params)
				}
				if msg.Method == "Target.attachedToTarget" || msg.Method == "Target.detachedFromTarget" {
					c.mu.Lock()
					ch := c.subCh
					c.mu.Unlock()
					if ch != nil {
						select {
						case ch <- subTargetEvent{msg.SessionID, msg.Method, msg.Params}:
						default: // subscriber is behind; never block the read loop
						}
					}
				}
			}
			continue // events and target-scoped notifications
		}
		c.mu.Lock()
		ch, ok := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if ok {
			ch <- cdpResult{Result: msg.Result, Error: msg.Error}
		}
	}
}

func (c *cdp) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	pending := c.pending
	c.pending = map[int64]chan cdpResult{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- cdpResult{Error: &cdpCallError{Message: err.Error()}}
	}
}

// call sends one CDP command, optionally scoped to an attached target session.
func (c *cdp) call(ctx context.Context, session, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.seq++
	id := c.seq
	ch := make(chan cdpResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	buf, err := json.Marshal(msg)
	if err != nil {
		c.drop(id)
		return nil, err
	}
	c.writeMu.Lock()
	sendErr := websocket.Message.Send(c.conn, string(buf))
	c.writeMu.Unlock()
	if sendErr != nil {
		c.drop(id)
		return nil, fmt.Errorf("cdp send %s: %w", method, sendErr)
	}

	select {
	case <-ctx.Done():
		c.drop(id)
		return nil, ctx.Err()
	case res := <-ch:
		if res.Error != nil {
			return nil, res.Error
		}
		return res.Result, nil
	}
}

func (c *cdp) drop(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// setSubTargets installs (nil clears) the sub-target event sink. Install it
// before Target.setAutoAttach so no attach notification can be lost.
func (c *cdp) setSubTargets(ch chan subTargetEvent) {
	c.mu.Lock()
	c.subCh = ch
	c.mu.Unlock()
}

func (c *cdp) Close() { _ = c.conn.Close() }

type evalResult struct {
	Result struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

// evaluate runs an expression in the page and returns its JSON value. The
// 15s cap keeps a wedged browser from blocking a chat request forever.
func (c *cdp) evaluate(ctx context.Context, session, expr string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := c.call(ctx, session, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
		"userGesture":   true,
	})
	if err != nil {
		return nil, err
	}
	var out evalResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("runtime.evaluate bad json: %w", err)
	}
	if d := out.ExceptionDetails; d != nil {
		msg := d.Text
		if d.Exception != nil && d.Exception.Description != "" {
			msg = d.Exception.Description
		}
		return nil, fmt.Errorf("javascript error: %s", msg)
	}
	return out.Result.Value, nil
}
