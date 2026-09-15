package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScript mimics prod-tunnel.sh's shape: --status/--stop flags, an EXIT trap
// that "stops the host", and a long-running foreground child standing in for
// `aws ssm start-session`.
const fakeScript = `#!/usr/bin/env bash
set -euo pipefail
INSTANCE="i-0fake"                                                             # fake-nat, t4g.nano, test
LOCAL_PORT="${PROD_TUNNEL_PORT:-13307}"
case "${1:-}" in
  --status) echo "host=running ssm=Online"; exit 0 ;;
  --stop)   echo "explicit-stop" >> "$LOG"; exit 0 ;;
esac
trap 'echo "stopping $INSTANCE …"; echo "trap-ran" >> "$LOG"' EXIT
echo "tunnel: 127.0.0.1:$LOCAL_PORT → rds:3306"
echo "Port $LOCAL_PORT opened for sessionId fake"
echo "Waiting for connections..."
sleep 300
`

func waitPhase(t *testing.T, tn *tunnel, want tunnelPhase) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tn.view().phase == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("phase = %v, want %v (last line %q)", tn.view().phase, want, tn.view().lastLine)
}

func TestTunnelStartStopRunsExitTrap(t *testing.T) {
	tunnelChanged = func() {} // no AppKit in tests

	dir := t.TempDir()
	script := filepath.Join(dir, "prod-tunnel.sh")
	log := filepath.Join(dir, "log")
	if err := os.WriteFile(script, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACMONITOR_TUNNEL_SCRIPT", script)
	t.Setenv("LOG", log)

	tn := newTunnel()
	if tn.instance != "i-0fake" || tn.name != "fake-nat" || tn.port != "13307" {
		t.Fatalf("parsed instance=%q name=%q port=%q", tn.instance, tn.name, tn.port)
	}

	tn.refresh()
	if v := tn.view(); v.host != "running" || v.ssm != "Online" {
		t.Fatalf("status poll: host=%q ssm=%q", v.host, v.ssm)
	}

	tn.start()
	waitPhase(t, tn, tunnelUp)
	if !tn.view().on() {
		t.Fatal("switch should show On while up")
	}
	if got := tn.view().mark(); got != "🔌" {
		t.Fatalf("mark = %q", got)
	}

	tn.stop()
	waitPhase(t, tn, tunnelOff)
	tn.wait(5 * time.Second)

	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "trap-ran") {
		t.Fatalf("EXIT trap did not run on stop; log=%q", b)
	}
	if strings.Contains(string(b), "explicit-stop") {
		t.Fatalf("fallback --stop ran although the trap finished; log=%q", b)
	}
	if v := tn.view(); v.on() || v.lastLine != "" {
		t.Fatalf("after stop: phase=%v lastLine=%q", v.phase, v.lastLine)
	}
}

func TestTunnelStatusFailureShowsReason(t *testing.T) {
	tunnelChanged = func() {}
	dir := t.TempDir()
	script := filepath.Join(dir, "prod-tunnel.sh")
	body := "#!/usr/bin/env bash\nINSTANCE=\"i-0fake\"\necho 'Could not connect to the endpoint URL: \"https://ec2.example\"' >&2\nexit 255\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACMONITOR_TUNNEL_SCRIPT", script)
	tn := newTunnel()
	tn.refresh()
	v := tn.view()
	if v.host != "?" || !strings.HasPrefix(v.pollErr, "Could not connect") {
		t.Fatalf("host=%q pollErr=%q", v.host, v.pollErr)
	}
	if got := v.hostLine(); !strings.Contains(got, "unreachable — Could not connect") {
		t.Fatalf("hostLine = %q", got)
	}
}

func TestTunnelMissingScript(t *testing.T) {
	tunnelChanged = func() {}
	t.Setenv("MACMONITOR_TUNNEL_SCRIPT", filepath.Join(t.TempDir(), "nope.sh"))
	tn := newTunnel()
	if v := tn.view(); v.available || v.phase != tunnelFailed {
		t.Fatalf("missing script: available=%v phase=%v", v.available, v.phase)
	}
	tn.start() // must be a no-op, not a panic
	if tn.view().on() {
		t.Fatal("started without a script")
	}
}
