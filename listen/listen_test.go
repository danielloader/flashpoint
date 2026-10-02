package listen

import (
	"net"
	"testing"
)

func TestListenFallsBackToBind(t *testing.T) {
	t.Setenv("LISTEN_FDS", "")
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, ok := ln.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("addr %v", ln.Addr())
	}
}

func TestInheritedRespectsListenPID(t *testing.T) {
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_PID", "1")
	if Inherited() {
		t.Fatal("LISTEN_PID for another process must not count")
	}
	t.Setenv("LISTEN_PID", "")
	if !Inherited() {
		t.Fatal("LISTEN_FDS=1 without LISTEN_PID should count")
	}
}
