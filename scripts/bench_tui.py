#!/usr/bin/env python3
"""Measure what an open, untouched dashboard costs per second.

This is the number that matters most for jobtail: the TUI is a process people
leave in a tab for hours, so its steady-state cost is paid whether or not
anyone is looking at it. `go test -bench` can't see it, because the cost comes
from the real program's refresh loop driving a real terminal.

So this runs the actual binary in a real pty at a real size against a real
data directory, and reports CPU per second and peak RSS.

Two details make the numbers trustworthy:

  * CPU comes from wait4's rusage, in microseconds. /proc/<pid>/stat counts in
    10 ms clock ticks, far too coarse here, and /proc/<pid>/schedstat reads
    near-zero on kernels with sched_schedstats=0 (most of them).
  * Startup cost is cancelled by running twice for different durations and
    differencing: cpu(T) = startup + T*rate, so rate = Δcpu/ΔT.

It also answers the terminal capability probes (OSC 11, CSI 6n, DA1) a real
terminal would answer. Without that the program blocks at startup waiting for
a reply and never draws anything at all.

Usage: bench_tui.py BINARY DATADIR [WIDTH HEIGHT]
"""
import fcntl
import os
import pty
import select
import signal
import struct
import sys
import termios
import time

SHORT_RUN = 4.0   # seconds
LONG_RUN = 24.0   # seconds


def run(binary, datadir, width, height, secs):
    """Run the TUI for secs seconds; return (cpu_ms, peak_rss_kb, bytes_out)."""
    pid, fd = pty.fork()
    if pid == 0:
        env = dict(
            os.environ,
            JOBTAIL_DATA_DIR=datadir,
            TERM="xterm-256color",
            COLUMNS=str(width),
            LINES=str(height),
            # Point the update check at a closed port so no part of this
            # measurement is network latency.
            JOBTAIL_UPDATE_API="http://127.0.0.1:9",
        )
        os.execve(binary, [binary, "tui"], env)

    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
    nbytes, peak_rss, deadline = 0, 0, time.time() + secs
    while time.time() < deadline:
        readable, _, _ = select.select([fd], [], [], 0.2)
        if readable:
            try:
                chunk = os.read(fd, 1 << 20)
            except OSError:
                break
            nbytes += len(chunk)
            if b"\x1b]11;?" in chunk:            # background color query
                os.write(fd, b"\x1b]11;rgb:0c0c/0c0c/0c0c\x1b\\")
            if b"\x1b[6n" in chunk:              # cursor position report
                os.write(fd, b"\x1b[1;1R")
            if b"\x1b[c" in chunk or b"\x1b[0c" in chunk:  # device attributes
                os.write(fd, b"\x1b[?62;1;2;6;9;15;22c")
        try:
            with open(f"/proc/{pid}/status") as f:
                for line in f:
                    if line.startswith("VmHWM:"):
                        peak_rss = max(peak_rss, int(line.split()[1]))
                        break
        except FileNotFoundError:
            break

    os.kill(pid, signal.SIGKILL)
    _, _, ru = os.wait4(pid, 0)
    os.close(fd)
    return (ru.ru_utime + ru.ru_stime) * 1000.0, peak_rss, nbytes


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    binary, datadir = sys.argv[1], sys.argv[2]
    width = int(sys.argv[3]) if len(sys.argv) > 3 else 172
    height = int(sys.argv[4]) if len(sys.argv) > 4 else 40

    cpu1, rss1, out1 = run(binary, datadir, width, height, SHORT_RUN)
    cpu2, rss2, out2 = run(binary, datadir, width, height, LONG_RUN)
    rate = (cpu2 - cpu1) / (LONG_RUN - SHORT_RUN)
    written = (out2 - out1) / (LONG_RUN - SHORT_RUN)
    print(
        f"{rate:6.2f} ms CPU/s   {max(rss1, rss2) / 1024:6.1f} MiB peak RSS   "
        f"{written:7.1f} B/s to terminal"
    )


if __name__ == "__main__":
    main()
