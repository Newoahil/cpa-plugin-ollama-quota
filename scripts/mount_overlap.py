#!/usr/bin/env python3
"""Report whether a directory overlaps an existing container's bind mounts.

    mount_overlap.py <dir> [<alternate-form-of-dir>]

Reads ``docker inspect --format`` output from stdin, one container per line::

    /name|source1|source2|...

Exit codes are part of the contract with scripts/verify-hotswap.sh:

    0  at least one overlap; one line per hit on stdout
    1  the check ran and found no overlap
    2  usage error
    3  internal error (anything unexpected)

The caller must only treat 1 as "safe to continue". The separate error codes
exist because "the check could not run" and "the check found nothing" are
different facts, and conflating them would turn a broken safety check into a
silent pass.

Why this exists: ``mktemp -d`` proves the run gets a brand-new leaf directory,
but that alone does not prove the directory is outside an already-running CPA's
bind mount. If a CPA mounted a parent directory, a fresh temp dir inside it
would be visible to that CPA. Comparing against every running container's mount
sources is the check that closes that gap. It is read-only: it inspects, never
mutates.

Comparison is on normalized absolute paths. The same location is spelled
differently by Git Bash (``/c/...``), by Docker on Windows (``C:\\...``), and by
Docker Desktop's Linux backend (``/run/desktop/mnt/host/c/...``), so all three
are folded to one form before comparing. Both the given form and the alternate
form are passed in so at least one can match whatever spelling Docker reports.

Two properties are easy to get wrong here and both are load-bearing:

* ``/`` is an ancestor of every absolute path. Naive prefix matching compares
  against ``"//"`` and therefore misses every overlap under the filesystem root.
* Windows paths are case-insensitive. Default Windows filesystems treat
  ``C:/Temp/Test`` and ``c:/temp`` as the same tree, so a case-sensitive compare
  would report "no overlap" for a directory the container can plainly see.
  POSIX paths keep case-sensitive semantics.
"""

import os
import sys

EXIT_OVERLAP = 0
EXIT_NO_OVERLAP = 1
EXIT_USAGE = 2
EXIT_ERROR = 3

# Docker Desktop's Linux backend rewrites Windows paths under this prefix.
_DESKTOP_WINDOWS_PREFIX = "/run/desktop/mnt/host/"


def normalize(path):
    """Fold one path spelling to a comparable absolute form.

    Returns "" for input that names nothing usable (empty, or "." after
    normalization). A root path normalizes to "/" and stays "/" — never to "".
    """
    if path is None:
        return ""
    value = path.strip().strip('"').replace("\\", "/")
    if not value:
        return ""

    # Docker Desktop's Linux backend spells a Windows location with this prefix.
    if value.startswith(_DESKTOP_WINDOWS_PREFIX):
        value = "/" + value[len(_DESKTOP_WINDOWS_PREFIX):]

    # A Git Bash drive path (/c/foo) and a Windows drive path (C:/foo) name the
    # same place; fold both to the drive-letter form before comparing.
    if len(value) > 2 and value[0] == "/" and value[2] == "/" and value[1].isalpha():
        value = value[1].upper() + ":" + value[2:]

    value = os.path.normpath(value).replace("\\", "/")
    # normpath can leave doubled separators (notably for "//"), and a doubled
    # root separator would break every ancestor comparison below.
    while "//" in value:
        value = value.replace("//", "/")

    if value == ".":
        return ""
    if value == "/":
        return "/"
    return value.rstrip("/") or "/"


def is_windows(path):
    """Report whether a normalized path is a Windows drive path (C:/...)."""
    return len(path) >= 2 and path[1] == ":"


def comparison_key(path):
    """The form actually compared: Windows paths lowercased, POSIX untouched.

    Windows drive paths are case-insensitive because the default filesystem is;
    POSIX paths are not, so they keep their case. Doing this on the whole path
    (not just the drive letter) is what makes C:/Temp/Test and c:/temp match.
    """
    return path.lower() if is_windows(path) else path


def _is_within(parent, child):
    """True when `child` is `parent` itself or sits under it."""
    if parent == "/":
        # The filesystem root is an ancestor of every absolute path.
        return child.startswith("/")
    return child == parent or child.startswith(parent + "/")


def overlaps(a, b):
    """True when two normalized absolute paths denote the same tree or nest."""
    if not a or not b:
        return False
    return _is_within(comparison_key(a), comparison_key(b)) or _is_within(
        comparison_key(b), comparison_key(a)
    )


def evaluate(candidates, lines):
    """Return the list of hit descriptions for candidate paths against lines."""
    hits = []
    for line in lines:
        line = line.strip()
        if not line:
            continue
        parts = line.split("|")
        container = parts[0].strip() or "?"
        for source in parts[1:]:
            normalized_source = normalize(source)
            if not normalized_source:
                continue
            for candidate in candidates:
                if overlaps(candidate, normalized_source):
                    hits.append(
                        "{} mount source {} overlaps {}".format(
                            parts[0].strip() or "?", source.strip(), candidate
                        )
                    )
                    break
    return hits


def main(argv):
    if len(argv) < 2:
        print("usage: mount_overlap.py <dir> [<alternate-form>]", file=sys.stderr)
        return EXIT_USAGE

    candidates = []
    for raw in argv[1:]:
        normalized = normalize(raw)
        if normalized and normalized not in candidates:
            candidates.append(normalized)
    if not candidates:
        print("mount_overlap.py: no usable candidate directory was given", file=sys.stderr)
        return EXIT_USAGE

    hits = evaluate(candidates, sys.stdin.read().splitlines())
    if hits:
        for hit in hits:
            print(hit)
        return EXIT_OVERLAP
    return EXIT_NO_OVERLAP


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv))
    except SystemExit:
        raise
    except BaseException as error:  # noqa: BLE001 - must never look like "no overlap"
        # An unexpected failure must not decay into exit 1 ("no overlap"): the
        # caller treats 1 as "proven safe". Report it as an error instead.
        print("mount_overlap.py: internal error: %s" % (error,), file=sys.stderr)
        sys.exit(EXIT_ERROR)
