package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// reexecMarker distinguishes "I am the outer launcher" from "I am the re-exec'd copy running as PID 1 inside the new namespace"
const reexecMarker = "__namespace_init__"

func main() {
	if len(os.Args) > 1 && os.Args[1] == reexecMarker {
		runInsideNamespace(os.Args[2:])
		return
	}
	spawn(os.Args[1:])
}

// spawn re-executes this same binary via /proc/self/exe inside a fresh PID namespace.
// exec.Cmd + Cloneflags creates the child with clone(2), so the new process is born into the namespace rather than joining in later.
func spawn(args []string) {
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}

	cmd := exec.Command("/proc/self/exe", append([]string{reexecMarker}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
	}
	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "namespace child exited with error:", err)
		os.Exit(1)
	}
}

// runInsideNamespace is the code that runs AS PID 1 of the new namespace, after clone() but before we hand off to the real target program.
func runInsideNamespace(args []string) {
	fmt.Printf("[pid namespace] my pid in here: %d (should be 1)\n", unix.Getpid())

	if err := setupMounts(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mount setup failed:", err)
		os.Exit(1)
	}

	fmt.Printf("[pid namespace] about to exec: %v\n", args)

	// execve replaces this process image in place no fork, no leftover
	// Go runtime state, just this process becoming the target binary while remaining PID 1 of the namespace.
	if err := unix.Exec(args[0], args, os.Environ()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "exec failed:", err)
		os.Exit(1)
	}
}

// setupMounts detaches this namespaces mount table from the hosts propagation group,
// then mounts a fresh /proc bound to our new PID namespace so ps/top/etc. reports only what's actually in here
func setupMounts() error {
	// Without this, mounts here would still propagate to/from the host even though CLONES_NEWS gave us a "separate" table
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making / private: %w", err)
	}
	// The proc that we inherited is still bound to the host's PID namespace
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("making /proc: %w", err)
	}
	return nil
}
