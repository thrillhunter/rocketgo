package rocket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type ddpError struct {
	Code      any    `json:"error,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
	ErrorType string `json:"errorType,omitempty"`
}

func (e *ddpError) Error() string {
	if e == nil {
		return ""
	}
	if e.Reason != "" {
		return e.Reason
	}
	if e.Message != "" {
		return e.Message
	}
	return "rocket: ddp error"
}

type ddpInbound struct {
	Msg        string `json:"msg"`
	ID         string `json:"id,omitempty"`
	Session    string `json:"session,omitempty"`
	Collection string `json:"collection,omitempty"`
	Fields     struct {
		EventName string            `json:"eventName,omitempty"`
		Args      []json.RawMessage `json:"args,omitempty"`
	} `json:"fields,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ddpError       `json:"error,omitempty"`
	Subs   []string        `json:"subs,omitempty"`
}

type ddpCallResult struct {
	Result json.RawMessage
	Err    error
}

type ddpSubscription struct {
	Name   string
	Params []any
}

type ddpConn struct {
	client *Client

	writeMu sync.Mutex
	mu      sync.RWMutex
	ws      *websocket.Conn
	closed  bool
	seq     uint64

	pendingCalls map[string]chan ddpCallResult
	pendingSubs  map[string]chan error
	subs         map[string]ddpSubscription
}

func newDDPConn(client *Client) *ddpConn {
	return &ddpConn{
		client:       client,
		pendingCalls: make(map[string]chan ddpCallResult),
		pendingSubs:  make(map[string]chan error),
		subs:         make(map[string]ddpSubscription),
	}
}

func (d *ddpConn) connect(ctx context.Context) error {
	if err := d.dialAndHandshake(ctx); err != nil {
		return err
	}
	if err := d.login(ctx); err != nil {
		_ = d.closeCurrentSocket()
		return err
	}
	go d.readLoop()
	return nil
}

func (d *ddpConn) close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()
	d.failPending(errors.New("rocket: ddp closed"))
	return d.closeCurrentSocket()
}

func (d *ddpConn) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	payload := map[string]any{
		"msg":    "method",
		"method": method,
		"id":     d.nextID(),
		"params": params,
	}

	resultCh := make(chan ddpCallResult, 1)
	d.mu.Lock()
	d.pendingCalls[payload["id"].(string)] = resultCh
	d.mu.Unlock()

	if err := d.writeJSON(payload); err != nil {
		d.mu.Lock()
		delete(d.pendingCalls, payload["id"].(string))
		d.mu.Unlock()
		return nil, err
	}

	select {
	case result := <-resultCh:
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Result, nil
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.pendingCalls, payload["id"].(string))
		d.mu.Unlock()
		return nil, ctx.Err()
	case <-d.client.closed:
		return nil, errors.New("rocket: client closed")
	}
}

func (d *ddpConn) subscribe(ctx context.Context, name string, params ...any) error {
	key := d.subscriptionKey(name, params)

	d.mu.Lock()
	if _, exists := d.subs[key]; exists {
		d.mu.Unlock()
		return nil
	}
	d.subs[key] = ddpSubscription{Name: name, Params: append([]any(nil), params...)}
	d.mu.Unlock()

	if !d.isConnected() {
		return nil
	}
	return d.subscribeNow(ctx, name, params)
}

func (d *ddpConn) subscribeNow(ctx context.Context, name string, params []any) error {
	id := d.nextID()
	payload := map[string]any{
		"msg":    "sub",
		"id":     id,
		"name":   name,
		"params": params,
	}

	ready := make(chan error, 1)
	d.mu.Lock()
	d.pendingSubs[id] = ready
	d.mu.Unlock()

	if err := d.writeJSON(payload); err != nil {
		d.mu.Lock()
		delete(d.pendingSubs, id)
		d.mu.Unlock()
		return err
	}

	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.pendingSubs, id)
		d.mu.Unlock()
		return ctx.Err()
	case <-d.client.closed:
		return errors.New("rocket: client closed")
	}
}

func (d *ddpConn) readLoop() {
	for {
		raw, inbound, err := d.readMessage()
		if err != nil {
			if d.isClosed() {
				return
			}
			_ = d.closeCurrentSocket()
			d.failPending(err)
			if !d.client.cfg.AutoReconnect {
				d.client.handleDisconnect(err)
				return
			}
			if reconnectErr := d.reconnect(err); reconnectErr != nil {
				return
			}
			continue
		}
		d.handleInbound(raw, inbound)
	}
}

func (d *ddpConn) reconnect(cause error) error {
	d.client.handleDisconnect(cause)
	backoff := time.Second
	for {
		if d.isClosed() {
			return errors.New("rocket: client closed")
		}

		ctx, cancel := context.WithTimeout(context.Background(), defaultInternalWait)
		err := d.dialAndHandshake(ctx)
		if err == nil {
			err = d.login(ctx)
		}
		if err == nil {
			err = d.resubscribeInline(ctx)
		}
		cancel()

		if err == nil {
			d.client.emit(&ConnectEvent{Time: time.Now()})
			go d.postReconnectSync()
			return nil
		}
		d.client.emit(&ErrorEvent{Time: time.Now(), Err: err})

		select {
		case <-time.After(backoff):
		case <-d.client.closed:
			return errors.New("rocket: client closed")
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (d *ddpConn) resubscribeInline(ctx context.Context) error {
	d.mu.RLock()
	subs := make([]ddpSubscription, 0, len(d.subs))
	for _, sub := range d.subs {
		subs = append(subs, ddpSubscription{Name: sub.Name, Params: append([]any(nil), sub.Params...)})
	}
	d.mu.RUnlock()

	pending := make(map[string]struct{}, len(subs))
	for _, sub := range subs {
		id, err := d.sendInlineSubscribe(sub.Name, sub.Params...)
		if err != nil {
			return err
		}
		pending[id] = struct{}{}
	}
	return d.awaitInlineSubscriptions(ctx, pending)
}

func (d *ddpConn) dialAndHandshake(ctx context.Context) error {
	header := http.Header{}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, d.client.wsURL, header)
	if err != nil {
		return err
	}

	connectPayload := map[string]any{
		"msg":     "connect",
		"version": "1",
		"support": []string{"1", "pre2", "pre1"},
	}

	d.writeMu.Lock()
	if err := conn.WriteJSON(connectPayload); err != nil {
		d.writeMu.Unlock()
		_ = conn.Close()
		return err
	}
	d.writeMu.Unlock()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			_ = conn.Close()
			return err
		}
		var inbound ddpInbound
		if err := json.Unmarshal(data, &inbound); err != nil {
			continue
		}
		switch inbound.Msg {
		case "connected":
			d.mu.Lock()
			old := d.ws
			d.ws = conn
			d.mu.Unlock()
			if old != nil && old != conn {
				_ = old.Close()
			}
			return nil
		case "ping":
			d.writeMu.Lock()
			_ = conn.WriteJSON(map[string]string{"msg": "pong"})
			d.writeMu.Unlock()
		case "failed":
			_ = conn.Close()
			return errors.New("rocket: ddp version negotiation failed")
		}
	}
}

func (d *ddpConn) login(ctx context.Context) error {
	login := strings.TrimSpace(d.client.cfg.Login)
	password := d.client.cfg.Password
	session := d.client.Session()
	if session.AuthToken == "" {
		if d.client.cfg.AuthToken != "" && d.client.cfg.UserID != "" {
			session = Session{UserID: d.client.cfg.UserID, AuthToken: d.client.cfg.AuthToken, UpdatedAt: time.Now()}
		}
	}

	type loginAttempt struct {
		ResumeToken string
		Params      []any
	}
	attempts := make([]loginAttempt, 0, 2)
	if session.AuthToken != "" {
		attempts = append(attempts, loginAttempt{
			ResumeToken: session.AuthToken,
			Params:      []any{map[string]string{"resume": session.AuthToken}},
		})
	}
	if login != "" && password != "" {
		userField := "username"
		if strings.Contains(login, "@") {
			userField = "email"
		}
		digest := sha256.Sum256([]byte(password))
		attempts = append(attempts, loginAttempt{Params: []any{map[string]any{
			"user": map[string]string{
				userField: login,
			},
			"password": map[string]string{
				"digest":    hex.EncodeToString(digest[:]),
				"algorithm": "sha-256",
			},
		}}})
	}
	if len(attempts) == 0 {
		return errors.New("rocket: either auth token or login/password is required")
	}

	var lastErr error
	for _, attempt := range attempts {
		result, err := d.callInline(ctx, "login", attempt.Params...)
		if err != nil {
			lastErr = err
			if attempt.ResumeToken != "" {
				d.client.setSession(Session{})
			}
			continue
		}

		var resp struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal(result, &resp); err != nil {
			lastErr = err
			continue
		}
		if resp.ID == "" {
			lastErr = errors.New("rocket: login returned an empty user id")
			continue
		}
		if resp.Token == "" {
			resp.Token = attempt.ResumeToken
		}
		d.client.setSession(Session{UserID: resp.ID, AuthToken: resp.Token, UpdatedAt: time.Now()})
		return nil
	}
	return lastErr
}

func (d *ddpConn) handleInbound(raw json.RawMessage, inbound *ddpInbound) {
	switch inbound.Msg {
	case "ping":
		_ = d.writeJSON(map[string]string{"msg": "pong"})
	case "result":
		if inbound.ID == "" {
			return
		}
		d.mu.Lock()
		ch, ok := d.pendingCalls[inbound.ID]
		if ok {
			delete(d.pendingCalls, inbound.ID)
		}
		d.mu.Unlock()
		if ok {
			if inbound.Error != nil {
				ch <- ddpCallResult{Err: inbound.Error}
			} else {
				ch <- ddpCallResult{Result: cloneRaw(inbound.Result)}
			}
		}
	case "ready":
		d.mu.Lock()
		for _, id := range inbound.Subs {
			if ch, ok := d.pendingSubs[id]; ok {
				delete(d.pendingSubs, id)
				ch <- nil
			}
		}
		d.mu.Unlock()
	case "nosub":
		if inbound.ID == "" {
			return
		}
		d.mu.Lock()
		if ch, ok := d.pendingSubs[inbound.ID]; ok {
			delete(d.pendingSubs, inbound.ID)
			if inbound.Error != nil {
				ch <- inbound.Error
			} else {
				ch <- errors.New("rocket: subscription closed")
			}
		}
		d.mu.Unlock()
	case "changed", "added", "removed":
		go d.client.handleDDPCollection(cloneRaw(raw), inbound)
	}
}

func (d *ddpConn) readMessage() (json.RawMessage, *ddpInbound, error) {
	d.mu.RLock()
	ws := d.ws
	d.mu.RUnlock()
	if ws == nil {
		return nil, nil, errors.New("rocket: ddp socket is not connected")
	}
	_, data, err := ws.ReadMessage()
	if err != nil {
		return nil, nil, err
	}
	var inbound ddpInbound
	if err := json.Unmarshal(data, &inbound); err != nil {
		return nil, nil, err
	}
	return cloneRaw(data), &inbound, nil
}

func (d *ddpConn) callInline(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	id := d.nextID()
	payload := map[string]any{
		"msg":    "method",
		"method": method,
		"id":     id,
		"params": params,
	}
	if err := d.writeJSON(payload); err != nil {
		return nil, err
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-d.client.closed:
			return nil, errors.New("rocket: client closed")
		default:
		}

		raw, inbound, err := d.readMessage()
		if err != nil {
			return nil, err
		}
		if inbound.Msg == "result" && inbound.ID == id {
			if inbound.Error != nil {
				return nil, inbound.Error
			}
			return cloneRaw(inbound.Result), nil
		}
		d.handleInlineInbound(raw, inbound)
	}
}

func (d *ddpConn) sendInlineSubscribe(name string, params ...any) (string, error) {
	id := d.nextID()
	payload := map[string]any{
		"msg":    "sub",
		"id":     id,
		"name":   name,
		"params": params,
	}
	if err := d.writeJSON(payload); err != nil {
		return "", err
	}
	return id, nil
}

func (d *ddpConn) awaitInlineSubscriptions(ctx context.Context, pending map[string]struct{}) error {
	for {
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.client.closed:
			return errors.New("rocket: client closed")
		default:
		}

		raw, inbound, err := d.readMessage()
		if err != nil {
			return err
		}
		switch inbound.Msg {
		case "ready":
			for _, subID := range inbound.Subs {
				delete(pending, subID)
			}
		case "nosub":
			if _, ok := pending[inbound.ID]; ok {
				if inbound.Error != nil {
					return inbound.Error
				}
				return errors.New("rocket: subscription closed")
			}
		}
		d.handleInlineInbound(raw, inbound)
	}
}

func (d *ddpConn) handleInlineInbound(raw json.RawMessage, inbound *ddpInbound) {
	if inbound == nil {
		return
	}
	switch inbound.Msg {
	case "ping":
		_ = d.writeJSON(map[string]string{"msg": "pong"})
	default:
		d.handleInbound(raw, inbound)
	}
}

func (d *ddpConn) writeJSON(payload any) error {
	d.mu.RLock()
	ws := d.ws
	d.mu.RUnlock()
	if ws == nil {
		return errors.New("rocket: ddp socket is not connected")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return ws.WriteJSON(payload)
}

func (d *ddpConn) postReconnectSync() {
	ctx, cancel := context.WithTimeout(context.Background(), defaultInternalWait)
	defer cancel()

	if _, err := d.client.GetRooms(ctx); err != nil {
		d.client.log.Debug("room refresh after reconnect failed", "error", err)
	}
	if _, err := d.client.GetSubscriptions(ctx); err != nil {
		d.client.log.Debug("subscription refresh after reconnect failed", "error", err)
	}
	if d.client.cfg.E2EE.Enabled {
		if err := d.client.e2ee.primeSubscriptions(ctx); err != nil {
			d.client.log.Debug("prime subscriptions after reconnect failed", "error", err)
		}
	}
}

func (d *ddpConn) nextID() string {
	id := atomic.AddUint64(&d.seq, 1)
	return fmt.Sprintf("go%d", id)
}

func (d *ddpConn) subscriptionKey(name string, params []any) string {
	encoded, _ := json.Marshal(params)
	return name + "|" + string(encoded)
}

func (d *ddpConn) failPending(err error) {
	d.mu.Lock()
	calls := d.pendingCalls
	subs := d.pendingSubs
	d.pendingCalls = make(map[string]chan ddpCallResult)
	d.pendingSubs = make(map[string]chan error)
	d.mu.Unlock()

	for _, ch := range calls {
		ch <- ddpCallResult{Err: err}
	}
	for _, ch := range subs {
		ch <- err
	}
}

func (d *ddpConn) isConnected() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ws != nil && !d.closed
}

func (d *ddpConn) isClosed() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.closed
}

func (d *ddpConn) closeCurrentSocket() error {
	d.mu.Lock()
	ws := d.ws
	d.ws = nil
	d.mu.Unlock()
	if ws != nil {
		return ws.Close()
	}
	return nil
}
