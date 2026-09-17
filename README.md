# container-from-scratch

A minimal container runtime written in Go, built to understand what Docker
actually does under the hood. No container libraries, no image registry, no
daemon. Just the Linux kernel primitives Docker is built on: namespaces,
cgroups, and pivot_root, wired together by hand.

## What this is

Docker looks like magic until you take it apart. This project takes it
apart. Each stage adds exactly one kernel mechanism on top of the last,
so by the end you have a working, from-scratch demonstration of:

- Process isolation via PID namespaces
- Filesystem isolation via mount namespaces and a private `/proc`
- Hostname isolation via UTS namespaces
- A private root filesystem via `pivot_root`
- Memory, CPU, and process-count limits via cgroups v2
- Network isolation via network namespaces
- Copy-on-write layering via OverlayFS
- Zombie process reaping, running as PID 1

## Requirements

- Linux, with cgroups v2 (the unified hierarchy). Most modern distros
  ship this by default.
- Root, or `CAP_SYS_ADMIN` at minimum. Namespace and mount syscalls
  require it.
- Go 1.21 or later.
- `busybox-static`, for building a minimal root filesystem to run inside
  the container.

## Setup

Build the layered root filesystem the container will pivot into:

```bash
sudo apt install busybox-static

mkdir -p layers/lower/bin layers/lower/proc layers/upper layers/work layers/merged
cp "$(which busybox)" layers/lower/bin/busybox
cd layers/lower/bin
for cmd in sh ls ps mount cat echo hostname ip ping; do
  ln -sf busybox "$cmd"
done
cd -
```

`layers/lower` is the read-only base image. `layers/upper` is where the
container's own writes land. `layers/work` is scratch space OverlayFS
needs internally. None of this is committed; `.gitignore` excludes it.

Build the binary:

```bash
go build -o container-from-scratch .
```

## Usage

```bash
sudo ./container-from-scratch ./layers
sudo ./container-from-scratch ./layers /bin/ls /
```

With no command given, it defaults to `/bin/sh`, an interactive shell
inside the container.

## What you get

Inside the container:

- `echo $$` reports PID 1, isolated from every process on the host
- `ps aux` shows only processes inside the container
- `hostname` reports a name independent of the host
- `ls /` shows only the layered root filesystem, not the host's
- Memory use above 20 MB gets OOM-killed
- CPU use is throttled to roughly 10 percent of one core
- Process count is capped at 64
- `ip addr` shows only a loopback interface, and it starts down
- Writes go to the upper layer only; the lower layer is never touched

## How it works

The binary re-executes itself through `/proc/self/exe` with
`CLONE_NEWPID`, `CLONE_NEWNS`, `CLONE_NEWUTS`, and `CLONE_NEWNET` set on
the child process. That child is born directly into the new namespaces
and becomes PID 1 inside them. From there it:

1. Makes its mount table private, so nothing it mounts leaks back to
   the host
2. Sets a container-local hostname
3. Mounts an OverlayFS combining the read-only base layer with a
   writable layer
4. Calls `pivot_root` into that merged view, replacing its filesystem
   root entirely and unmounting the old one
5. Mounts a fresh `/proc`, bound to its own PID namespace
6. Starts the target command as a child rather than exec'ing into it
   directly, so it can stay PID 1 and reap any process that gets
   orphaned onto it

Resource limits are applied from outside the namespaces, on the host
side: a cgroup is created, memory, CPU, and process-count limits are
written into it, and the container's real PID is added before it does
any work.

## What is deliberately out of scope

This is a learning project, not a Docker replacement. Left out on
purpose:

- Image registries or pulling images. You build the root filesystem by
  hand.
- A long-running daemon or REST API. This runs one container and exits.
- Real bridge networking and iptables NAT. The network namespace is
  wired up and proven isolated; connecting it to the outside world with
  a veth pair is documented as a manual exercise, not automated.
- User namespaces. Everything here runs as real root, not a mapped,
  unprivileged one.

## Project structure

```
container.go     the entire runtime, one file
layers/          generated at setup time, not committed
  lower/         read-only base filesystem
  upper/         container writes land here
  work/          OverlayFS internal scratch space
  merged/        the combined view the container is pivoted into
```
