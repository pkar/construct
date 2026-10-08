// Package runtest provides a fake container engine for tests. The test
// binary plays the engine: Install puts a script on PATH that runs the
// test binary again, and Serve, called first thing in TestMain, handles
// the engine commands when the binary was started that way.
//
// A fake container's filesystem is a copy of a host directory; exec runs
// the script with /bin/sh in that directory, so test scripts use paths
// relative to the container root.
package runtest

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	envState = "RUNTEST_ENGINE_STATE"
	envBase  = "RUNTEST_ENGINE_BASE"
	envFail  = "RUNTEST_ENGINE_FAIL"
)

// Serve acts as the engine and exits when the binary was started by an
// Install script; otherwise it returns at once.
func Serve() {
	state := os.Getenv(envState)
	if state == "" {
		return
	}
	code, err := serve(state, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake engine:", err)
		if code == 0 {
			code = 125
		}
	}
	os.Exit(code)
}

// Engine is an installed fake engine.
type Engine struct {
	Name  string
	Path  string
	State string
}

// Install creates a fake engine called name whose containers start as a
// copy of base, and puts it first on PATH. With fail set, every run
// command fails, as when the engine's VM is not running.
func Install(t testing.TB, name, base string, fail bool) Engine {
	t.Helper()
	bin := t.TempDir()
	state := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	failVal := ""
	if fail {
		failVal = "1"
	}
	// GORACE skips the race detector's one-second sleep at exit, which
	// every engine command would otherwise pay under go test -race.
	script := fmt.Sprintf("#!/bin/sh\nGORACE=atexit_sleep_ms=0 %s=%q %s=%q %s=%q exec %q \"$@\"\n",
		envState, state, envBase, base, envFail, failVal, exe)
	p := filepath.Join(bin, name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return Engine{Name: name, Path: p, State: state}
}

// Calls returns the engine commands run so far, one per line.
func (e Engine) Calls(t testing.TB) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.State, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func serve(state string, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("no command")
	}
	f, err := os.OpenFile(filepath.Join(state, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(f, strings.ReplaceAll(strings.Join(args, " "), "\n", "\\n"))
	f.Close()

	switch args[0] {
	case "run":
		if os.Getenv(envFail) != "" {
			return 125, fmt.Errorf("cannot connect to the engine")
		}
		name := flagValue(args, "--name")
		if name == "" {
			return 2, fmt.Errorf("run without --name")
		}
		return 0, copyTree(os.Getenv(envBase), filepath.Join(state, name))
	case "exec":
		var env []string
		i := 1
		for ; i < len(args) && strings.HasPrefix(args[i], "-"); i += 2 {
			if args[i] == "-e" {
				env = append(env, args[i+1])
			}
		}
		if len(args) < i+4 {
			return 2, fmt.Errorf("exec: want NAME /bin/sh -c SCRIPT")
		}
		root := filepath.Join(state, args[i])
		if _, err := os.Stat(root); err != nil {
			return 1, fmt.Errorf("no container %s", args[i])
		}
		cmd := exec.Command("/bin/sh", "-c", args[i+3])
		cmd.Dir = root
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), nil
			}
			return 1, err
		}
		return 0, nil
	case "export":
		out := flagValue(args, "-o")
		return 0, exportTree(filepath.Join(state, args[len(args)-1]), out)
	case "rm":
		return 0, os.RemoveAll(filepath.Join(state, args[len(args)-1]))
	}
	return 2, fmt.Errorf("unknown command %s", args[0])
}

func flagValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
	})
}

// exportTree writes root as a tar, the way engines export a container:
// "./"-prefixed names, root ownership, and real modification times.
func exportTree(root, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if d.Type()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = "./" + filepath.ToSlash(rel)
		if d.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "root", "root"
		h.PAXRecords = nil
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
