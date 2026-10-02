package runner

import (
	"slices"
	"strings"
	"testing"
)

func TestQueueMerges(t *testing.T) {
	q := newQueue()
	q.push([]string{"a.go"}, false)
	q.push([]string{"b.go"}, true)
	<-q.c
	got, ok := q.take()
	if !ok || !slices.Equal(got.files, []string{"a.go", "b.go"}) || !got.force {
		t.Fatalf("got %+v", got)
	}
	if _, ok := q.take(); ok {
		t.Fatal("queue should be empty")
	}
}

func TestSummarise(t *testing.T) {
	for in, want := range map[string]string{
		"a":     "a",
		"a,b":   "a and b",
		"a,b,c": "a and 2 more",
	} {
		if got := summarise(strings.Split(in, ",")); got != want {
			t.Errorf("%s: %q", in, got)
		}
	}
}

func TestCleanEnv(t *testing.T) {
	got := clean([]string{"LISTEN_FDS=1", "HOME=/h", "LISTEN_PIDX=2"}, "LISTEN_FDS")
	if !slices.Equal(got, []string{"HOME=/h", "LISTEN_PIDX=2"}) {
		t.Fatalf("got %q", got)
	}
}
