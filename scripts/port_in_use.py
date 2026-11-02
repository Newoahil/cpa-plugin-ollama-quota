#!/usr/bin/env python3
"""Report whether a TCP port is already in use.

Exits 0 when something is listening on the port, 1 when it is free. It first
tries a connect, then reads the listen table, because Git Bash on Windows can
report a live listener as free and mangles netstat output piped through grep.
Written for the standard library only: scripts/verify-hotswap.sh requires
python3 and nothing else.
"""

import re
import socket
import subprocess
import sys


def listening(port):
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(1.0)
    try:
        sock.connect(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        sock.close()


def in_listen_table(port):
    pattern = re.compile(r"[:.]" + str(port) + r"\s")
    for command in (["ss", "-ltn"], ["netstat", "-ano"]):
        try:
            raw = subprocess.run(command, capture_output=True, timeout=20).stdout
        except (OSError, subprocess.SubprocessError):
            continue
        for line in raw.decode("latin-1").splitlines():
            upper = line.upper()
            if ("LISTEN" in upper) and pattern.search(line):
                return True
    return False


def main():
    if len(sys.argv) != 2:
        print("usage: port_in_use.py <port>", file=sys.stderr)
        return 2
    port = int(sys.argv[1])
    return 0 if (listening(port) or in_listen_table(port)) else 1


if __name__ == "__main__":
    sys.exit(main())
