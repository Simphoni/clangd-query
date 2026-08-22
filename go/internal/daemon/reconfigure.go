package daemon

import (
	"sync"
	"time"

	"clangd-query/internal/logger"
)

// Re-runs the project's generate command after source files are added or
// removed, so that build systems which compute their file lists dynamically
// (e.g. CMake projects using file(GLOB_RECURSE)) pick up the new file set
// without a manual reconfigure. Triggering is debounced over a configurable
// delay, and runs are serialized: a notification that arrives while a run is
// in flight schedules exactly one follow-up run, since a configure that
// started before the last batch of file operations cannot have observed it.
//
// After a successful run, clangd picks up the regenerated
// compile_commands.json on its own (it watches the file and reloads the
// database automatically), so the reconfigurer does not notify clangd.
type Reconfigurer struct {
	projectRoot string
	command     string
	delay       time.Duration
	logger      logger.Logger

	mu      sync.Mutex
	timer   *time.Timer
	running bool
	done    chan struct{} // closed when the current run finishes
	rerun   bool
	stopped bool
}

// Creates a reconfigurer that runs the given generate command at the project
// root. A zero delay falls back to DefaultReconfigureDelay.
func NewReconfigurer(projectRoot, command string, delay time.Duration, log logger.Logger) *Reconfigurer {
	if delay <= 0 {
		delay = DefaultReconfigureDelay
	}
	return &Reconfigurer{
		projectRoot: projectRoot,
		command:     command,
		delay:       delay,
		logger:      log,
	}
}

// Records that the set of source files changed and schedules a generate run
// once no further changes arrive within the configured delay. Cheap to call
// frequently; bursts of file operations collapse into a single run.
func (r *Reconfigurer) NotifySetChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(r.delay, r.startRun)
}

// Fires when the debounce window closes and kicks off the serialized run.
func (r *Reconfigurer) startRun() {
	r.mu.Lock()

	if r.stopped {
		r.mu.Unlock()
		return
	}
	if r.running {
		// A run is already in flight; it started before the changes this
		// timer represents, so queue one follow-up run.
		r.rerun = true
		r.mu.Unlock()
		return
	}
	r.running = true
	r.done = make(chan struct{})
	r.mu.Unlock()

	go r.run()
}

// Executes the generate command, then drains any pending rerun request
// before going idle.
func (r *Reconfigurer) run() {
	r.logger.Info("Auto-reconfiguring (source files added or removed): %s", r.command)
	if err := runGenerateCommand(r.projectRoot, r.command, r.logger); err != nil {
		// A failed reconfigure must not take the daemon down; the next
		// set change (or daemon restart) will try again.
		r.logger.Error("Auto-reconfigure failed: %v", err)
	} else {
		r.logger.Info("Auto-reconfigure finished; clangd will reload the compilation database")
	}

	r.mu.Lock()
	r.running = false
	rerun := r.rerun
	r.rerun = false
	stopped := r.stopped
	close(r.done)
	r.mu.Unlock()

	if rerun && !stopped {
		r.NotifySetChanged()
	}
}

// Prevents further scheduled runs and waits until any in-flight run has
// finished, so that a shutting-down daemon never leaves a generate process
// racing against the next daemon's startup.
func (r *Reconfigurer) Stop() {
	r.mu.Lock()
	r.stopped = true
	if r.timer != nil {
		r.timer.Stop()
	}
	for r.running {
		done := r.done
		r.mu.Unlock()
		<-done
		r.mu.Lock()
	}
	r.mu.Unlock()
}
