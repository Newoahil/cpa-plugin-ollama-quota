#!/usr/bin/env python3
"""Package a versioned Linux/arm64 plugin with a SHA-256 of the whole ZIP.

    python scripts/package-release.py --version v0.3.1
    python scripts/package-release.py --version v0.3.1 --library dist/build/ollama-quota-v0.3.1.so

Uses only the Python standard library. The ZIP contains just the shared library
at its root; checksums.txt stays beside it. Use a fresh output directory for each
run so an old package or checksum can never be silently overwritten.
"""

import argparse
import hashlib
from pathlib import Path
import re
import struct
import sys
import zipfile


ROOT = Path(__file__).resolve().parent.parent
VERSION_PATTERN = re.compile(
    r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
    r"(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
)


def plugin_metadata(version):
    source = (ROOT / "internal/quotaplugin/plugin.go").read_text(encoding="utf-8")
    values = {}
    for key in ("Name", "Version"):
        matches = re.findall(r'^\s*' + key + r'\s*=\s*"([^"\n]+)"\s*$', source, re.M)
        if len(matches) != 1:
            raise ValueError("expected one %s constant in plugin.go" % key)
        values[key] = matches[0]
    if not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", values["Name"]):
        raise ValueError("plugin Name is not a safe artifact name")
    if not VERSION_PATTERN.fullmatch(values["Version"]):
        raise ValueError("plugin Version must be a semantic version without a v prefix")
    requested = version.removeprefix("v")
    if requested != values["Version"]:
        raise ValueError("requested version %r does not match plugin Version %s" % (version, values["Version"]))
    return values["Name"], values["Version"]


def check_library(path, expected_name):
    if path.name != expected_name:
        raise ValueError("library filename must be %s" % expected_name)
    payload = path.read_bytes()
    if len(payload) < 64 or payload[:7] != b"\x7fELF\x02\x01\x01":
        raise ValueError("library must be a little-endian ELF64 file")
    elf_type, machine, elf_version = struct.unpack_from("<HHI", payload, 16)
    entry, program_offset = struct.unpack_from("<QQ", payload, 24)
    header_size, program_size, program_count = struct.unpack_from("<HHH", payload, 52)
    if machine != 183:
        raise ValueError("library must target arm64 (ELF EM_AARCH64=183), got %d" % machine)
    if elf_type != 3 or entry != 0:
        raise ValueError("library must be an ELF shared library (ET_DYN, no entry point)")
    if (elf_version != 1 or header_size != 64 or program_size != 56
            or program_count == 0 or program_offset < header_size
            or program_offset + program_size * program_count > len(payload)):
        raise ValueError("library has an invalid or truncated ELF header")
    return payload


def package(version, library, output_dir):
    name, version = plugin_metadata(version)
    library_name = "%s-v%s.so" % (name, version)
    library = library if library is not None else ROOT / "dist" / library_name
    payload = check_library(library, library_name)
    license_text = (ROOT / "LICENSE").read_bytes()

    output_dir.mkdir(parents=True, exist_ok=True)
    if any(output_dir.iterdir()):
        raise ValueError("output directory must be empty: %s" % output_dir)
    archive_path = output_dir / ("%s_%s_linux_arm64.zip" % (name, version))
    # Fixed metadata makes the same input produce the same archive on reruns.
    member = zipfile.ZipInfo(library_name, date_time=(1980, 1, 1, 0, 0, 0))
    member.create_system = 3
    member.external_attr = 0o100755 << 16
    with zipfile.ZipFile(archive_path, "x") as archive:
        archive.comment = license_text
        archive.writestr(member, payload, compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)
    digest = hashlib.sha256(archive_path.read_bytes()).hexdigest()
    with (output_dir / "checksums.txt").open("x", encoding="utf-8", newline="\n") as checksum:
        checksum.write("%s  %s\n" % (digest, archive_path.name))
    print("packaged: %s" % archive_path)
    print("%s  %s" % (digest, archive_path.name))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="release version; must match plugin.go (v prefix optional)")
    parser.add_argument("--check-version", action="store_true", help="only check the source version and print the expected library filename")
    parser.add_argument("--library", type=Path, help="versioned .so; defaults to dist/<Name>-v<Version>.so")
    parser.add_argument("--output-dir", type=Path, default=ROOT / "dist/release", help="fresh package directory (default: dist/release)")
    args = parser.parse_args()
    try:
        if args.check_version:
            name, version = plugin_metadata(args.version)
            print("%s-v%s.so" % (name, version))
        else:
            package(args.version, args.library, args.output_dir)
    except (OSError, ValueError, zipfile.BadZipFile) as error:
        print("package-release: %s" % error, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
