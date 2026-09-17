package main

import (
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeSupervisedProcess struct {
	exit    chan int
	stopped chan struct{}
	once    sync.Once
}

func newFakeProcess() *fakeSupervisedProcess {
	return &fakeSupervisedProcess{exit: make(chan int, 1), stopped: make(chan struct{})}
}
func (p *fakeSupervisedProcess) Wait() int { return <-p.exit }
func (p *fakeSupervisedProcess) stop() {
	p.once.Do(func() { close(p.stopped); p.exit <- restartExitCode })
}
func (p *fakeSupervisedProcess) Signal(os.Signal) error { p.stop(); return nil }
func (p *fakeSupervisedProcess) Kill() error            { p.stop(); return nil }

func TestSupervisorExitAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codes []int
		want  int
	}{
		{"normal", []int{0}, 0}, {"abnormal", []int{23}, 23}, {"restartOnce", []int{75, 0}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := runSupervisor(func() (supervisedProcess, error) {
				if calls >= len(tc.codes) {
					t.Fatal("unexpected additional spawn")
				}
				p := newFakeProcess()
				p.exit <- tc.codes[calls]
				calls++
				return p, nil
			}, make(chan os.Signal), time.Millisecond, time.Second)
			if got != tc.want || calls != len(tc.codes) {
				t.Fatalf("code=%d spawns=%d", got, calls)
			}
		})
	}
}
func TestSupervisorSpawnFailure(t *testing.T) {
	if got := runSupervisor(func() (supervisedProcess, error) { return nil, errors.New("failed") }, make(chan os.Signal), 0, time.Second); got != 1 {
		t.Fatalf("code=%d", got)
	}
}
func TestSupervisorStopDoesNotRestart(t *testing.T) {
	p := newFakeProcess()
	signals := make(chan os.Signal, 1)
	started := make(chan struct{})
	result := make(chan int, 1)
	calls := 0
	go func() {
		result <- runSupervisor(func() (supervisedProcess, error) { calls++; close(started); return p, nil }, signals, time.Millisecond, time.Second)
	}()
	<-started
	signals <- os.Interrupt
	select {
	case got := <-result:
		if got != 0 || calls != 1 {
			t.Fatalf("code=%d spawns=%d", got, calls)
		}
	case <-time.After(time.Second):
		t.Fatal("stop timed out")
	}
	select {
	case <-p.stopped:
	default:
		t.Fatal("child was not stopped")
	}
}
func TestSupervisorStopDuringRestartDelay(t *testing.T) {
	p := newFakeProcess()
	signals := make(chan os.Signal, 1)
	started := make(chan struct{})
	result := make(chan int, 1)
	calls := 0
	go func() {
		result <- runSupervisor(func() (supervisedProcess, error) { calls++; close(started); return p, nil }, signals, time.Hour, time.Second)
	}()
	<-started
	p.exit <- restartExitCode
	signals <- os.Interrupt
	select {
	case got := <-result:
		if got != 0 || calls != 1 {
			t.Fatalf("code=%d spawns=%d", got, calls)
		}
	case <-time.After(time.Second):
		t.Fatal("stop timed out")
	}
}
func TestSupervisorChildEnvironment(t *testing.T) {
	got := supervisorChildEnvironment([]string{"PATH=x", "WB2A_SUPERVISED_CHILD=0", "wb2a_supervised_child=old", "OTHER=y"})
	want := []string{"PATH=x", "OTHER=y", "WB2A_SUPERVISED_CHILD=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected environment: %v", got)
	}
}
