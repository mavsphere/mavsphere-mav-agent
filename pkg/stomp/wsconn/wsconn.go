package wsconn

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-stomp/stomp"
	"github.com/gorilla/websocket"
)

// HandshakeError carries the HTTP status for failed WS upgrades (useful to decide on relogin).
type HandshakeError struct {
	Status int
	Err    error
}

func (e *HandshakeError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("websocket handshake failed: status=%d err=%v", e.Status, e.Err)
	}
	return fmt.Sprintf("websocket handshake failed: %v", e.Err)
}
func (e *HandshakeError) Unwrap() error { return e.Err }

// WSConn adapts a Gorilla WebSocket to an io.ReadWriteCloser with streaming semantics.
type WSConn struct {
	*websocket.Conn
	pending bytes.Buffer

	// Gorilla requires a single writer at a time.
	writeMu      sync.Mutex
	writeTimeout time.Duration

	doneOnce sync.Once
	doneCh   chan struct{}
}

func NewWSConn(wsURL, token, mavId string) (*WSConn, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}

	headers := http.Header{}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if mavId != "" {
		headers.Set("mavId", mavId)
	}

	d := *websocket.DefaultDialer
	d.HandshakeTimeout = 15 * time.Second
	d.EnableCompression = true
	// If you use self-signed certs in dev; otherwise remove:
	d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	// Many Spring STOMP setups require the subprotocol
	d.Subprotocols = []string{"v12.stomp"}

	wsConn, resp, err := d.Dial(u.String(), headers)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, &HandshakeError{Status: status, Err: err}
	}

	w := &WSConn{
		Conn:         wsConn,
		writeTimeout: 15 * time.Second, // default; matches your MsgSendTimeout
		doneCh:       make(chan struct{}),
	}

	// Transport keepalive (WS ping/pong)
	_ = wsConn.SetReadDeadline(time.Now().Add(60 * time.Second))
	wsConn.SetPongHandler(func(string) error {
		return wsConn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	// Ensure background ping loop stops on close
	wsConn.SetCloseHandler(func(code int, text string) error {
		w.signalDone()
		// use default close behavior
		return nil
	})

	go w.pingLoop(25*time.Second, 5*time.Second)

	return w, nil
}

func (w *WSConn) pingLoop(every time.Duration, writeBudget time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-w.doneCh:
			return
		case <-t.C:
			// Control writes also need serialization
			w.writeMu.Lock()
			_ = w.SetWriteDeadline(time.Now().Add(writeBudget))
			_ = w.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(writeBudget))
			w.writeMu.Unlock()
		}
	}
}

func (w *WSConn) signalDone() {
	w.doneOnce.Do(func() { close(w.doneCh) })
}

// Read implements streaming semantics over message-framed WebSockets.
func (w *WSConn) Read(b []byte) (int, error) {
	for w.pending.Len() == 0 {
		mt, msg, err := w.ReadMessage()
		if err != nil {
			return 0, err
		}
		// STOMP can be text or binary; accept both.
		if mt == websocket.TextMessage || mt == websocket.BinaryMessage {
			w.pending.Write(msg)
			break
		}
		// Ignore non-data frames and keep reading.
	}
	return w.pending.Read(b)
}

func (w *WSConn) Write(b []byte) (int, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	if w.writeTimeout > 0 {
		_ = w.SetWriteDeadline(time.Now().Add(w.writeTimeout))
	}
	if err := w.WriteMessage(websocket.TextMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *WSConn) Close() error {
	w.signalDone()
	return w.Conn.Close()
}

// DialStompOverWebSocket dials WS, then performs STOMP CONNECT with sane options.
func DialStompOverWebSocket(wsURL, token, mavId string) (*stomp.Conn, error) {
	ws, err := NewWSConn(wsURL, token, mavId)
	if err != nil {
		return nil, err
	}

	opts := []func(*stomp.Conn) error{
		stomp.ConnOpt.AcceptVersion(stomp.V12),
		stomp.ConnOpt.Host("/"),
		stomp.ConnOpt.HeartBeat(10*time.Second, 10*time.Second),
		stomp.ConnOpt.HeartBeatGracePeriodMultiplier(3.0),
		stomp.ConnOpt.HeartBeatError(35 * time.Second),

		// Keep this, but note: it does not guarantee underlying WS write won’t block
		// unless the transport enforces deadlines (which WSConn.Write now does).
		stomp.ConnOpt.MsgSendTimeout(15 * time.Second),

		stomp.ConnOpt.ReadChannelCapacity(32),
		stomp.ConnOpt.WriteChannelCapacity(32),
		stomp.ConnOpt.ReadBufferSize(64 * 1024),
		stomp.ConnOpt.WriteBufferSize(64 * 1024),
	}

	// Add auth/identity to STOMP CONNECT (some backends check here too)
	if token != "" {
		opts = append(opts, stomp.ConnOpt.Header("Authorization", "Bearer "+token))
	}
	if mavId != "" {
		opts = append(opts, stomp.ConnOpt.Header("mavId", mavId))
	}

	return stomp.Connect(ws, opts...)
}
