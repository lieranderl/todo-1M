package natsutil

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/todo-1m/project/internal/messaging"
)

type Client struct {
	Conn *nats.Conn
	JS   nats.JetStreamContext
}

func ConnectJetStream(url string) (*Client, error) {
	conn, err := nats.Connect(url)
	if err != nil {
		return nil, err
	}
	js, err := conn.JetStream()
	if err != nil {
		_ = conn.Drain()
		conn.Close()
		return nil, err
	}
	if err := messaging.EnsureStreams(js); err != nil {
		_ = conn.Drain()
		conn.Close()
		return nil, err
	}
	return &Client{Conn: conn, JS: js}, nil
}

func ConnectJetStreamWithRetry(url string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		client, err := ConnectJetStream(url)
		if err == nil {
			return client, nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("connect jetstream timeout after %s: %w", timeout, lastErr)
}

func (c *Client) Close() {
	if c == nil || c.Conn == nil {
		return
	}
	_ = c.Conn.Drain()
	c.Conn.Close()
}

type Publisher interface {
	Publish(subject string, payload []byte) error
}

type JetStreamPublisher struct {
	JS nats.JetStreamContext
}

func (p JetStreamPublisher) Publish(subject string, payload []byte) error {
	_, err := p.JS.Publish(subject, payload)
	return err
}
