//go:build !windows

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestListenUnix(t *testing.T) {
	dir, err := os.MkdirTemp("", "fbr") // short: socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "api.sock")
	ln, err := listenUnix(path, 0o660)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o660 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket file: %v %v", fi.Mode(), err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })}
	go srv.Serve(ln)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	resp, err := client.Get("http://unix/books")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" {
		t.Errorf("got %q", b)
	}
	srv.Close()

	// a socket left behind is replaced; another file is not
	if _, err := os.Stat(path); err != nil {
		// net.UnixListener removes its file on Close: leave one behind
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		l.Close()
	}
	ln, err = listenUnix(path, 0o600)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	ln.Close()
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, []byte("data"), 0o600)
	if _, err := listenUnix(plain, 0o660); err == nil {
		t.Error("a regular file was replaced")
	}
	if b, _ := os.ReadFile(plain); string(b) != "data" {
		t.Error("the regular file changed")
	}
}
