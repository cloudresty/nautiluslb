#!/usr/bin/env python3
"""Histogram of the buffer sizes of every pipe a process holds.

Run in a container sharing the host PID namespace (see README):
  docker run --rm --privileged --pid host -v $PWD/test/perf:/p NETSHOOT python3 /p/pipesizes.py PID

NautilusLB's splice path holds one pipe per direction for the life of a
connection (Go's internal/poll keeps it for the whole TCPConn.ReadFrom), and
Go asks for 1 MiB pipes. An unprivileged user (NautilusLB runs as UID 65532)
whose pipes exceed fs.pipe-user-pages-soft (16384 pages = 64 MiB) only gets
small pipes from then on, which this shows.
"""
import collections
import fcntl
import os
import sys

F_GETPIPE_SZ = 1032
pid = sys.argv[1]
sizes = collections.Counter()
seen = set()
for fd in os.listdir(f"/proc/{pid}/fd"):
    path = f"/proc/{pid}/fd/{fd}"
    try:
        target = os.readlink(path)
    except OSError:
        continue
    if not target.startswith("pipe:") or target in seen:
        continue
    seen.add(target)
    try:
        f = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
    except OSError:
        try:
            f = os.open(path, os.O_WRONLY | os.O_NONBLOCK)
        except OSError:
            continue
    try:
        sizes[fcntl.fcntl(f, F_GETPIPE_SZ)] += 1
    finally:
        os.close(f)
print("pipes:", sum(sizes.values()))
for size, n in sorted(sizes.items()):
    print(f"  {size // 1024:>5} KiB x {n}")
