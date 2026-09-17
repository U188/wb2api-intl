package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"
)

var errRestartPending = errors.New("restart already pending")
var errRestartInvalidConfig = errors.New("restart preflight: configuration is invalid")

// controller reserves a restart before validation, so concurrent requests
// cannot schedule multiple shutdowns. Inject validationFn only before serving.
type controller struct {
	pending      atomic.Bool
	validationFn func() error
	cancel       func()
	delay        time.Duration
}

func newRestartController(configPath string, cancel func()) *controller {
	return &controller{
		validationFn: func() error { _, err := Load(configPath); return err },
		cancel:       cancel,
		delay:        250 * time.Millisecond,
	}
}

func (c *controller) Request() error {
	if !c.pending.CompareAndSwap(false, true) {
		return errRestartPending
	}
	if c.validationFn != nil {
		if err := c.validationFn(); err != nil {
			c.pending.Store(false)
			// Do not expose parser errors: config values can contain credentials.
			return errRestartInvalidConfig
		}
	}
	time.AfterFunc(c.delay, func() {
		if c.cancel != nil {
			c.cancel()
		}
	})
	return nil
}
func (c *controller) Pending() bool { return c.pending.Load() }

// StatusCode allows the panel adapter to map duplicate requests to HTTP 409.
// A successful Request must be answered with HTTP 202 before delayed shutdown.
func restartRequestStatus(err error) int {
	if err == nil {
		return http.StatusAccepted
	}
	if errors.Is(err, errRestartPending) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// shutdownHTTP bounds the graceful drain, then force-closes active connections
// (including streaming requests). Background cleanup and flushes follow this.
func shutdownHTTP(srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	closeErr := srv.Close()
	if err != nil {
		return err
	}
	return closeErr
}
