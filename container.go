package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// reexecMarker distinguishes "I am the outer launcher" from "I am the re-exec'd copy running as PID 1 inside the new namespace"
const reexecMarker = "__namespace_init__"

func main() {
	if len(os.Args) > 1 && os.Args[1] == reexecMarker {
		if len(os.Args) < 4 {
			_, _ = fmt.Fprintln(os.Stderr, "Usage: <marker> <rootfs> <cmd> [args]")
			os.Exit(1)
		}
		runInsideNamespace(os.Args[2], os.Args[3:])
		return
	}
	if len(os.Args) < 2 {
		_, _ = fmt.Fprintln(os.Stderr, "Usage: container-from-scratch <rootfs> <cmd> [args]")
	}
	rootfs := os.Args[1]
	cmd := os.Args[2:]
	if len(cmd) == 0 {
		cmd = []string{"/bin/sh"}
	}
	spawn(rootfs, cmd)
}

// spawn re-executes this same binary via /proc/self/exe inside a fresh PID namespace.
// exec.Cmd + Cloneflags creates the child with clone(2), so the new process is born into the namespace rather than joining in later.
func spawn(rootfs string, args []string) {
	argv := append([]string{reexecMarker, rootfs}, args...)
	cmd := exec.Command("/proc/self/exe", argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}
	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "namespace child exited with error:", err)
		os.Exit(1)
	}
}

// runInsideNamespace is the code that runs AS PID 1 of the new namespace, after clone() but before we hand off to the real target program.
func runInsideNamespace(rootfs string, args []string) {
	fmt.Printf("[pid namespace] my pid in here: %d (should be 1)\n", unix.Getpid())

	if err := makeMountsPrivate(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mount setup failed", err)
		os.Exit(1)
	}

	if err := unix.Sethostname([]byte("container")); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "set hostname failed:", err)
		os.Exit(1)
	}

	if err := pivotRoot(rootfs); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pivot root failed:", err)
		os.Exit(1)
	}

	if err := mountProc(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mount failed:", err)
		os.Exit(1)
	}

	fmt.Printf("[rootfs] pivoted into %s, about to exec %v\n", rootfs, args)

	// execve replaces this process image in place no fork, no leftover
	// Go runtime state, just this process becoming the target binary while remaining PID 1 of the namespace.
	if err := unix.Exec(args[0], args, os.Environ()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "exec failed:", err)
		os.Exit(1)
	}
}

func makeMountsPrivate() error {
	return unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
}

func mountProc() error {
	return unix.Mount("proc", "/proc", "proc", 0, "")
}

// pivotRoot makes newRoot the process's root filesystem and unmounts the old root, so nothing outside newRoot is reachable anymore
func pivotRoot(newRoot string) error {
	if err := unix.Mount(newRoot, newRoot, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind-mounting new root: %w", err)
	}
	oldRootDir := filepath.Join(newRoot, ".pivot_root_old")
	if err := os.MkdirAll(oldRootDir, 0700); err != nil {
		return fmt.Errorf("creating old root dir: %w", err)
	}
	if err := unix.PivotRoot(newRoot, oldRootDir); err != nil {
		return fmt.Errorf("pivot-root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir to /: %w", err)
	}
	oldRootInNewRoot := "/.pivot_root_old"
	if err := unix.Unmount(oldRootInNewRoot, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmounting old root: %w", err)
	}
	return os.RemoveAll(oldRootInNewRoot)
}
