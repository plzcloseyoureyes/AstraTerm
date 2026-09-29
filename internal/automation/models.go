package automation

import "time"

// JSON contract of the automation module (mirrored in web/src/features/automation/types.ts). Snippet and Macro are
// the core model types (SPEC §5.2).

// Script is a saved JavaScript automation script (AUTO-10).
type Script struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Content     string    `json:"content"`
	OwnerID     string    `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// TriggerScope limits a trigger to some sessions; an empty scope matches every session of the owner.
type TriggerScope struct {
	ConnectionIDs []string `json:"connectionIds,omitempty"`
	Protocols     []string `json:"protocols,omitempty"`
	Tags          []string `json:"tags,omitempty"`
}

// Trigger action types. highlight is evaluated by the browser (keyword highlighter); every other action runs in the
// backend so triggers also work with no browser attached. notify and sound are delivered as events.
const (
	ActHighlight = "highlight"
	ActNotify    = "notify"
	ActSound     = "sound"
	ActSend      = "send"
	ActLog       = "log"
	ActScript    = "runScript"
	ActSnippet   = "runSnippet"
)

// TriggerAction is one action of a trigger. Only the fields of its type are used.
type TriggerAction struct {
	Type string `json:"type"`
	// highlight
	Color      string `json:"color,omitempty"`
	Background string `json:"background,omitempty"`
	Underline  bool   `json:"underline,omitempty"`
	// notify
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
	Level   string `json:"level,omitempty"`
	Desktop bool   `json:"desktop,omitempty"`
	// sound
	Sound string `json:"sound,omitempty"`
	// send: text (C escapes such as \r \t \x03 are decoded) or a stored secret of the session's connection
	Text   string `json:"text,omitempty"`
	Secret string `json:"secret,omitempty"`
	Enter  bool   `json:"enter,omitempty"`
	// runScript / runSnippet
	ScriptID  string `json:"scriptId,omitempty"`
	SnippetID string `json:"snippetId,omitempty"`
}

// Trigger events: "output" (default) matches Pattern against every line of output; the others fire on session
// events, Pattern (optional) then matches the event text (the command line for "command", the session title for
// "connect", the state message for "disconnect").
const (
	EventOutput     = "output"
	EventConnect    = "connect"
	EventDisconnect = "disconnect"
	EventCommand    = "command" // a command finished (OSC 133 D; needs shell integration)
)

// Exit-code filters of "command" triggers.
const (
	ExitAny   = "any"
	ExitOK    = "ok"
	ExitError = "error"
)

// Trigger is a regex over the ANSI-stripped output of the owner's sessions — or a session event — that fires
// actions (AUTO-7).
type Trigger struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Enabled       bool            `json:"enabled"`
	Event         string          `json:"event"`
	Exit          string          `json:"exit,omitempty"`           // command: any | ok | error
	MinDuration   int             `json:"minDurationSec,omitempty"` // command: only commands that ran at least this long
	Pattern       string          `json:"pattern"`
	CaseSensitive bool            `json:"caseSensitive"`
	Scope         TriggerScope    `json:"scope"`
	Actions       []TriggerAction `json:"actions"`
	CooldownMs    int             `json:"cooldownMs"`
	Once          bool            `json:"once"`
	SortOrder     int             `json:"sortOrder"`
	Stats         TriggerStats    `json:"stats"`
	OwnerID       string          `json:"-"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// TriggerStats are runtime counters (not persisted; reset on restart).
type TriggerStats struct {
	Hits      int64      `json:"hits"`
	LastHitAt *time.Time `json:"lastHitAt,omitempty"`
}

// TriggerLogEntry is one line captured by a trigger's "log" action.
type TriggerLogEntry struct {
	ID           int64     `json:"id"`
	TriggerID    string    `json:"triggerId"`
	TriggerName  string    `json:"triggerName"`
	SessionID    string    `json:"sessionId,omitempty"`
	SessionTitle string    `json:"sessionTitle,omitempty"`
	ConnectionID string    `json:"connectionId,omitempty"`
	Line         string    `json:"line"`
	TS           time.Time `json:"ts"`
}

// Batch / schedule action kinds.
const (
	KindCommand = "command"
	KindSnippet = "snippet"
	KindScript  = "script"
)

// Execution modes of batch commands.
const (
	ModeAuto    = "auto"    // exec for SSH connections, an interactive session otherwise
	ModeExec    = "exec"    // SSH exec channel (clean stdout/stderr + exit code)
	ModeSession = "session" // a terminal session: type the command, capture the output until the prompt returns
)

// JobAction describes what a batch run or a scheduled task executes on each connection.
type JobAction struct {
	Kind        string            `json:"kind"`
	Command     string            `json:"command,omitempty"`
	SnippetID   string            `json:"snippetId,omitempty"`
	ScriptID    string            `json:"scriptId,omitempty"`
	Variables   map[string]string `json:"variables,omitempty"`
	Mode        string            `json:"mode,omitempty"`
	TimeoutSec  int               `json:"timeoutSec,omitempty"`
	Parallel    int               `json:"parallel,omitempty"`
	StopOnError bool              `json:"stopOnError,omitempty"`
}

// Schedule notification policies.
const (
	NotifyNever   = "never"
	NotifyFailure = "failure"
	NotifyAlways  = "always"
)

// Schedule is a cron-scheduled task (AUTO-12).
type Schedule struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	Spec          string     `json:"spec"`
	Action        JobAction  `json:"action"`
	ConnectionIDs []string   `json:"connectionIds"`
	Notify        string     `json:"notify"`
	LastRunAt     *time.Time `json:"lastRunAt,omitempty"`
	LastStatus    string     `json:"lastStatus,omitempty"`
	LastRunID     string     `json:"lastRunId,omitempty"`
	NextRunAt     *time.Time `json:"nextRunAt,omitempty"`
	OwnerID       string     `json:"-"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// Run kinds, origins and statuses.
const (
	RunScript = "script"
	RunBatch  = "batch"

	OriginManual   = "manual"
	OriginSchedule = "schedule"
	OriginTrigger  = "trigger"

	StatusRunning  = "running"
	StatusOK       = "ok"
	StatusError    = "error"
	StatusCanceled = "canceled"
	StatusSkipped  = "skipped"
	StatusPending  = "pending"
)

// RunSummary counts per-host outcomes of a run.
type RunSummary struct {
	Total   int `json:"total"`
	OK      int `json:"ok"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// HostResult is the outcome of a batch run on one connection.
type HostResult struct {
	ConnectionID string `json:"connectionId"`
	Name         string `json:"name"`
	Host         string `json:"host,omitempty"`
	Status       string `json:"status"`
	Mode         string `json:"mode,omitempty"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	Output       string `json:"output,omitempty"`
	Error        string `json:"error,omitempty"`
	DurationMs   int64  `json:"durationMs"`
}

// Run is one entry of the run history (script runs, batch runs, scheduled tasks).
type Run struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind"`
	RefID      string       `json:"refId,omitempty"` // the schedule for scheduled tasks, else the script (script runs)
	Name       string       `json:"name"`
	Origin     string       `json:"origin"`
	Target     string       `json:"target,omitempty"`
	Status     string       `json:"status"`
	JobID      string       `json:"jobId,omitempty"`
	Error      string       `json:"error,omitempty"`
	Log        string       `json:"log,omitempty"`
	Results    []HostResult `json:"results,omitempty"`
	Summary    RunSummary   `json:"summary"`
	OwnerID    string       `json:"-"`
	StartedAt  time.Time    `json:"startedAt"`
	FinishedAt *time.Time   `json:"finishedAt,omitempty"`
}

// LogonAction is one step of connection.options.logonActions (AUTO-8): wait for Expect (a regular expression over
// the ANSI-stripped output), then type Send or the stored secret named Secret (followed by Enter unless Enter is
// false).
type LogonAction struct {
	Expect     string `json:"expect,omitempty"`
	Send       string `json:"send,omitempty"`
	Secret     string `json:"secret,omitempty"`
	Enter      *bool  `json:"enter,omitempty"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
	Optional   bool   `json:"optional,omitempty"`
	DelayMs    int    `json:"delayMs,omitempty"`
}

// MacroStep is one step of a macro (AUTO-1). It extends the core model.MacroStep {data, delayMs} (SPEC §5.2) with
// optional fields stored in the same JSON column of the core macros table:
//
//	waitFor    wait (after delayMs) until this RE2 pattern appears in the session's ANSI-stripped output
//	timeoutMs  how long waitFor may wait (default 30 s, max 10 min); a timeout stops the replay on that session
//	secret     type this stored secret of the session's connection (never saved in the macro) before data
type MacroStep struct {
	Data      string `json:"data"`
	DelayMs   int    `json:"delayMs"`
	WaitFor   string `json:"waitFor,omitempty"`
	TimeoutMs int    `json:"timeoutMs,omitempty"`
	Secret    string `json:"secret,omitempty"`
}

// Macro is a recorded / edited sequence of steps (core macros table, extended steps).
type Macro struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Steps     []MacroStep `json:"steps"`
	OwnerID   string      `json:"-"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// DangerMatch is one hit of the dangerous-command guard (SEC-21).
type DangerMatch struct {
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Line     string `json:"line"`
}

// Limits.
const (
	maxNameLen        = 200
	maxDescriptionLen = 4000
	maxSnippetBytes   = 256 << 10
	maxScriptBytes    = 256 << 10
	maxSendBytes      = 1 << 20
	maxTags           = 32
	maxTagLen         = 64
	maxMacroSteps     = 10000
	maxMacroStepBytes = 64 << 10
	maxMacroBytes     = 1 << 20
	maxMacroDelayMs   = 10 * 60 * 1000
	maxTargets        = 256
	maxTriggers       = 200
	maxTriggerActions = 16
	maxSchedules      = 100
	maxScripts        = 500
	maxLogonActions   = 32
	maxRunLog         = 256 << 10
	maxHostOutput     = 256 << 10
	maxRunsPerUser    = 500
	maxTriggerLog     = 1000
)
