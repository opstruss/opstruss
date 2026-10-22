package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
)

// testSupervisor is a supervisor of stack default under a new root, with
// the events of the daemon's log.
func testSupervisor(t *testing.T) (*Supervisor, string, func() string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "daemon"), 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var said []string
	sup := newSupervisor(root, "default", &registry{path: filePath(root, "default")}, func(ev record.Event) {
		mu.Lock()
		said = append(said, ev.Address+": "+ev.Message)
		mu.Unlock()
	})
	go sup.recover()
	t.Cleanup(sup.stopAll)
	return sup, root, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(said, "\n")
	}
}

// testService is a service at default/<name> that runs script.
func testService(t *testing.T, root, name, script string) *engine.Service {
	return &engine.Service{Address: "default/" + name, Dir: filepath.Join(root, "daemon", "default", name),
		Program: &plugin.Program{Path: "/bin/sh", Args: []string{"sh", "-c", script}, Dir: t.TempDir(), Env: []string{"PATH=" + os.Getenv("PATH")}},
		Restart: "on-failure", Stop: plugin.Stop{Signal: syscall.SIGTERM, Grace: 5 * time.Second},
		Ready: engine.Ready{Interval: 20 * time.Millisecond, Timeout: 5 * time.Second}}
}

func serviceLog(t *testing.T, root, name string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(root, "logs", "default", name+".jsonl"))
	return string(data)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("%s did not happen", what)
}

// The gate lets the program run only once its pid is in <stack>.json.
func TestServiceRecordedBeforeItRuns(t *testing.T) {
	sup, root, _ := testSupervisor(t)
	s := testService(t, root, "web", `if grep -q "\"pid\": $$," "$FILE"; then echo recorded; else echo missing; fi; exec sleep 30`)
	s.Program.Env = append(s.Program.Env, "FILE="+filePath(root, "default"))
	if err := sup.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the line of the service", func() bool { return strings.Contains(serviceLog(t, root, "web"), `"message":"recorded"`) })
	if err := sup.Stop(context.Background(), s.Address); err != nil {
		t.Fatal(err)
	}
	if ps := sup.reg.processes(); len(ps) != 0 {
		t.Fatalf("entries left after stop: %+v", ps)
	}
}

// The gate runs its program once the daemon writes in the pipe; a daemon
// that dies first closes the pipe, and the program never runs.
func TestGate(t *testing.T) {
	for _, word := range []bool{true, false} {
		marker := filepath.Join(t.TempDir(), "ran")
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", gate, "opstruss", "/bin/sh", "-c", `touch "$0"`, marker)
		cmd.ExtraFiles = []*os.File{os.Stdin, r}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		if word {
			_, _ = w.Write([]byte("go\n"))
		}
		_ = w.Close()
		err = cmd.Wait()
		_, statErr := os.Stat(marker)
		if word != (err == nil && statErr == nil) {
			t.Fatalf("with the daemon's word %v: the gate exited with %v, the program ran: %v", word, err, statErr == nil)
		}
	}
}

// stop.run runs first, then the signal reaches what is left within the
// grace; the output of both is in the log of the service.
func TestServiceStopRun(t *testing.T) {
	sup, root, _ := testSupervisor(t)
	s := testService(t, root, "db", `trap 'echo terminated; exit 0' TERM; echo up; while :; do sleep 0.05; done`)
	s.StopRun = &plugin.Program{Path: "/bin/sh", Args: []string{"sh", "-c", `echo "stopping $OPSTRUSS_SERVICE_ID"`}, Dir: s.Program.Dir,
		Env: s.Program.Env}
	if err := sup.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the service up", func() bool { return strings.Contains(serviceLog(t, root, "db"), `"message":"up"`) })
	if err := sup.Stop(context.Background(), s.Address); err != nil {
		t.Fatal(err)
	}
	log := serviceLog(t, root, "db")
	stopping, terminated := strings.Index(log, `"task":"stop","stream":"stdout","message":"stopping `), strings.Index(log, `"message":"terminated"`)
	if stopping < 0 || terminated < stopping {
		t.Fatalf("stop.run then the signal, in the log:\n%s", log)
	}
	if st, ok := sup.Status(s.Address); ok {
		t.Fatalf("a stopped service is still known: %+v", st)
	}
}

// Restarts back off from the first delay, doubling, and the count shows
// in the status.
func TestServiceRestartBackoff(t *testing.T) {
	first := firstDelay
	firstDelay = 10 * time.Millisecond
	t.Cleanup(func() { firstDelay = first })
	sup, root, said := testSupervisor(t)
	s := testService(t, root, "flaky", `exit 3`)
	if err := sup.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	eventually(t, "three restarts", func() bool {
		st, _ := sup.Status(s.Address)
		return st.Restarts >= 3
	})
	for _, d := range []string{"10ms", "20ms", "40ms"} {
		if !strings.Contains(said(), "exited with 3; restarting in "+d) {
			t.Fatalf("no restart in %s:\n%s", d, said())
		}
	}
	if err := sup.Stop(context.Background(), s.Address); err != nil {
		t.Fatal(err)
	}
}

// A service is ready once its check passes, and up hears why it is not
// when it never does.
func TestServiceReady(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	sup, root, _ := testSupervisor(t)

	s := testService(t, root, "api", `exec sleep 30`)
	s.Ready.HTTP, s.Ready.Timeout = srv.URL, 300*time.Millisecond
	err = sup.Start(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "not ready after 300ms") || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("not ready: %v", err)
	}
	if _, ok := sup.Status(s.Address); ok {
		t.Fatal("a service that was not ready is still known")
	}
	time.AfterFunc(100*time.Millisecond, func() { up.Store(true) })
	s.Ready.Timeout = 5 * time.Second
	if err := sup.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if st, _ := sup.Status(s.Address); st.State != "running" || st.PID == 0 {
		t.Fatalf("status %+v", st)
	}

	tcp := testService(t, root, "cache", `exec sleep 30`)
	tcp.Ready.TCP = ln.Addr().String()
	if err := sup.Start(context.Background(), tcp); err != nil {
		t.Fatal(err)
	}
	// One that exits before it is ready, with nothing to restart it, says so.
	once := testService(t, root, "once", `exit 0`)
	once.Ready.TCP, once.Restart = "127.0.0.1:1", "no"
	if err := sup.Start(context.Background(), once); err == nil || !strings.Contains(err.Error(), "exited with 0 before it was ready") {
		t.Fatalf("exit before ready: %v", err)
	}
}

// What a daemon that ended left is stopped before a service starts again:
// the group of a leader with the recorded pid and start time, and the
// processes that carry the id in groups of their own.
func TestRecoverLeftovers(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "daemon", "default", "web"), 0o700); err != nil {
		t.Fatal(err)
	}
	reg := &registry{path: filePath(root, "default")}
	var entries []*Process
	var exited []chan struct{}
	for _, script := range []string{"exec sleep 30", "sleep 30 & wait"} {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join("daemon", "default", "web", id+".lock")
		tr, err := proc.Track(filepath.Join(root, lock))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = append(os.Environ(), ServiceID+"="+id)
		cmd.ExtraFiles = []*os.File{tr.File()}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = tr.Close()
		t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		exited = append(exited, done)
		start, err := proc.StartTime(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, &Process{Address: "default/web", ID: id, PID: cmd.Process.Pid, Start: start,
			Started: time.Now().UTC().Format(time.RFC3339), Lock: lock, Signal: "SIGTERM", Grace: "5s"})
	}
	if err := reg.update(func(f *File) { f.Processes = entries }); err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(root, "default", reg, func(record.Event) {})
	sup.recover()
	defer sup.stopAll()
	for _, done := range exited {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a leftover still runs")
		}
	}
	if ps := reg.processes(); len(ps) != 0 {
		t.Fatalf("entries left after recovery: %+v", ps)
	}
}
