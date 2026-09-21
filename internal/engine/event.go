// Package engine runs update jobs on a worker pool and reports through events.
// It knows nothing of the front ends.
package engine

import "time"

type EventKind int

const (
	RunStarted EventKind = iota
	Queued
	Checking
	UpToDate
	NewVersion
	Downloading
	Extracting
	// Pending: downloaded, waiting for the end-of-run system-package batch.
	Pending
	PostHooks
	Installed
	Marked // mark-current recorded the latest version without installing
	Notice
	Failed
	Skipped
	// ActionNeeded (final): root wasn't available; Message is the command to
	// run. Not a failure.
	ActionNeeded
	// Finalizing (run-level): the system-package batch is starting. A sudo
	// prompt may follow.
	Finalizing
	RunFinished
)

var kindNames = [...]string{"RunStarted", "Queued", "Checking", "UpToDate", "NewVersion", "Downloading",
	"Extracting", "Pending", "PostHooks", "Installed", "Marked", "Notice", "Failed", "Skipped",
	"ActionNeeded", "Finalizing", "RunFinished"}

func (k EventKind) String() string { return kindNames[k] }

// Event is the only way jobs talk to a front end.
type Event struct {
	AppID       string
	Name        string
	Description string
	Kind        EventKind
	Version     string // display version
	Previous    string // display version recorded before this run
	Bytes       int64  // Downloading
	Total       int64  // Downloading; -1 if unknown
	Message     string
	Err         error

	// Final marks a job's last event, which carries its buffered log.
	Final bool
	Log   []LogLine

	Summary *Summary // RunStarted (Total only) and RunFinished
}

// LogLine is one buffered debug line; Level 1 shows at -v, 2 at -vv.
type LogLine struct {
	Level int
	Text  string
}

type Summary struct {
	Total     int // jobs in the run
	Updated   int // installed (update), found new (check) or marked (mark-current)
	UpToDate  int
	Failed    int
	Skipped   int
	Action    int // downloaded, but the user has to finish the install (needs root)
	Cancelled bool
	Elapsed   time.Duration
}
