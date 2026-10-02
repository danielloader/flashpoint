// Package event is what the runner tells a front end: log lines and status.
package event

import (
	"fmt"
	"time"
)

// Source says which process a line is about.
type Source int

const (
	API Source = iota
	Web
	System
)

func (s Source) String() string {
	switch s {
	case API:
		return "api"
	case Web:
		return "web"
	}
	return "flashpoint"
}

// Kind separates a child's own output from what flashpoint says about it.
type Kind int

const (
	Output     Kind = iota // a line the child wrote
	Info                   // flashpoint's own note, e.g. "ready in 1.2s"
	Error                  // flashpoint's note about a failure
	Diagnostic             // compiler output from a failed build
	Marker                 // "--- build #3 started": for log files, not screens
)

// Line is one line of log.
type Line struct {
	Seq    uint64
	Time   time.Time
	Source Source
	Kind   Kind
	Text   string
}

// APIState is the Go server's state.
type APIState int

const (
	APIStarting    APIState = iota
	APIBuilding             // a build is running; the old server, if any, still serves
	APIReady                // serving
	APIOffline              // not running, e.g. stopped or not answering
	APICrashed              // exited on its own
	APIBuildFailed          // the last build failed
)

func (s APIState) String() string {
	return [...]string{"starting", "building", "ready", "offline", "crashed", "build failed"}[s]
}

// WebState is the dev server's state.
type WebState int

const (
	WebNone WebState = iota // no web app
	WebStarting
	WebReady
	WebDown
)

func (s WebState) String() string {
	return [...]string{"none", "starting", "ready", "down"}[s]
}

// Status is a snapshot of both processes.
type Status struct {
	API       APIState
	APIDetail string // the error summary for a failed build or a crash
	// Serving is true while some build is answering requests, which a
	// failed build does not change.
	Serving bool
	// Watching is false when rebuilds happen only on request.
	Watching  bool
	LastBuild time.Duration
	LastReady time.Duration // save to serving, for the last restart
	// ErrorSeq is the first line of the last failed build's output.
	ErrorSeq uint64

	Web       WebState
	WebDetail string

	APIURL string
	WebURL string

	// ProbeDials counts the connections flashpoint has opened to check
	// readiness, for GET /status; it should stay flat while idle.
	ProbeDials int64
}

// Sink receives events. Its methods may be called from any goroutine.
type Sink interface {
	Line(Line)
	Status(Status)
}

// Seconds formats a duration the way the log lines do: 1.4s, or 85ms.
func Seconds(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
