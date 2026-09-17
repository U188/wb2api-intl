package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	restartExitCode    = 75
	supervisedChildEnv = "WB2A_SUPERVISED_CHILD"
	// Opt out only when an external process manager is explicitly configured.
	disableSupervisorEnv = "WB2A_DISABLE_SUPERVISOR"
)

type supervisedProcess interface {
	Wait() int
	Signal(os.Signal) error
	Kill() error
}

type execSupervisedProcess struct{ cmd *exec.Cmd }

func (p *execSupervisedProcess) Wait() int {
	if err := p.cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if code := exit.ExitCode(); code >= 0 {
				return code
			}
			if status, ok := exit.Sys().(syscall.WaitStatus); ok {
				return 128 + int(status.Signal())
			}
		}
		return 1
	}
	return 0
}
func (p *execSupervisedProcess) Signal(s os.Signal) error { return p.cmd.Process.Signal(s) }
func (p *execSupervisedProcess) Kill() error              { return p.cmd.Process.Kill() }

// maybeRunSupervisor must be the first statement in main, before flag parsing
// or listener creation. The parent exits here; only the marked child runs main.
func maybeRunSupervisor() {
	if os.Getenv(supervisedChildEnv) == "1" || os.Getenv(disableSupervisorEnv) == "1" {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		log.Print("supervisor: cannot locate executable")
		os.Exit(1)
	}
	cwd, err := os.Getwd()
	if err != nil {
		log.Print("supervisor: cannot obtain working directory")
		os.Exit(1)
	}
	env := supervisorChildEnvironment(os.Environ())
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	code := runSupervisor(func() (supervisedProcess, error) {
		cmd := exec.Command(executable, os.Args[1:]...)
		cmd.Env, cmd.Dir = env, cwd
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &execSupervisedProcess{cmd: cmd}, nil
	}, signals, 500*time.Millisecond, 6*time.Second)
	signal.Stop(signals)
	os.Exit(code)
}

func supervisorChildEnvironment(env []string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, supervisedChildEnv) {
			result = append(result, entry)
		}
	}
	return append(result, supervisedChildEnv+"=1")
}

func stopSupervisedProcess(child supervisedProcess, sig os.Signal) {
	// os.Process.Signal cannot forward Interrupt on Windows. Console Ctrl+C
	// normally reaches both processes; Kill is the fallback that prevents an
	// orphan even when the event was delivered only to the parent.
	if runtime.GOOS == "windows" || child.Signal(sig) != nil {
		_ = child.Kill()
	}
}

// runSupervisor restarts only the reserved, intentional exit code. Once a stop
// signal is observed, including during the restart delay, spawning is forbidden.
func runSupervisor(spawn func() (supervisedProcess, error), signals <-chan os.Signal, restartDelay, stopTimeout time.Duration) int {
	for {
		select {
		case <-signals:
			return 0
		default:
		}
		child, err := spawn()
		if err != nil {
			log.Print("supervisor: failed to start child")
			return 1
		}
		done := make(chan int, 1)
		go func() { done <- child.Wait() }()
		var code int
		select {
		case code = <-done:
			// Prioritize a pending stop over a simultaneous restart exit.
			select {
			case <-signals:
				return 0
			default:
			}
		case sig := <-signals:
			stopSupervisedProcess(child, sig)
			timer := time.NewTimer(stopTimeout)
			select {
			case <-done:
			case <-timer.C:
				_ = child.Kill()
				<-done
			case <-signals:
				_ = child.Kill()
				<-done
			}
			timer.Stop()
			return 0
		}
		if code != restartExitCode {
			return code
		}
		timer := time.NewTimer(restartDelay)
		select {
		case <-timer.C:
		case <-signals:
			timer.Stop()
			return 0
		}
	}
}
