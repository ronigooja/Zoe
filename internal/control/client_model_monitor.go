package control

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"github.com/ronigooja/Zoe/internal/modelmonitor"
)

var monitorID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type monitorSubscription struct {
	id     string
	source *modelmonitor.Subscription
	cancel context.CancelFunc
	done   chan struct{}
}

func (c *clientSocket) stopMonitor() {
	if c.monitor == nil {
		return
	}
	m := c.monitor
	c.monitor = nil
	m.source.Close()
	m.cancel()
	<-m.done
}

func (c *clientSocket) monitorWrite(value any) error {
	ctx, cancel := context.WithTimeout(c.ctx, time.Second)
	defer cancel()
	return c.write(ctx, value)
}

func (c *clientSocket) handleMonitorFrame(data []byte) {
	if c.monitor != nil {
		select {
		case <-c.monitor.done:
			c.stopMonitor()
		default:
		}
	}
	var f struct {
		Type string `json:"type"`
		ID   string `json:"subscription_id"`
	}
	if json.Unmarshal(data, &f) != nil {
		return
	}
	errorFrame := func(code string) {
		_ = c.monitorWrite(map[string]any{"channel": "model_monitor", "type": "error", "subscription_id": f.ID, "error": map[string]string{"code": code}})
	}
	if !monitorID.MatchString(f.ID) {
		f.ID = ""
		errorFrame("invalid_subscription_id")
		return
	}
	// Instance bearer authentication constructs exactly this owner principal.
	if c.owner != "owner" || c.device != "instance" {
		errorFrame("forbidden")
		return
	}
	switch f.Type {
	case "subscribe":
		if c.monitor != nil {
			errorFrame("already_subscribed")
			return
		}
		source := modelmonitor.Subscribe()
		ctx, cancel := context.WithCancel(c.ctx)
		m := &monitorSubscription{id: f.ID, source: source, cancel: cancel, done: make(chan struct{})}
		c.monitor = m
		if c.monitorWrite(map[string]any{"channel": "model_monitor", "type": "subscribed", "subscription_id": f.ID}) != nil {
			source.Close()
			cancel()
			close(m.done)
			c.monitor = nil
			return
		}
		go c.runMonitor(ctx, m)
	case "unsubscribe":
		if c.monitor == nil || c.monitor.id != f.ID {
			errorFrame("not_subscribed")
			return
		}
		c.stopMonitor()
		_ = c.monitorWrite(map[string]any{"channel": "model_monitor", "type": "unsubscribed", "subscription_id": f.ID})
	default:
		errorFrame("invalid_type")
	}
}

func (c *clientSocket) runMonitor(ctx context.Context, m *monitorSubscription) {
	defer close(m.done)
	defer m.source.Close()
	var sequence uint64
	gap := func() {
		if c.monitorWrite(map[string]any{"channel": "model_monitor", "type": "gap", "subscription_id": m.id}) != nil {
			c.cancel()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.source.Gap:
			gap()
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-m.source.Gap:
			gap()
			return
		case event := <-m.source.Events:
			sequence++
			err := c.monitorWrite(map[string]any{"channel": "model_monitor", "type": "event", "subscription_id": m.id, "sequence": sequence, "event": event})
			if err == errClientFrameTooLarge {
				gap()
				return
			}
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}
