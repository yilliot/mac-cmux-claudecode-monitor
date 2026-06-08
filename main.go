// macmonitor is a lightweight macOS menu bar app that shows live memory and
// disk usage in the top-right status bar. It reads stats via syscalls
// (gopsutil) rather than shelling out, so it idles near 0% CPU.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// stateFile is where Claude Code's hooks write the agent's current state. The
// hooks echo "running" on UserPromptSubmit/PreToolUse/PostToolUse and "idle" on
// Stop and on idle/permission notifications, so its contents track exactly when
// Claude (in cmux or anywhere) is actively working. See README for the snippet.
var stateFile = filepath.Join(os.Getenv("HOME"), ".macmonitor", "claude-state")

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

var current stats

// enabled is the single On/Off switch. When On, the app watches Claude Code and
// holds the keep-awake assertion only while Claude is running. When Off, it does
// nothing and the Mac sleeps normally.
var enabled bool

// claudeRunning reports whether Claude Code is mid-task, per the hook state file.
// A missing or unreadable file (no session, hooks not installed) reads as idle.
func claudeRunning() bool {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "running"
}

// applyMode reconciles the held assertion: awake only when On and Claude is busy.
func applyMode() {
	keepAwake.ensure(enabled && claudeRunning())
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
		// Release: kill the assertion-holder.
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		c.cmd = nil
		return
	}

	// Acquire. Flags:
	//   -i  prevent system idle sleep   (the important one for long jobs)
	//   -m  prevent disk from idle-sleeping
	//   -s  prevent system sleep on AC power
	// We intentionally omit -d so the display can still dim/sleep to save power;
	// the machine keeps running. Add "-d" here if you also want the screen on.
	cmd := exec.Command("caffeinate", "-i", "-m", "-s")
	if err := cmd.Start(); err != nil {
		return
	}
	c.cmd = cmd

	// Reap the process if caffeinate ever exits on its own, so state stays honest.
	go func() {
		_ = cmd.Wait()
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

	// Prime values immediately so the bar isn't blank on launch.
	readMemory()
	readDisk()
	render()

	go pollLoop()

	app.RunApplication()
}

// pollLoop refreshes memory and disk on independent cadences.
func pollLoop() {
	memTick := time.NewTicker(memInterval)
	diskTick := time.NewTicker(diskInterval)
	defer memTick.Stop()
	defer diskTick.Stop()

	for {
		select {
		case <-memTick.C:
			readMemory()
			applyMode() // re-check Claude's state every tick when On
			render()
		case <-diskTick.C:
			readDisk()
			render()
		}
	}
}

func readMemory() {
	if v, err := mem.VirtualMemory(); err == nil {
		current.memPercent = v.UsedPercent
		current.memUsed = v.Used
		current.memTotal = v.Total
	}
}

func readDisk() {
	if u, err := disk.Usage(diskMount); err == nil {
		current.diskPercent = u.UsedPercent
		current.diskUsed = u.Used
		current.diskFree = u.Free
		current.diskTotal = u.Total
	}
}

const gb = 1024 * 1024 * 1024

// render pushes the compact two-line title into the menu bar (GB, number only):
//
//	m:9     (used memory, top line)
//	s:234   (remaining storage, bottom line)
func render() {
	memUsedGB := float64(current.memUsed) / gb
	diskFreeGB := float64(current.diskFree) / gb
	// Top-bar detection cue: ☕️ when On and Claude is actively running (Mac held
	// awake), a hollow dot when On but watching an idle Claude, nothing when Off.
	mark := ""
	switch {
	case keepAwake.on():
		mark = "☕️"
	case enabled:
		mark = "◦"
	}
	title := fmt.Sprintf("m:%.0f%s\ns:%.0f", memUsedGB, mark, diskFreeGB)
	menuet.App().SetMenuState(&menuet.MenuState{Title: title})
}

// menuItems builds the dropdown shown when the title is clicked.
func menuItems() []menuet.MenuItem {
	// Single On/Off toggle. On = keep awake while Claude Code is running.
	toggle := menuet.MenuItem{
		Text:  "Keep Awake while Claude runs",
		State: enabled,
		Clicked: func() {
			enabled = !enabled
			applyMode()
			render()
			menuet.App().MenuChanged()
		},
	}

	// Live detection line so you can see what the watcher currently sees.
	var status string
	switch {
	case !enabled:
		status = "Off"
	case keepAwake.on():
		status = "Claude running — awake ☕️"
	default:
		status = "Claude idle — letting it sleep"
	}

	return []menuet.MenuItem{
		toggle,
		{Text: "  " + status},
		{Type: menuet.Separator},
		{
			Text: fmt.Sprintf("Memory: %.1f%% used", current.memPercent),
		},
		{
			Text: fmt.Sprintf("  %s of %s", humanBytes(current.memUsed), humanBytes(current.memTotal)),
		},
		{Type: menuet.Separator},
		{
			Text: fmt.Sprintf("Disk (%s): %.1f%% used", diskMount, current.diskPercent),
		},
		{
			Text: fmt.Sprintf("  %s of %s", humanBytes(current.diskUsed), humanBytes(current.diskTotal)),
		},
	}
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
