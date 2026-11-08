package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
)

// fakesudo stands in for sudo where a test cannot be root: it runs its
// command as the same user, as sudo does when it execs the command itself,
// with descriptors 0, 1, and 2 and no other, and SUDO_USER, SUDO_UID, and
// SUDO_GID. It reads $HOME/.fakesudo, since sudo gets the clean
// environment of OpsTruss:
//
//	password   the password it wants; without the file, it wants none
//	ticket     the ticket that sudo -v makes, which -n then takes
//	log_input  put a pipe on the stdin of the command, as sudoers log_input does
//	denied     refuse the user after any password, as sudo refuses one that sudoers does not name
//	log        each call, one line of its arguments
//
// It knows sudo -n [-v | -- <command>...] and sudo -S -p <prompt> -v, which
// reads one password from stdin, and fails a wrong one as sudo does.
func fakesudo(args []string) int {
	dir := filepath.Join(os.Getenv("HOME"), ".fakesudo")
	if f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
		_, _ = fmt.Fprintln(f, strings.Join(args, " "))
		_ = f.Close()
	}
	var noPrompt, stdin, validate bool
	var command []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-n":
			noPrompt = true
		case a == "-S":
			stdin = true
		case a == "-p" && i+1 < len(args):
			i++
		case a == "-v":
			validate = true
		case a == "--":
			command = args[i+1:]
			i = len(args)
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "sudo: unknown option %s\n", a)
			return 1
		default:
			command = args[i:]
			i = len(args)
		}
	}
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	want, err := os.ReadFile(filepath.Join(dir, "password"))
	if err == nil && !has("ticket") {
		switch {
		case noPrompt:
			fmt.Fprintln(os.Stderr, "sudo: a password is required")
			return 1
		case !stdin:
			fmt.Fprintln(os.Stderr, "sudo: a terminal is required to read the password")
			return 1
		}
		r := bufio.NewReader(os.Stdin)
		line, err := r.ReadString('\n')
		if has("denied") {
			host, _ := os.Hostname()
			fmt.Fprintf(os.Stderr, "Sorry, user %s may not run sudo on %s.\n", os.Getenv("USER"), host)
			return 1
		}
		if strings.TrimSuffix(line, "\n") != strings.TrimSpace(string(want)) {
			if err == nil {
				// sudo reads stdin again for the next try, and finds its end.
				fmt.Fprint(os.Stderr, "Sorry, try again.\n\nsudo: no password was provided\n")
			}
			fmt.Fprintln(os.Stderr, "sudo: 1 incorrect password attempt")
			return 1
		}
		if err := os.WriteFile(filepath.Join(dir, "ticket"), nil, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "sudo:", err)
			return 1
		}
	}
	if validate {
		return 0
	}
	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sudo -n -- <command>")
		return 1
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "sudo: %s: command not found\n", command[0])
		return 1
	}
	env := os.Environ()
	if u, err := user.Current(); err == nil {
		env = append(env, "SUDO_USER="+u.Username, "SUDO_UID="+u.Uid, "SUDO_GID="+u.Gid)
	}
	if has("log_input") {
		// sudo stays the parent and copies its stdin to the command.
		cmd := exec.Command(path, command[1:]...)
		cmd.Args, cmd.Env, cmd.Stdout, cmd.Stderr = command, env, os.Stdout, os.Stderr
		w, err := cmd.StdinPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "sudo:", err)
			return 1
		}
		go func() {
			_, _ = io.Copy(w, os.Stdin)
			_ = w.Close()
		}()
		var exit *exec.ExitError
		if err := cmd.Wait(); errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return 0
	}
	err = syscall.Exec(path, command, env)
	fmt.Fprintf(os.Stderr, "sudo: %s: %v\n", command[0], err)
	return 1
}
