// tunnel.go — a menu switch for the production database tunnel.
//
// The tunnel itself is ~/herd/commun/scripts/prod-tunnel.sh: it starts the SSM
// jump host on demand, forwards RDS 3306 to 127.0.0.1:13306, and stops the host
// again when the tunnel closes so it only costs disk while idle. This file just
// drives that script from the menu bar:
//
//	On  → run the script (host starts, SSM comes up, port forward opens)
//	Off → the Ctrl+C equivalent: SIGINT the script's process group, so its EXIT
//	      trap stops the host again
//
// The host's EC2 state is polled through the script's own --status flag, so the
// instance id, region and endpoint live in exactly one place (the script).
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// tunnelScript is the script that owns the tunnel, relative to $HOME.
	// MACMONITOR_TUNNEL_SCRIPT overrides it with an absolute path.
	tunnelScript = "herd/commun/scripts/prod-tunnel.sh"

	// tunnelPoll is how often the host's EC2 state is refreshed. DescribeInstances
	// is free and takes ~1s, so this is cheap; it also runs right after every
	// On/Off transition so the menu doesn't wait a full period to catch up.
	tunnelPoll = 30 * time.Second

	// tunnelStopWait bounds how long Off waits for the script's EXIT trap to
	// stop the host before the process group is force-killed and the host is
	// stopped explicitly with --stop.
	tunnelStopWait = 60 * time.Second

	// tunnelStatusTimeout bounds a single --status poll (two AWS API calls).
	tunnelStatusTimeout = 25 * time.Second
)

// tunnelPhase is the lifecycle of the script process we own.
type tunnelPhase int

const (
	tunnelOff      tunnelPhase = iota // no script running
	tunnelStarting                    // script running, port forward not yet open
	tunnelUp                          // session-manager-plugin is listening locally
	tunnelStopping                    // Off requested; waiting for the EXIT trap to stop the host
	tunnelFailed                      // script exited on its own with an error
)

// tunnel is the controller. All fields are guarded by mu: the script's output
// reader, the status poller and AppKit's main thread (menu clicks/renders) all
// touch them.
type tunnel struct {
	mu sync.Mutex

	script   string // absolute path to prod-tunnel.sh
	name     string // human label parsed from the script ("commun-nat")
	instance string // EC2 instance id parsed from the script
	port     string // local port parsed from the script

	phase    tunnelPhase
	cmd      *exec.Cmd
	done     chan struct{} // closed once cmd has been reaped
	stopping bool          // Off was requested, so an exit is expected rather than a failure
	lastLine string        // most recent script output line, shown in the menu
	since    time.Time     // when the current starting/stopping phase began

	pollErr string // why the last --status poll failed (offline, expired creds…)

	host       string    // EC2 state from --status: running/stopped/pending/stopping, "?" if unknown
	ssm        string    // SSM agent ping from --status (only present while host=running)
	checkedAt  time.Time // when host/ssm were last refreshed
	refreshing bool      // a --status poll is in flight; don't stack another
}

var prodTunnel = newTunnel()

// tunnelView is an immutable snapshot for rendering.
type tunnelView struct {
	name, instance, port string
	phase                tunnelPhase
	lastLine             string
	since                time.Time
	host, ssm, pollErr   string
	checkedAt            time.Time
	available            bool // the script exists
}

// newTunnel locates the script and pulls the display metadata out of it, so the
// app never has to hard-code an instance id that the script already knows.
func newTunnel() *tunnel {
	t := &tunnel{
		script: filepath.Join(os.Getenv("HOME"), tunnelScript),
		name:   "prod",
		host:   "?",
		port:   "13306",
	}
	if p := os.Getenv("MACMONITOR_TUNNEL_SCRIPT"); p != "" {
		t.script = p
	}
	src, err := os.ReadFile(t.script)
	if err != nil {
		t.phase = tunnelFailed
		t.lastLine = "script not found: " + t.script
		t.script = ""
		return t
	}
	// INSTANCE="i-0d75cf550071ee354"    # commun-nat, t4g.nano, SSM only
	if m := regexp.MustCompile(`(?m)^INSTANCE="([^"]+)"\s*#\s*([^,\n]+)`).FindSubmatch(src); m != nil {
		t.instance = string(m[1])
		t.name = strings.TrimSpace(string(m[2]))
	} else if m := regexp.MustCompile(`(?m)^INSTANCE="([^"]+)"`).FindSubmatch(src); m != nil {
		t.instance = string(m[1])
	}
	// LOCAL_PORT="${PROD_TUNNEL_PORT:-13306}"
	if m := regexp.MustCompile(`(?m)^LOCAL_PORT="\$\{PROD_TUNNEL_PORT:-(\d+)\}"`).FindSubmatch(src); m != nil {
		t.port = string(m[1])
	}
	return t
}

func (t *tunnel) view() tunnelView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return tunnelView{
		name: t.name, instance: t.instance, port: t.port,
		phase: t.phase, lastLine: t.lastLine, since: t.since,
		host: t.host, ssm: t.ssm, pollErr: t.pollErr, checkedAt: t.checkedAt,
		available: t.script != "",
	}
}

// on reports whether the switch should show as checked: we own a script process
// that is opening or holding the tunnel.
func (v tunnelView) on() bool {
	return v.phase == tunnelStarting || v.phase == tunnelUp
}

// command builds an exec.Cmd for the script with a PATH that works when the app
// is launched from Finder/launchd (which gives it only /usr/bin:/bin:...):
// aws lives in Homebrew's bin and session-manager-plugin in ~/.local/bin.
func (t *tunnel) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "bash", append([]string{t.script}, args...)...)
	home := os.Getenv("HOME")
	path := strings.Join([]string{
		"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(home, ".local", "bin"), os.Getenv("PATH"),
	}, ":")
	cmd.Env = append(os.Environ(), "PATH="+path, "AWS_PAGER=")
	return cmd
}

// toggle is the menu click: Off/failed → start, starting/up → stop.
func (t *tunnel) toggle() {
	if t.view().on() {
		t.stop()
	} else {
		t.start()
	}
}

// start launches the script in its own process group so stop can deliver the
// Ctrl+C equivalent to bash, the aws CLI and session-manager-plugin together.
func (t *tunnel) start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.script == "" || t.cmd != nil {
		return
	}
	cmd := t.command(context.Background())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.phase, t.lastLine = tunnelFailed, err.Error()
		return
	}
	cmd.Stderr = cmd.Stdout // same pipe, so stderr lines show in the menu too
	if err := cmd.Start(); err != nil {
		t.phase, t.lastLine = tunnelFailed, err.Error()
		return
	}
	t.cmd = cmd
	t.done = make(chan struct{})
	t.stopping = false
	t.phase = tunnelStarting
	t.lastLine = ""
	t.since = time.Now()
	go t.watch(cmd, out, t.done)
}

// watch streams the script's output into lastLine, promotes the phase to Up
// when session-manager-plugin reports it is listening, and reaps the process.
func (t *tunnel) watch(cmd *exec.Cmd, out io.Reader, done chan struct{}) {
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		t.mu.Lock()
		t.lastLine = line
		// session-manager-plugin prints "Port 13306 opened for sessionId …" and
		// then "Waiting for connections..." once the local listener is ready.
		if t.phase == tunnelStarting && (strings.HasPrefix(line, "Waiting for connections") ||
			(strings.HasPrefix(line, "Port ") && strings.Contains(line, " opened "))) {
			t.phase = tunnelUp
		}
		t.mu.Unlock()
		t.changed()
	}
	err := cmd.Wait()

	t.mu.Lock()
	if t.cmd == cmd {
		t.cmd = nil
		switch {
		case t.stopping, err == nil:
			t.phase = tunnelOff
			t.lastLine = ""
		default:
			t.phase = tunnelFailed
			t.lastLine = fmt.Sprintf("%v — %s", err, t.lastLine)
		}
		t.stopping = false
	}
	t.mu.Unlock()
	close(done)
	t.changed()
	t.refresh() // pick up the host's new state right away
}

// stop closes the tunnel and lets the script stop the host. It returns
// immediately; use wait to block until the script has actually exited.
func (t *tunnel) stop() {
	t.mu.Lock()
	cmd, done := t.cmd, t.done
	if cmd == nil {
		t.mu.Unlock()
		return
	}
	t.stopping = true
	t.phase = tunnelStopping
	t.lastLine = ""
	t.since = time.Now()
	t.mu.Unlock()
	t.changed()

	// SIGINT the whole group — exactly what Ctrl+C in a terminal does. The aws
	// session and plugin exit, then bash runs its EXIT trap (stop_host).
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)

	go func() {
		select {
		case <-done:
			return
		case <-time.After(tunnelStopWait):
		}
		// The trap didn't finish in time. Kill the group, then stop the host
		// ourselves so it can't be left running (and billing) unnoticed.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), tunnelStatusTimeout)
		defer cancel()
		_ = t.command(ctx, "--stop").Run()
		t.refresh()
	}()
}

// wait blocks until no script process is running, or the timeout elapses.
// Used on Quit so the tunnel (and the host) don't outlive the app.
func (t *tunnel) wait(timeout time.Duration) {
	t.mu.Lock()
	done := t.done
	running := t.cmd != nil
	t.mu.Unlock()
	if !running {
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// stopHost stops the jump host without touching a tunnel we own. It's offered
// in the menu when the host is running but the switch is Off — e.g. after a
// `--keep` session in a terminal — since that's the state that quietly costs.
func (t *tunnel) stopHost() {
	t.mu.Lock()
	if t.script == "" || t.cmd != nil {
		t.mu.Unlock()
		return
	}
	t.lastLine = "stopping " + t.instance + "…"
	t.mu.Unlock()
	t.changed()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), tunnelStatusTimeout)
		defer cancel()
		_ = t.command(ctx, "--stop").Run()
		t.mu.Lock()
		if t.cmd == nil && t.phase == tunnelOff {
			t.lastLine = ""
		}
		t.mu.Unlock()
		t.refresh()
	}()
}

var (
	reHost = regexp.MustCompile(`host=(\S+)`)
	reSSM  = regexp.MustCompile(`ssm=(\S+)`)
)

// refresh polls the host's EC2 state via `prod-tunnel.sh --status`. Safe to
// call from any goroutine; concurrent calls collapse into one poll.
func (t *tunnel) refresh() {
	t.mu.Lock()
	if t.script == "" || t.refreshing {
		t.mu.Unlock()
		return
	}
	t.refreshing = true
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), tunnelStatusTimeout)
	defer cancel()
	out, err := t.command(ctx, "--status").Output()

	host, ssm, pollErr := "?", "", ""
	switch {
	case err == nil:
		if m := reHost.FindSubmatch(out); m != nil {
			host = string(m[1])
		}
		if m := reSSM.FindSubmatch(out); m != nil {
			ssm = string(m[1])
		}
	case ctx.Err() != nil:
		pollErr = "timed out (offline?)"
	default:
		// Output() keeps stderr in the ExitError; its last line is the aws CLI's
		// one-line reason ("Could not connect to the endpoint URL", "expired
		// token", …), which is what you want to see in the menu.
		pollErr = err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			if last := lastNonEmptyLine(string(ee.Stderr)); last != "" {
				pollErr = last
			}
		}
	}

	t.mu.Lock()
	changed := host != t.host || ssm != t.ssm || pollErr != t.pollErr
	t.host, t.ssm, t.pollErr, t.checkedAt = host, ssm, pollErr, time.Now()
	t.refreshing = false
	t.mu.Unlock()
	if changed {
		t.changed()
	}
}

// changed re-renders the bar and refreshes the dropdown if it's open.
func (t *tunnel) changed() { tunnelChanged() }

// tunnelChanged is a variable so tests can run the controller without AppKit.
var tunnelChanged = func() {
	render()
	menuItemsChanged()
}

// mark is the compact cue appended to the bar's storage line.
func (v tunnelView) mark() string {
	switch v.phase {
	case tunnelUp:
		return "🔌"
	case tunnelStarting, tunnelStopping:
		return "…"
	case tunnelFailed:
		return "⚠️"
	}
	if v.host == "running" {
		return "●" // host up but no tunnel of ours — it's billing while it sits there
	}
	return ""
}

// statusLine is the human-readable line under the switch in the dropdown.
// In-progress phases carry their elapsed time, so a stall (no network, a slow
// host boot) is visible instead of looking frozen.
func (v tunnelView) statusLine() string {
	switch v.phase {
	case tunnelUp:
		return fmt.Sprintf("tunnel up: 127.0.0.1:%s → RDS 3306", v.port)
	case tunnelStarting:
		return v.progress("starting", "contacting AWS")
	case tunnelStopping:
		return v.progress("stopping", "letting the script stop the host")
	case tunnelFailed:
		return "failed: " + v.lastLine
	}
	if v.lastLine != "" {
		return v.lastLine
	}
	if v.host == "running" {
		return "tunnel off — host is still running"
	}
	return "tunnel off"
}

// progress formats "<verb>… <elapsed> · <last script line>".
func (v tunnelView) progress(verb, fallback string) string {
	detail := v.lastLine
	if detail == "" {
		detail = fallback
	}
	return fmt.Sprintf("%s… %s · %s", verb, time.Since(v.since).Round(time.Second), detail)
}

// hostLine describes the instance and its last-polled state.
func (v tunnelView) hostLine() string {
	host := v.host
	switch {
	case v.pollErr != "":
		host = "unreachable — " + v.pollErr
	case v.ssm != "":
		host += " · ssm " + v.ssm
	}
	age := ""
	if !v.checkedAt.IsZero() {
		age = fmt.Sprintf(" (%s ago)", time.Since(v.checkedAt).Round(time.Second))
	}
	return fmt.Sprintf("%s: %s%s", v.instance, host, age)
}

// lastNonEmptyLine returns the final non-blank line of s, trimmed.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// truncate keeps menu lines from stretching the dropdown across the screen.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
