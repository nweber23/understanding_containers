package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// reexecMarker distinguishes "I am the outer launcher" from "I am the re-exec'd copy running as PID 1 inside the new namespace"
const reexecMarker = "__namespace_init__"

const (
	cgroupPath       = "/sys/fs/cgroup/container-from-scratch"
	memoryLimitBytes = "20971520"
	cpuMax           = "10000 100000"
	pidsMax          = "64"
)

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
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNET,
	}
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "namespace child exited with error:", err)
		os.Exit(1)
	}
	cleanup, err := setupCgroup(cmd.Process.Pid)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "cgroup setup failed:", err)
		os.Exit(1)
	}
	if err := cmd.Wait(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "cgroup wait failed:", err)
		os.Exit(1)
	}
}

// setupCgroup creates a fresh cgroup caps its memory and CPU, and moves PID into it.
func setupCgroup(pid int) (cleanup func(), err error) {
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return nil, fmt.Errorf("creating group: %w", err)
	}
	cleanup = func() { _ = os.Remove(cgroupPath) }

	if err := os.WriteFile(filepath.Join(cgroupPath, "memory.max"), []byte(memoryLimitBytes), 0644); err != nil {
		return cleanup, fmt.Errorf("setting memory limit: %w", err)
	}

	if err := os.WriteFile(filepath.Join(cgroupPath, "memory.swap.max"), []byte("0"), 0644); err != nil {
		return cleanup, fmt.Errorf("setting memory swap: %w", err)
	}

	if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.max"), []byte(cpuMax), 0644); err != nil {
		return cleanup, fmt.Errorf("setting cpu max: %w", err)
	}

	if err := os.WriteFile(filepath.Join(cgroupPath, "pids.max"), []byte(pidsMax), 0644); err != nil {
		return cleanup, fmt.Errorf("setting pids max: %w", err)
	}

	if err := os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0644); err != nil {
		return cleanup, fmt.Errorf("setting cgroup.procs: %w", err)
	}
	return cleanup, nil
}

// runInsideNamespace is the code that runs AS PID 1 of the new namespace, after clone() but before we hand off to the real target program.
func runInsideNamespace(layersRoot string, args []string) {
	fmt.Printf("[pid namespace] my pid in here: %d (should be 1)\n", unix.Getpid())

	if err := makeMountsPrivate(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mount setup failed", err)
		os.Exit(1)
	}

	if err := unix.Sethostname([]byte("container")); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "set hostname failed:", err)
		os.Exit(1)
	}

	merged, err := mountOverlay(layersRoot)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "overlay mount failed:", err)
		os.Exit(1)
	}

	if err := pivotRoot(merged); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pivot root failed:", err)
		os.Exit(1)
	}

	if err := mountProc(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mount failed:", err)
		os.Exit(1)
	}

	fmt.Printf("[rootfs] pivoted into %s, about to run %v\n", merged, args)

	runAsInit(args)
}

// runAsInit starts the target command as a child instead of exec'ing
// directly into it, so this process stays PID 1 and can reap zombies.
// Any process that gets orphaned and reparented to PID 1 must be
// wait()'d or it lingers forever as a <defunct> entry.
func runAsInit(args []string) {
	child := exec.Command(args[0], args[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "starting", args, "failed:", err)
		os.Exit(1)
	}
	mainPid := child.Process.Pid

	exitCode := 0
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			break // ECHILD: no children left to reap
		}
		if pid == mainPid {
			exitCode = ws.ExitStatus()
		}
	}
	os.Exit(exitCode)
}

func makeMountsPrivate() error {
	return unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
}

func mountProc() error {
	return unix.Mount("proc", "/proc", "proc", 0, "")
}

// mountOverlay combines layersRoot/lower with layersRoot/upper into layersRoot/merged.
func mountOverlay(layersRoot string) (string, error) {
	lower := filepath.Join(layersRoot, "lower")
	upper := filepath.Join(layersRoot, "upper")
	work := filepath.Join(layersRoot, "work")
	merged := filepath.Join(layersRoot, "merged")

	for _, dir := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	absLower, err := filepath.Abs(lower)
	if err != nil {
		return "", err
	}
	absUpper, err := filepath.Abs(upper)
	if err != nil {
		return "", err
	}
	absWork, err := filepath.Abs(work)
	if err != nil {
		return "", err
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", absLower, absUpper, absWork)
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return "", fmt.Errorf("mounting overlay: %w", err)
	}
	return merged, nil
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
