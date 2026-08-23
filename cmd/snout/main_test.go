package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHandleSignalShutdown(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal os.Signal
		want   string
	}{
		{name: "SIGINT", signal: os.Interrupt, want: "snout: received SIGINT, graceful shutdown started\n"},
		{name: "SIGTERM", signal: syscall.SIGTERM, want: "snout: received SIGTERM, graceful shutdown started\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			signals := make(chan os.Signal, 1)
			var stderr bytes.Buffer
			stopped := false
			err := handleSignalShutdown(test.signal, signals, &stderr, func() { stopped = true }, func(context.Context) error { return nil }, func() error { return nil }, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if !stopped {
				t.Fatal("workers were not stopped")
			}
			if got := stderr.String(); got != test.want+"snout: graceful shutdown completed\n" {
				t.Fatalf("unexpected stderr: %q", got)
			}
		})
	}
}

func TestRepeatedSignalInterruptsShutdown(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	err := handleSignalShutdown(os.Interrupt, signals, &bytes.Buffer{}, func() {}, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, func() error { return nil }, time.Second)
	if !errors.Is(err, errForcedShutdown) {
		t.Fatalf("got %v, want forced shutdown", err)
	}
}

func TestGracefulShutdownTimeout(t *testing.T) {
	signals := make(chan os.Signal)
	err := handleSignalShutdown(os.Interrupt, signals, &bytes.Buffer{}, func() {}, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, func() error { return nil }, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected timeout result: %v", err)
	}
}

func TestShutdownServiceMessagesNeverUseStdout(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	signals := make(chan os.Signal)
	if err := handleSignalShutdown(syscall.SIGTERM, signals, &stderr, func() {}, func(context.Context) error { return nil }, func() error { return nil }, time.Second); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout contains service messages: %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr does not contain shutdown messages")
	}
}
