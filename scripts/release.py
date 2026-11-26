import argparse
import re
import stat
import zipfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
PLUGIN_ID = "omp-billion-context"
TARGETS = {"linux-amd64": "so", "darwin-arm64": "dylib", "windows-amd64": "dll"}


def version():
    value = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", value):
        raise ValueError("VERSION must contain one semantic release version without a v prefix")
    return value


def library_name(release, target):
    return f"{PLUGIN_ID}-v{release}.{TARGETS[target]}"


def archive_name(release, target):
    os_name, arch = target.split("-")
    return f"{PLUGIN_ID}_{release}_{os_name}_{arch}.zip"


def verify_archive(path, release, target):
    if path.name != archive_name(release, target):
        raise ValueError(f"unexpected release asset name: {path.name}")
    with zipfile.ZipFile(path) as archive:
        members = archive.infolist()
        expected = library_name(release, target)
        if len(members) != 1 or members[0].filename != expected or members[0].is_dir() or members[0].file_size == 0:
            raise ValueError(f"{path.name} must contain exactly one nonempty root member {expected}")
        file_type = stat.S_IFMT(members[0].external_attr >> 16)
        if file_type not in (0, stat.S_IFREG):
            raise ValueError(f"{path.name} library member must be a regular file")
        if archive.testzip() is not None:
            raise ValueError(f"corrupt release archive: {path.name}")


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("version")
    library = commands.add_parser("library")
    library.add_argument("--target", required=True, choices=TARGETS)
    tag = commands.add_parser("check-tag")
    tag.add_argument("--tag", required=True)
    package = commands.add_parser("package")
    package.add_argument("--target", required=True, choices=TARGETS)
    package.add_argument("--library", required=True, type=Path)
    package.add_argument("--dist", required=True, type=Path)
    verify = commands.add_parser("verify")
    verify.add_argument("--dist", required=True, type=Path)
    verify.add_argument("--tag")
    args = parser.parse_args()
    try:
        release = version()
        supplied_tag = getattr(args, "tag", None)
        if supplied_tag is not None and supplied_tag != f"v{release}":
            raise ValueError(f"tag must be v{release}, got {supplied_tag}")
        if args.command == "version":
            print(release)
        elif args.command == "library":
            print(library_name(release, args.target))
        elif args.command == "package":
            if args.library.name != library_name(release, args.target):
                raise ValueError(f"library must be named {library_name(release, args.target)}")
            args.dist.mkdir(parents=True, exist_ok=True)
            destination = args.dist / archive_name(release, args.target)
            with zipfile.ZipFile(destination, "w", compression=zipfile.ZIP_DEFLATED) as archive:
                archive.write(args.library, args.library.name)
            verify_archive(destination, release, args.target)
            print(destination)
        elif args.command == "verify":
            expected = {archive_name(release, target) for target in TARGETS}
            actual = {path.name for path in args.dist.glob("*.zip")}
            if actual != expected:
                raise ValueError(f"release assets differ: expected {sorted(expected)}, got {sorted(actual)}")
            for target in TARGETS:
                verify_archive(args.dist / archive_name(release, target), release, target)
            print(f"verified {release} release assets for {', '.join(TARGETS)}")
    except (OSError, ValueError, zipfile.BadZipFile) as error:
        parser.error(str(error))


if __name__ == "__main__":
    main()
