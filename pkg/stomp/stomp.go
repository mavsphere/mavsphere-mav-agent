package stomp

import (
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type StompClient struct {
	conn   *websocket.Conn
	mavId  string
	sendCh chan string
	doneCh chan struct{}
}

func NewStompClient(backendURL, token, mavId string) (*StompClient, error) {
	u, err := url.Parse(backendURL)
	if err != nil {
		return nil, fmt.Errorf("invalid backend URL: %w", err)
	}
	if !strings.Contains(u.RawQuery, "token=") {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += "token=" + url.QueryEscape(token)
	}
	wsURL := u.String()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("WebSocket dial failed: %w", err)
	}

	client := &StompClient{
		conn:   conn,
		mavId:  mavId,
		sendCh: make(chan string, 10),
		doneCh: make(chan struct{}),
	}

	go client.writeLoop()

	if err := client.sendConnect(); err != nil {
		return nil, err
	}

	return client, nil
}

func (c *StompClient) sendConnect() error {
	frame := "CONNECT\naccept-version:1.2\nheart-beat:10000,10000\n\n\x00"
	return c.conn.WriteMessage(websocket.TextMessage, []byte(frame))
}

func (c *StompClient) writeLoop() {
	for {
		select {
		case msg := <-c.sendCh:
			err := c.conn.WriteMessage(websocket.TextMessage, []byte(msg))
			if err != nil {
				log.Printf("[STOMP] Write error: %v", err)
			}
		case <-c.doneCh:
			return
		}
	}
}

func (c *StompClient) SendHeartbeat(capabilities []string) {
	payload := fmt.Sprintf(`{"mavId":"%s","capabilities":[%s],"timestamp":%d}`,
		c.mavId,
		`"`+strings.Join(capabilities, `","`)+`"`,
		time.Now().UnixMilli(),
	)
	frame := fmt.Sprintf("SEND\ndestination:/app/agent/%s/heartbeat\ncontent-type:application/json\n\n%s\x00", c.mavId, payload)
	c.sendCh <- frame
}

func (c *StompClient) Close() {
	close(c.doneCh)
	_ = c.conn.Close()
}
