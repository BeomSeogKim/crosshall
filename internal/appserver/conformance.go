//go:build conformance

package appserver

import (
	"encoding/json"
	"fmt"
	"time"
)

func (c *Client) ConformanceDeadline(deadline time.Time) error {
	return c.conn.SetDeadline(deadline)
}

func (c *Client) ConformanceStart(method string, params any) (int, error) {
	if method != "thread/start" && method != "turn/start" {
		return 0, fmt.Errorf("%w: %s", ErrNotAllowed, method)
	}
	c.next++
	return int(c.next), c.writeJSON(map[string]any{"id": c.next, "method": method, "params": params})
}

func (c *Client) ConformanceRead(message any) error {
	payload, err := c.readMessage()
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, message)
}
