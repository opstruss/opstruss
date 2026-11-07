package proc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// SudoCommand is the hidden command that sudo runs as root, as sudoers
// names it; the name never changes.
const SudoCommand = "__sudo"

// SudoExit is the exit code of opstruss __sudo for a failure of its own,
// when it cannot take a program.
const SudoExit = 125

// Spec is a program that opstruss __sudo starts as root: what it runs,
// where, with what environment, how it stops, and the directories whose
// files that root makes go back to the operator. Its descriptors come
// beside it, as 0, 1, 2, and 3 and up.
type Spec struct {
	Path   string        `json:"path"`
	Args   []string      `json:"args"`
	Dir    string        `json:"dir"`
	Env    []string      `json:"env"`
	Signal string        `json:"signal"`
	Grace  time.Duration `json:"grace"`
	Own    []string      `json:"own,omitempty"`
}

// sudoMessage is one line between OpsTruss and opstruss __sudo, which is
// internal and may change with the binary, since both are the same one.
type sudoMessage struct {
	Ready    bool     `json:"ready,omitempty"`
	Spec     *Spec    `json:"spec,omitempty"`
	Started  bool     `json:"started,omitempty"`
	Error    string   `json:"error,omitempty"`
	Errno    int      `json:"errno,omitempty"`
	Stop     bool     `json:"stop,omitempty"`
	Kill     bool     `json:"kill,omitempty"`
	Exit     *int     `json:"exit,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// SudoRefused is the error of a sudo that did not start opstruss __sudo:
// it needs a password, or the operator may not run it. Message is what
// sudo said.
type SudoRefused struct{ Message string }

func (e *SudoRefused) Error() string {
	if e.Message == "" {
		return "sudo refused to run opstruss " + SudoCommand
	}
	return e.Message
}

// Sudo is how to start opstruss __sudo: sudo and the binary of OpsTruss,
// by path, and the environment of sudo itself.
type Sudo struct {
	Path string // sudo
	Exe  string // opstruss
	Env  []string
}

// Args are the arguments of sudo that start opstruss __sudo, without a
// password and without any other argument, as a narrow rule of sudoers
// names them.
func (s Sudo) Args() []string { return []string{"sudo", "-n", "--", s.Exe, SudoCommand} }

// maxSudoMessages bounds what OpsTruss keeps of what sudo itself writes.
const maxSudoMessages = 64 << 10

// SudoProgram is a program that runs as root through opstruss __sudo.
type SudoProgram struct {
	ctx   context.Context
	conn  *net.UnixConn
	dec   *json.Decoder
	send  func(sudoMessage) error
	ended func() string
}

// Start starts spec as root through opstruss __sudo, with files as its
// descriptors 0, 1, 2, and 3 and up, which the caller may close once Start
// returns. An error of type *SudoRefused says that sudo did not start
// __sudo; a *fs.PathError says that the program did not start.
func (s Sudo) Start(ctx context.Context, spec Spec, files []*os.File) (*SudoProgram, error) {
	if len(files) > maxFiles {
		return nil, fmt.Errorf("a program that runs as root takes at most %d descriptors", maxFiles)
	}
	conn, dec, ended, err := s.ready()
	if err != nil {
		return nil, err
	}
	var m sudoMessage
	var mu sync.Mutex
	send := func(m sudoMessage) error {
		mu.Lock()
		defer mu.Unlock()
		return json.NewEncoder(conn).Encode(m)
	}
	fds := make([]int, len(files))
	for i, f := range files {
		fds[i] = int(f.Fd())
	}
	mu.Lock()
	_, _, err = conn.WriteMsgUnix([]byte{0}, syscall.UnixRights(fds...), nil)
	mu.Unlock()
	if err == nil {
		err = send(sudoMessage{Spec: &spec})
	}
	if err == nil {
		err = dec.Decode(&m)
	}
	if err != nil || !m.Started {
		msg := ended()
		switch {
		case m.Errno != 0:
			return nil, &fs.PathError{Op: "fork/exec", Path: spec.Path, Err: syscall.Errno(m.Errno)}
		case m.Error != "":
			return nil, errors.New(m.Error)
		}
		return nil, fmt.Errorf("opstruss %s ended before it started the program: %s", SudoCommand, cmp(msg, err))
	}
	return &SudoProgram{ctx: ctx, conn: conn, dec: dec, send: send, ended: ended}, nil
}

// ready starts opstruss __sudo through sudo -n and waits until it says
// that it is ready, which sudo refuses when it wants a password. ended
// closes the socket, waits for sudo, and gives what sudo said.
func (s Sudo) ready() (conn *net.UnixConn, dec *json.Decoder, ended func() string, err error) {
	conn, theirs, err := socketPair()
	if err != nil {
		return nil, nil, nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = conn.Close()
		_ = theirs.Close()
		return nil, nil, nil, err
	}
	cmd := &exec.Cmd{Path: s.Path, Args: s.Args(), Env: s.Env, Stdin: theirs, Stdout: pw, Stderr: pw}
	_, err = Start(cmd)
	_ = theirs.Close()
	_ = pw.Close()
	if err != nil {
		_ = conn.Close()
		_ = pr.Close()
		return nil, nil, nil, fmt.Errorf("sudo cannot start: %w", err)
	}
	said := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(pr, maxSudoMessages))
		_, _ = io.Copy(io.Discard, pr)
		_ = pr.Close()
		said <- b
	}()
	ended = func() string {
		_ = conn.Close()
		_ = cmd.Wait()
		return strings.TrimSpace(string(<-said))
	}
	dec = json.NewDecoder(conn)
	var m sudoMessage
	if err := dec.Decode(&m); err != nil || !m.Ready {
		msg := ended()
		if cmd.ProcessState.ExitCode() == SudoExit {
			return nil, nil, nil, errors.New(msg)
		}
		return nil, nil, nil, &SudoRefused{Message: msg}
	}
	return conn, dec, ended, nil
}

// Probe starts opstruss __sudo through sudo -n with no program, which says
// whether sudo runs it without a password: nil, or a *SudoRefused with what
// sudo said.
func (s Sudo) Probe() error {
	_, _, ended, err := s.ready()
	if err == nil {
		ended()
	}
	return err
}

// Wait waits for the program, as Wait does for one of this user: the
// context of Start stops it with the signal and grace of its spec, and the
// kill channel of that context kills it. When OpsTruss dies, __sudo sees
// its socket close and stops the program the same way, so a program of
// root never outlives the run.
func (p *SudoProgram) Wait() (code int, stopped bool, warnings []string, err error) {
	done := make(chan struct{})
	var stop atomic.Bool
	go func() {
		kill := Kill(p.ctx)
		select {
		case <-p.ctx.Done():
			stop.Store(true)
			_ = p.send(sudoMessage{Stop: true})
		case <-done:
			return
		}
		select {
		case <-kill:
			_ = p.send(sudoMessage{Kill: true})
		case <-done:
		}
	}()
	var m sudoMessage
	err = p.dec.Decode(&m)
	close(done)
	msg := p.ended()
	if err != nil || m.Exit == nil {
		return 0, stop.Load(), nil, fmt.Errorf("opstruss %s ended before the program did: %s", SudoCommand, cmp(msg, err))
	}
	return *m.Exit, stop.Load(), m.Warnings, nil
}

// cmp is what sudo said, or else err.
func cmp(said string, err error) string {
	if said != "" || err == nil {
		return said
	}
	return err.Error()
}

// socketPair makes the socket between OpsTruss and __sudo: ours, and the
// end that sudo gets as its stdin. Neither end leaks into another program
// that starts meanwhile, or the end of ours in it would keep __sudo from
// seeing OpsTruss die.
func socketPair() (*net.UnixConn, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	ours := os.NewFile(uintptr(fds[0]), "sudo")
	defer func() { _ = ours.Close() }()
	c, err := net.FileConn(ours)
	if err != nil {
		_ = syscall.Close(fds[1])
		return nil, nil, err
	}
	return c.(*net.UnixConn), os.NewFile(uintptr(fds[1]), "sudo"), nil
}

// Serve is opstruss __sudo, which sudo runs as root with the socket of
// OpsTruss as its stdin: it takes one program and its descriptors, runs
// it in a process group of its own, stops it as OpsTruss says or once the
// socket closes, gives the operator back the files that root made in the
// directories of the run, and says how the program ended. It returns its
// own exit code; the program's goes through the socket.
func Serve(stderr io.Writer, exe string) int {
	var st unix.Stat_t
	if err := unix.Fstat(0, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		_, _ = fmt.Fprintf(stderr, "opstruss %s: its stdin is not the socket of opstruss, as sudoers log_input makes it; add Defaults!%s !log_input to sudoers\n",
			SudoCommand, exe)
		return SudoExit
	}
	c, err := net.FileConn(os.Stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "opstruss %s: %v\n", SudoCommand, err)
		return SudoExit
	}
	conn := c.(*net.UnixConn)
	enc := json.NewEncoder(conn)
	if enc.Encode(sudoMessage{Ready: true}) != nil {
		return SudoExit
	}
	files, err := receiveFiles(conn)
	if err != nil {
		return SudoExit
	}
	dec := json.NewDecoder(conn)
	var m sudoMessage
	if err := dec.Decode(&m); err != nil || m.Spec == nil {
		return SudoExit
	}
	spec := m.Spec
	reply := func(m sudoMessage) {
		_ = enc.Encode(m)
	}
	if len(files) < 3 {
		reply(sudoMessage{Error: "opstruss " + SudoCommand + " takes the descriptors 0, 1, and 2 of the program"})
		return 0
	}
	sig, err := ParseSignal(spec.Signal)
	if err != nil {
		reply(sudoMessage{Error: err.Error()})
		return 0
	}
	env := spec.Env
	for _, k := range []string{"SUDO_USER", "SUDO_UID", "SUDO_GID"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	cmd := &exec.Cmd{Path: spec.Path, Args: spec.Args, Dir: spec.Dir, Env: env, Stdin: files[0], Stdout: files[1],
		Stderr: files[2], ExtraFiles: files[3:]}
	g, err := Start(cmd)
	for _, f := range files {
		_ = f.Close()
	}
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			reply(sudoMessage{Errno: int(errno)})
		} else {
			reply(sudoMessage{Error: err.Error()})
		}
		return 0
	}
	reply(sudoMessage{Started: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, kill := WithKill(ctx)
	defer kill()
	go func() {
		// The socket closes when OpsTruss dies, which stops the program as
		// a stop does.
		defer cancel()
		for {
			var m sudoMessage
			if dec.Decode(&m) != nil {
				return
			}
			switch {
			case m.Kill:
				cancel()
				kill()
			case m.Stop:
				cancel()
			}
		}
	}()
	_, warnings := Wait(ctx, cmd, g, sig, spec.Grace)
	warnings = append(warnings, giveBack(spec.Own)...)
	code := ExitCode(cmd.ProcessState)
	reply(sudoMessage{Exit: &code, Warnings: warnings})
	return 0
}

// maxFiles bounds the descriptors of one program.
const maxFiles = 256

// receiveFiles reads the descriptors that come with one byte.
func receiveFiles(conn *net.UnixConn) ([]*os.File, error) {
	b, oob := make([]byte, 1), make([]byte, syscall.CmsgSpace(maxFiles*4))
	_, oobn, flags, _, err := conn.ReadMsgUnix(b, oob)
	if err != nil {
		return nil, err
	}
	if flags&syscall.MSG_CTRUNC != 0 {
		return nil, fmt.Errorf("more than %d descriptors", maxFiles)
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for i := range msgs {
		fds, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			return nil, err
		}
		for _, fd := range fds {
			syscall.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "fd "+strconv.Itoa(fd)))
		}
	}
	return files, nil
}

// giveBack gives the operator who ran sudo the files that root owns in
// dirs, without following a symlink, so a later program that runs as the
// operator, and the removal of the run's directory, still reach them, as
// SUDO_UID and SUDO_GID name the operator. Files elsewhere stay root's, as
// with sudo at a shell.
func giveBack(dirs []string) []string {
	uid, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 != nil || err2 != nil || uid == 0 {
		return nil
	}
	var failed []string
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid == 0 {
				if err := os.Lchown(p, uid, gid); err != nil && len(failed) < 3 {
					failed = append(failed, err.Error())
				}
			}
			return nil
		})
	}
	if len(failed) > 0 {
		return []string{"some files that the program made as root stay root's: " + strings.Join(failed, "; ")}
	}
	return nil
}
