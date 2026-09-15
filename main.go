// macmonitor is a lightweight macOS menu bar app that shows live memory and
// disk usage in the top-right status bar. It reads stats via syscalls
// (gopsutil) rather than shelling out, so it idles near 0% CPU.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/caseymrm/menuet"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

// How often to refresh each metric. Memory changes fast; disk barely moves, so
// we poll it far less often to stay cheap.
const (
	memInterval  = 2 * time.Second
	diskInterval = 30 * time.Second
	diskMount    = "/"
)

const (
	// staleAfter bounds how long a "running" marker is trusted without being
	// re-stamped. SessionEnd normally deletes a session's file; this is the
	// backstop for a session that died without running its hooks (crash, killed
	// terminal), which would otherwise pin the Mac awake forever.
	//
	// It has to stay comfortably longer than the slowest single tool call:
	// PreToolUse stamps the file when a tool starts and PostToolUse only
	// re-stamps when it finishes, so a 20-minute build legitimately leaves the
	// file untouched the whole time it runs. Erring short would drop the
	// assertion in the middle of exactly the long job it exists to protect.
	staleAfter = 30 * time.Minute

	// pruneAfter is when an abandoned session file is deleted outright, so the
	// sessions directory can't grow without bound.
	pruneAfter = 24 * time.Hour

	// enabledKey persists the toggle across restarts (NSUserDefaults).
	enabledKey = "keepAwakeEnabled"
)

// stateDir holds one file per Claude Code session, written by the hooks that
// install-hooks.sh installs: "running" while that session is working, "idle"
// when it stops, removed on SessionEnd. One file per session is what lets
// concurrent sessions (several cmux tabs) coexist — the Mac is held awake while
// ANY session is running. A single shared file would let whichever session
// finished last release the assertion out from under the others.
var stateDir = filepath.Join(os.Getenv("HOME"), ".macmonitor", "sessions")

// legacyStateFile is the original single-file state location. It's consulted
// only when no sessions directory exists yet, so upgrading the app before
// re-running install-hooks.sh still detects Claude instead of silently doing
// nothing.
var legacyStateFile = filepath.Join(os.Getenv("HOME"), ".macmonitor", "claude-state")

// stats holds the latest readings rendered into the menu bar.
type stats struct {
	memPercent  float64
	memUsed     uint64
	memTotal    uint64
	diskPercent float64
	diskUsed    uint64
	diskFree    uint64
	diskTotal   uint64
}

// mu guards current and enabled. Both are touched from two threads: the poll
// goroutine reads/writes the stats and reads the toggle, while AppKit's main
// thread flips the toggle on click and reads both when it renders the menu.
var (
	mu      sync.Mutex
	current stats
	enabled bool
)

func snapshot() stats {
	mu.Lock()
	defer mu.Unlock()
	return current
}

func isEnabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled
}

func setEnabled(v bool) {
	mu.Lock()
	enabled = v
	mu.Unlock()
	menuet.Defaults().SetBoolean(enabledKey, v)
}

// claudeRunning reports whether any Claude Code session is mid-task, per the
// per-session hook state files. Missing/unreadable files (no session, hooks not
// installed) read as idle, as do files too old to trust — see staleAfter.
func claudeRunning() bool {
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) == 0 {
		// No session files yet: either the current hooks were never installed,
		// or sessions started before an upgrade are still writing the old
		// single-file location. Falling back keeps detection working through
		// that migration; it's safe because legacyRunning applies the same
		// staleness guard, so an abandoned legacy file can't pin the Mac awake.
		return legacyRunning()
	}

	now := time.Now()
	running := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		if age > pruneAfter {
			os.Remove(filepath.Join(stateDir, e.Name()))
			continue
		}
		// Keep scanning even once we know the answer, so pruning still runs.
		if running || age > staleAfter {
			continue
		}
		b, err := os.ReadFile(filepath.Join(stateDir, e.Name()))
		if err == nil && strings.TrimSpace(string(b)) == "running" {
			running = true
		}
	}
	return running
}

// legacyRunning reads the pre-per-session state file, with the same staleness
// guard applied.
func legacyRunning() bool {
	info, err := os.Stat(legacyStateFile)
	if err != nil || time.Since(info.ModTime()) > staleAfter {
		return false
	}
	b, err := os.ReadFile(legacyStateFile)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "running"
}

// applyMode reconciles the held assertion: awake only when On and Claude is busy.
func applyMode() {
	keepAwake.ensure(isEnabled() && claudeRunning())
}

// keepAwake manages a single `caffeinate` subprocess that holds macOS power
// assertions while running. This is what keeps the Mac (and its network) alive
// during long Claude Code / cmux sessions. When the process is alive, the
// system won't idle-sleep; killing it releases the assertion immediately.
//
// caffeinate is part of macOS, so this adds no dependencies and uses the same
// IOPMAssertion mechanism Apple uses internally.
type caffeine struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

var keepAwake caffeine

// on reports whether the keep-awake assertion is currently held.
func (c *caffeine) on() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil
}

// ensure makes the held-assertion state match want, starting or stopping the
// caffeinate subprocess as needed. It's idempotent, so applyMode can call it
// every poll tick cheaply.
func (c *caffeine) ensure(want bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if want == (c.cmd != nil) {
		return // already in the desired state
	}

	if !want {
		// Release: kill the assertion-holder. We deliberately don't Wait here —
		// the reaper goroutine below owns the single permitted Wait for this
		// Cmd. exec.Cmd.Wait must be called exactly once; two callers racing on
		// it corrupts the Cmd's internal state.
		_ = c.cmd.Process.Kill()
		c.cmd = nil
		return
	}

	// Acquire. Flags:
	//   -i  prevent system idle sleep   (the important one for long jobs)
	//   -m  prevent disk from idle-sleeping
	//   -s  prevent system sleep on AC power
	//   -w  exit when OUR pid exits
	// We intentionally omit -d so the display can still dim/sleep to save power;
	// the machine keeps running. Add "-d" here if you also want the screen on.
	//
	// -w is what stops the assertion outliving the app. menuet's Quit calls
	// [NSApp terminate:], which tears the process down without running Go's
	// deferred cleanup, and a hard crash or SIGKILL skips even the signal
	// handler below. Without -w the child is reparented to launchd and keeps
	// asserting "forever", leaving the Mac unable to idle-sleep with no UI left
	// to stop it. -w makes caffeinate responsible for its own exit.
	cmd := exec.Command("caffeinate", "-i", "-m", "-s", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return
	}
	c.cmd = cmd

	// Reap the process if caffeinate ever exits on its own, so state stays honest.
	go func() {
		_ = cmd.Wait() // the sole Wait for this Cmd
		c.mu.Lock()
		if c.cmd == cmd {
			c.cmd = nil
		}
		c.mu.Unlock()
	}()
}

func main() {
	app := menuet.App()
	app.Label = "com.macmonitor.app"
	app.Children = menuItems

	// Restore the toggle from last run.
	mu.Lock()
	enabled = menuet.Defaults().Boolean(enabledKey)
	mu.Unlock()

	// Belt-and-braces cleanup for signalled shutdowns. caffeinate's -w already
	// covers the paths that skip this (NSApp terminate, SIGKILL, crash).
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		<-ch
		keepAwake.ensure(false)
		prodTunnel.stop()
		prodTunnel.wait(tunnelStopWait)
		os.Exit(0)
	}()

	// Quit from the menu skips Go's deferred cleanup, but menuet does wait on
	// this handle first: use it to close the prod tunnel so the jump host isn't
	// left running (and billing) after the app is gone.
	wg, ctx := app.GracefulShutdownHandles()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		prodTunnel.stop()
		prodTunnel.wait(tunnelStopWait)
	}()

	// Prime values immediately so the bar isn't blank on launch.
	readMemory()
	readDisk()
	applyMode()
	render()

	go pollLoop()
	go prodTunnel.refresh()

	app.RunApplication()
}

// pollLoop refreshes memory and disk on independent cadences.
func pollLoop() {
	memTick := time.NewTicker(memInterval)
	diskTick := time.NewTicker(diskInterval)
	tunnelTick := time.NewTicker(tunnelPoll)
	defer memTick.Stop()
	defer diskTick.Stop()
	defer tunnelTick.Stop()

	wasAwake := keepAwake.on()
	for {
		select {
		case <-memTick.C:
			readMemory()
			applyMode() // re-check Claude's state every tick when On
			render()
			// Refresh the dropdown's live status line if it's open when Claude
			// starts or stops working.
			if now := keepAwake.on(); now != wasAwake {
				wasAwake = now
				menuItemsChanged()
			}
		case <-diskTick.C:
			readDisk()
			render()
		case <-tunnelTick.C:
			go prodTunnel.refresh() // AWS calls take ~1s; keep them off the tick
		}
	}
}

func readMemory() {
	if v, err := mem.VirtualMemory(); err == nil {
		mu.Lock()
		current.memPercent = v.UsedPercent
		current.memUsed = v.Used
		current.memTotal = v.Total
		mu.Unlock()
	}
}

func readDisk() {
	if u, err := disk.Usage(diskMount); err == nil {
		mu.Lock()
		current.diskPercent = u.UsedPercent
		current.diskUsed = u.Used
		current.diskFree = u.Free
		current.diskTotal = u.Total
		mu.Unlock()
	}
}

const gb = 1024 * 1024 * 1024

// menuItemsChanged refreshes the dropdown if it's currently open.
func menuItemsChanged() {
	menuet.App().MenuChanged()
}

// render pushes the compact two-line title into the menu bar (GB, number only):
//
//	m:9☕️   (used memory, top line; keep-awake marker)
//	s:234🔌 (remaining storage, bottom line; prod-tunnel marker)
func render() {
	s := snapshot()
	memUsedGB := float64(s.memUsed) / gb
	diskFreeGB := float64(s.diskFree) / gb
	// Top-bar detection cue: ☕️ when On and Claude is actively running (Mac held
	// awake), a hollow dot when On but watching an idle Claude, nothing when Off.
	mark := ""
	switch {
	case keepAwake.on():
		mark = "☕️"
	case isEnabled():
		mark = "◦"
	}
	// Second line carries the prod-tunnel cue: 🔌 tunnel up, … starting/stopping,
	// ⚠️ failed, ● host running with no tunnel (still billing), nothing when off.
	title := fmt.Sprintf("m:%.0f%s\ns:%.0f%s", memUsedGB, mark, diskFreeGB, prodTunnel.view().mark())
	menuet.App().SetMenuState(&menuet.MenuState{Title: title})
}

// menuItems builds the dropdown shown when the title is clicked.
func menuItems() []menuet.MenuItem {
	s := snapshot()
	on := isEnabled()

	// Single On/Off toggle. On = keep awake while Claude Code is running.
	toggle := menuet.MenuItem{
		Text:  "Keep Awake while Claude runs",
		State: on,
		Clicked: func() {
			setEnabled(!isEnabled())
			applyMode()
			render()
			menuet.App().MenuChanged()
		},
	}

	// Live detection line so you can see what the watcher currently sees.
	var status string
	switch {
	case !on:
		status = "Off"
	case keepAwake.on():
		status = "Claude running — awake ☕️"
	default:
		status = "Claude idle — letting it sleep"
	}

	items := []menuet.MenuItem{
		toggle,
		{Text: "  " + status},
		{Type: menuet.Separator},
	}
	items = append(items, tunnelItems()...)
	items = append(items,
		menuet.MenuItem{Type: menuet.Separator},
		menuet.MenuItem{
			Text: fmt.Sprintf("Memory: %.1f%% used", s.memPercent),
		},
		menuet.MenuItem{
			Text: fmt.Sprintf("  %s of %s", humanBytes(s.memUsed), humanBytes(s.memTotal)),
		},
		menuet.MenuItem{Type: menuet.Separator},
		menuet.MenuItem{
			Text: fmt.Sprintf("Disk (%s): %.1f%% used", diskMount, s.diskPercent),
		},
		menuet.MenuItem{
			Text: fmt.Sprintf("  %s of %s", humanBytes(s.diskUsed), humanBytes(s.diskTotal)),
		},
	)
	return items
}

// tunnelItems is the prod-tunnel section: the On/Off switch, the instance and
// its EC2 state, and what the tunnel is doing right now.
func tunnelItems() []menuet.MenuItem {
	v := prodTunnel.view()
	if !v.available {
		return []menuet.MenuItem{
			{Text: "Prod Tunnel (unavailable)"},
			{Text: "  " + truncate(v.lastLine, 80)},
		}
	}
	items := []menuet.MenuItem{
		{
			Text:  "Prod Tunnel (" + v.name + ")",
			State: v.on(),
			Clicked: func() {
				prodTunnel.toggle()
				render()
				menuItemsChanged()
			},
		},
		{Text: "  " + truncate(v.hostLine(), 80)},
		{Text: "  " + truncate(v.statusLine(), 80)},
	}
	// A running host with no tunnel of ours (e.g. after `--keep` in a terminal)
	// is the state that quietly bills; offer the script's --stop directly.
	if v.phase == tunnelOff && v.host == "running" {
		items = append(items, menuet.MenuItem{
			Text: "  Stop host now",
			Clicked: func() {
				prodTunnel.stopHost()
				menuItemsChanged()
			},
		})
	}
	return items
}

// humanBytes formats a byte count as GB/MB with one decimal.
func humanBytes(b uint64) string {
	const unit = 1024.0
	f := float64(b)
	switch {
	case f >= unit*unit*unit:
		return fmt.Sprintf("%.1f GB", f/(unit*unit*unit))
	case f >= unit*unit:
		return fmt.Sprintf("%.1f MB", f/(unit*unit))
	default:
		return fmt.Sprintf("%.1f KB", f/unit)
	}
}
