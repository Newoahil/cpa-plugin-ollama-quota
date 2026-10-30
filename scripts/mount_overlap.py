#!/usr/bin/env python3
"""Report whether a directory overlaps an existing container's bind mounts.

    mount_overlap.py <dir> [<alternate-form-of-dir>]

Reads ``docker inspect --format`` output from stdin, one container per line::

    /name|source1|source2|...

Exits 0 and prints one line per overlapping mount when ``<dir>`` equals, contains,
or is contained by a mount source; exits 1 when nothing overlaps.

Why this exists: ``mktemp -d`` proves the run gets a brand-new leaf directory, but
that alone does not prove the directory is outside an already-running CPA's bind
mount. If a CPA mounted a parent directory, a fresh temp dir inside it would be
visible to that CPA. Comparing against every running container's mount sources is
the check that closes that gap. It is read-only: it inspects, never mutates.

Comparison is on normalized absolute paths. Separators are unified, Windows drive
letters are lowercased, and trailing separators are dropped, because the same
location is spelled differently by Git Bash (``/c/...``), by Docker on Windows
(``C:\\...``), and by Docker Desktop's Linux backend (``/run/desktop/mnt/host/c/...``).
Both the given form and the alternate form are tried so at least one of them can
match whatever spelling Docker reports.
"""

import os
import sys


def normalize(path):
    if not path:
        return ""
    value = path.strip().strip('"').replace("\\", "/")
    # Docker Desktop's Linux backend rewrites Windows paths under this prefix.
    prefix = "/run/desktop/mnt/host/"
    if value.startswith(prefix):
        value = "/" + value[len(prefix):]
    # A Git Bash drive path (/c/foo) and a Windows drive path (C:/foo) name the
    # same place; fold both to the drive-letter form before comparing.
    if len(value) > 2 and value[0] == "/" and value[2] == "/" and value[1].isalpha():
        value = value[1].upper() + ":" + value[2:]
    if len(value) >= 2 and value[1] == ":":
        value = value[0].lower() + value[1:]
    value = os.path.normpath(value).replace("\\", "/")
    if value == ".":
        return ""
    return value.rstrip("/") if value != "/" else value


def overlaps(a, b):
    if not a or not b:
        return False
    if a == b:
        return True
    return a.startswith(b + "/") or b.startswith(a + "/")


def main():
    if len(sys.argv) < 2:
        print("usage: mount_overlap.py <dir> [<alternate-form>]", file=sys.stderr)
        return 2

    candidates = []
    for raw in sys.argv[1:]:
        normalized = normalize(raw)
        if normalized and normalized not in candidates:
            candidates.append(normalized)

    hits = []
    for line in sys.stdin.read().splitlines():
        line = line.strip()
        if not line:
            continue
        parts = line.split("|")
        container = parts[0] or "?"
        for source in parts[1:]:
            normalized_source = normalize(source)
            if not normalized_source:
                continue
            for candidate in candidates:
                if overlaps(candidate, normalized_source):
                    hits.append(
                        "{} mount source {} overlaps {}".format(
                            container, source.strip(), candidate
                        )
                    )
                    break

    if hits:
        for hit in hits:
            print(hit)
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
