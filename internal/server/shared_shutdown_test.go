package server

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSharedMainProcessHelper(t *testing.T) {
	if os.Getenv("GOFER_SHARED_MAIN_PROCESS") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := RunShared(ctx); err != nil {
		os.Stderr.WriteString("Gofer startup or shutdown failed: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSharedMainProcessDrainsOnSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM acceptance requires a Unix host")
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_ADDR", address)
	t.Setenv("GOFER_BASE_URL", "http://"+address)
	t.Setenv("GOFER_AUTH_MODE", "open")
	t.Setenv("GOFER_DB_PATH", filepath.Join(t.TempDir(), "gofer.db"))
	t.Setenv("GOFER_SHUTDOWN_TIMEOUT", "5s")
	t.Setenv("GOFER_SHARED_MAIN_PROCESS", "1")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestSharedMainProcessHelper$")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = command.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			command.Process.Kill()
			<-done
		}
	})

	listening := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "listening on ") {
				listening <- struct{}{}
				return
			}
		}
	}()
	select {
	case <-listening:
	case <-time.After(60 * time.Second):
		t.Fatalf("server did not listen; stderr:\n%s", stderr.String())
	}

	// The banner prints before the listener opens, so poll until it answers.
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("healthz = %s before signal", response.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthz never answered: %v; stderr:\n%s", err, stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Hold an SSE stream open like a browser tab would; shutdown must not wait on it.
	streamRequest, err := http.NewRequest(http.MethodGet, "http://"+address+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamRequest.Header.Set("Accept", "text/event-stream")
	streamRequest.Header.Set("Origin", "http://"+address)
	stream, err := (&http.Client{}).Do(streamRequest)
	if err != nil {
		t.Fatalf("open event stream: %v; stderr:\n%s", err, stderr.String())
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("event stream = %s before signal", stream.Status)
	}

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("server did not exit after SIGTERM")
	}
	if waitErr != nil {
		t.Fatalf("server exited with %v; stderr:\n%s", waitErr, stderr.String())
	}
}
