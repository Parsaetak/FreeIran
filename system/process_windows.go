//go:build windows

package system

import (
	"context"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

func execLookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func executableName(name string) string {
	return name + ".exe"
}

func platformNote() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// Windows process-creation flags. Defined as constants so the intent
// of every flag is documented at the point of use.
const (
	// CREATE_NO_WINDOW: the child gets no console at all. This is the
	// flag the upstream Xray/v2rayN/sing-box launchers use to keep the
	// user desktop clean when a protocol core is spawned.
	createNoWindow = 0x08000000

	// CREATE_NEW_PROCESS_GROUP: the child is the root of a new process
	// group so a Ctrl-Break / group-wide event never bleeds into
	// sibling FreeIran subsystems.
	createNewProcessGroup = 0x00000200

	// DETACHED_PROCESS: belt-and-braces so a forked console child
	// cannot attach to FreeIran's window station.
	detachedProcess = 0x00000008

	// CREATE_BREAKAWAY_FROM_JOB: the child is created OUTSIDE any job
	// the parent belongs to. Used only in the retry tier of the job
	// binding strategy, for environments (pre-Windows-8 semantics or
	// non-nestable job hierarchies) where assigning an already-jobbed
	// child to our job is denied.
	createBreakawayFromJob = 0x01000000

	// TH32CS_SNAPPROCESS for CreateToolhelp32Snapshot.
	snapshotProcesses = 0x00000002

	statusStillActive = 0x00000103
)

var (
	procGetExitCodeProcess   = kernel32.NewProc("GetExitCodeProcess")
	procCreateToolhelp32Snap = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW      = kernel32.NewProc("Process32FirstW")
	procProcess32NextW       = kernel32.NewProc("Process32NextW")
	procTerminateProcess     = kernel32.NewProc("TerminateProcess")
)

// launchProcess starts a managed process with stdout/stderr captured
// WITHOUT opening a visible CMD/console window, then guarantees the
// supervision invariant — a protocol core must never outlive FreeIran
// — through a three-tier binding strategy:
//
//	tier 1  spawn + AssignProcessToJobObject (Windows 8+ nests jobs,
//	        so this succeeds even when the parent — e.g. a GitHub
//	        Actions runner agent — already runs inside a job).
//	tier 2  on ERROR_ACCESS_DENIED only: terminate the child, respawn
//	        once with CREATE_BREAKAWAY_FROM_JOB (born outside the
//	        enclosing job), assign again.
//	tier 3  supervised fallback: the launch SUCCEEDS without a job;
//	        Stop() performs a deterministic process-TREE termination
//	        (Toolhelp32 snapshot walk), and the degradation is never
//	        silent — it is logged, exposed through Diagnostics() and
//	        JobBound(), and counted in metrics.
//
// The job itself is configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
// so even an abnormal FreeIran death (crash, Task Manager kill,
// runner cancellation) makes the kernel reap every core process and
// its descendants.
func launchProcess(ctx context.Context, spec ProcessSpec) (*ManagedProcess, error) {
	if parentInJob() {
		logging.D("system", "launch",
			"parent process already runs inside a Windows job; child-job binding will nest")
	}

	cmd, err := startChild(ctx, spec, 0)
	if err != nil {
		return nil, launchError(spec, err)
	}

	job, bindErr := createAndBindJob(cmd.Process.Pid)
	if bindErr == nil {
		return supervise(ctx, spec, cmd, job, true, "")
	}

	// Retry tier: only when assignment was DENIED for job-hierarchy
	// reasons AND the child is still alive (a child that already
	// exited carries no orphan risk and must keep its real exit
	// status instead of being respawned).
	if isAssignRestricted(bindErr) && childAlive(cmd.Process.Pid) {
		logging.W("system", "job_bind",
			"direct job assignment denied (%v); retrying with CREATE_BREAKAWAY_FROM_JOB", bindErr)

		killAndWait(cmd)

		retry, retryErr := startChild(ctx, spec, createBreakawayFromJob)
		if retryErr == nil {
			retryJob, retryBind := createAndBindJob(retry.Process.Pid)
			if retryBind == nil {
				return supervise(ctx, spec, retry, retryJob, true, "")
			}

			// Breakaway spawn worked but assignment still failed:
			// keep this child under supervised fallback.
			return supervise(ctx, spec, retry, nil, false, retryBind.Error())
		}

		// Breakaway itself not permitted: final fallback with a
		// normally-created child.
		cmd2, err2 := startChild(ctx, spec, 0)
		if err2 != nil {
			return nil, launchError(spec, err2)
		}

		return supervise(ctx, spec, cmd2, nil, false, bindErr.Error())
	}

	// Non-restricted failure (job creation denied, or the child died
	// before assignment — e.g. an injected fail-fast core): the child
	// either is already dead or cannot leak; supervise it without a
	// job and surface the degradation.
	return supervise(ctx, spec, cmd, nil, false, bindErr.Error())
}

// startChild builds and starts one exec.Cmd with the standard
// no-window creation flags plus any strategy-specific extra flag.
// Output capture: when ProcessSpec carries Stdout/Stderr writers the
// child's streams are connected through exec's own anonymous pipes;
// the pipes are created without inheriting any console handle.
// cmd.Wait waits for the copy goroutines, so the writers are complete
// once the exited channel closes — that is the deterministic flush
// contract the tests rely on (no sleeps).
func startChild(ctx context.Context, spec ProcessSpec, extraFlags uint32) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir
	cmd.Stdout = orDiscard(spec.Stdout)
	cmd.Stderr = orDiscard(spec.Stderr)

	// Bound the time I/O pipes may stay open after process exit or
	// context cancellation, so Wait can never hang on a stuck pipe.
	cmd.WaitDelay = pipeDrainDelay

	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | createNewProcessGroup | detachedProcess | extraFlags,
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return cmd, nil
}

// createAndBindJob allocates a kill-on-close job and binds pid to it.
func createAndBindJob(pid int) (*jobHandle, error) {
	job, err := newJob()
	if err != nil {
		return nil, err
	}

	if err := job.assign(pid); err != nil {
		job.close()
		return nil, err
	}

	return job, nil
}

// childAlive reports whether pid is still running (used to decide
// whether a binding failure is worth a breakaway retry).
func childAlive(pid int) bool {
	raw, _, _ := procOpenProcess.Call(uintptr(processQueryLimited), 0, uintptr(pid))
	if raw == 0 {
		return false
	}

	defer closeHandle(syscall.Handle(raw))

	var exitCode uint32

	ok, _, _ := procGetExitCodeProcess.Call(
		uintptr(raw), uintptr(unsafe.Pointer(&exitCode)))

	return ok != 0 && exitCode == statusStillActive
}

// killAndWait terminates one unbound child and waits for its full
// reaping (process exit + pipe copy goroutines joined).
func killAndWait(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// processEntry32W is PROCESSENTRY32W: the Toolhelp32 snapshot row.
// Declared explicitly (rather than through unsafe raw offsets) so the
// Go compiler lays it out with the exact Windows ABI alignment.
type processEntry32W struct {
	Size              uint32
	Usage             uint32
	ProcessID         uint32
	DefaultHeapID     uintptr
	ModuleID          uint32
	Threads           uint32
	ParentProcessID   uint32
	PriorityClassBase int32
	Flags             uint32
	ExeFile           [260]uint16
}

// processTreeIDs returns pid plus every living descendant,
// descendants first (post-order), using a Toolhelp32 snapshot. It
// backs both the supervised-fallback kill path and the lifecycle
// tests that must verify no descendant survived.
func processTreeIDs(pid int) []int {
	snapRaw, _, _ := procCreateToolhelp32Snap.Call(uintptr(snapshotProcesses), 0)
	if snapRaw == 0 {
		return []int{pid}
	}

	snap := syscall.Handle(snapRaw)
	defer closeHandle(snap)

	children := make(map[int][]int, 64)

	var entry processEntry32W
	entry.Size = uint32(unsafe.Sizeof(entry))

	handle := uintptr(snap)
	ptr := uintptr(unsafe.Pointer(&entry))

	ok, _, _ := procProcess32FirstW.Call(handle, ptr)
	for ok != 0 {
		children[int(entry.ParentProcessID)] = append(
			children[int(entry.ParentProcessID)], int(entry.ProcessID))

		ok, _, _ = procProcess32NextW.Call(handle, ptr)
	}

	// Post-order collect: descendants before their parents. Cycles
	// (pid reuse races) are bounded by a visited set.
	visited := make(map[int]bool, len(children)+1)
	var order []int

	var walk func(int)
	walk = func(parent int) {
		if visited[parent] {
			return
		}
		visited[parent] = true

		for _, child := range children[parent] {
			walk(child)
		}

		if parent != pid {
			order = append(order, parent)
		}
	}

	walk(pid)
	order = append(order, pid)

	return order
}

// killProcessTree deterministically terminates pid and EVERY
// descendant (deepest first, then the ancestors). This is the
// supervised-fallback cleanup path when job binding is unavailable;
// it leaves no grandchild behind.
func killProcessTree(pid int) {
	for _, p := range processTreeIDs(pid) {
		terminateByPID(p)
	}
}

// terminateByPID opens PROCESS_TERMINATE and terminates one process.
// Failures (already-dead pids) are ignored: the goal is coverage of
// every living descendant, not precise accounting.
func terminateByPID(pid int) {
	raw, _, _ := procOpenProcess.Call(uintptr(processTerminate), 0, uintptr(pid))
	if raw == 0 {
		return
	}

	defer closeHandle(syscall.Handle(raw))

	_, _, _ = procTerminateProcess.Call(uintptr(raw), uintptr(jobExitCode))
}

// reapDescendants enforces the no-descendant-survives invariant after
// the direct child has exited (any exit path). When the kernel job
// binding is active the job-handle close that follows (kill-on-close)
// already guarantees it, so this is a no-op; in supervised fallback
// mode the Toolhelp32 tree kill reaps every descendant explicitly.
func reapDescendants(m *ManagedProcess) {
	if m == nil || m.pid <= 0 {
		return
	}

	m.mu.Lock()
	bound := m.jobBound
	m.mu.Unlock()

	if bound {
		return // closeJob's kill-on-close handles the domain.
	}

	killProcessTree(m.pid)
}

// politeShutdown has no equivalent for a windowless Windows child:
// there is no signal channel to a process created with
// CREATE_NO_WINDOW (console control events require a shared console,
// which the launcher deliberately avoids). The grace period is
// therefore a wait-for-natural-exit window, exactly as documented —
// the hard-kill phase provides the deterministic guarantee.
func politeShutdown(m *ManagedProcess) {}

// hardKill terminates the whole supervision domain in one step:
// TerminateJobObject when the kernel job binding is active (kills
// every member — the child plus any descendants — atomically), or
// the Toolhelp32 process-tree walk in supervised fallback mode.
func hardKill(m *ManagedProcess) {
	if m == nil || m.pid <= 0 {
		return
	}

	m.mu.Lock()
	job := m.job
	bound := m.jobBound
	m.mu.Unlock()

	if bound && job != nil {
		if err := job.terminate(); err != nil {
			logging.W("system", "job_terminate",
				"TerminateJobObject failed for pid %d, falling back to tree kill: %v",
				m.pid, err)

			killProcessTree(m.pid)
		}

		return
	}

	killProcessTree(m.pid)
}
