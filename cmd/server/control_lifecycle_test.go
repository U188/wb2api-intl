package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestartControllerDeduplicates(t *testing.T) {
	cancelled := make(chan struct{})
	var cancels atomic.Int32
	c := newRestartController("", func() { cancels.Add(1); close(cancelled) })
	c.validationFn = func() error { return nil }
	c.delay = 20 * time.Millisecond
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.Request()
			if err == nil {
				accepted.Add(1)
				if restartRequestStatus(err) != http.StatusAccepted {
					t.Error("not 202")
				}
			} else if !errors.Is(err, errRestartPending) || restartRequestStatus(err) != http.StatusConflict {
				t.Errorf("duplicate: %v", err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 || !c.Pending() {
		t.Fatal("restart was not deduplicated")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel not called")
	}
	if cancels.Load() != 1 {
		t.Fatal("multiple cancels")
	}
}

func TestRestartControllerInvalidConfigDoesNotCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"cooldown":{"soft_rate":"secret-value"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var cancels atomic.Int32
	c := newRestartController(path, func() { cancels.Add(1) })
	c.delay = time.Millisecond
	err := c.Request()
	if !errors.Is(err, errRestartInvalidConfig) || c.Pending() || restartRequestStatus(err) != http.StatusBadRequest {
		t.Fatalf("err=%v pending=%v", err, c.Pending())
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatal("config value leaked")
	}
	time.Sleep(10 * time.Millisecond)
	if cancels.Load() != 0 {
		t.Fatal("invalid config triggered cancel")
	}
	// A failed preflight releases the reservation and permits a corrected retry.
	c.validationFn = func() error { return nil }
	if err := c.Request(); err != nil {
		t.Fatal(err)
	}
}

func TestRestartControllerDelaysCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	c := newRestartController("", func() { close(cancelled) })
	c.validationFn = func() error { return nil }
	c.delay = 50 * time.Millisecond
	if err := c.Request(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
		t.Fatal("cancel occurred before response grace period")
	default:
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel not called")
	}
}

func TestShutdownHTTPNotStarted(t *testing.T) {
	if err := shutdownHTTP(&http.Server{}); err != nil {
		t.Fatal(err)
	}
}
