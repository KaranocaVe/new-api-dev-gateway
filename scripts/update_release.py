#!/usr/bin/env python3
"""Verify exact official Linux assets before updating release.json; never execute them."""
import argparse
import hashlib
import json
import os
import pathlib
import re
import struct
import tempfile
import urllib.request

REPO = "https://api.github.com/repos/QuantumNous/new-api"
DOWNLOAD = "https://github.com/QuantumNous/new-api/releases/download/"


def semver(tag):
    # Follow upstream's main release lanes, not older branch-specific hotfixes.
    # Generic SemVer would rank rc.19-i18nfix.2 above rc.48 (text > numeric).
    match = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)(?:-(alpha|beta|rc)\.(\d+))?", tag)
    if not match:
        raise ValueError("unsupported release lane")
    major, minor, patch, lane, number = match.groups()
    rank = {"alpha": 0, "beta": 1, "rc": 2, None: 3}[lane]
    return int(major), int(minor), int(patch), lane is None, (rank, int(number or 0))


def exact_assets(release):
    tag = release["tag_name"]
    assets = {asset["name"]: asset for asset in release.get("assets", [])}
    result = {}
    for arch, name in {"amd64": f"new-api-{tag}", "arm64": f"new-api-arm64-{tag}"}.items():
        asset = assets.get(name)
        if not asset:
            return None
        if asset["browser_download_url"] != DOWNLOAD + tag + "/" + name:
            raise ValueError("unexpected official asset URL")
        result[arch] = asset
    return result


def candidate(releases, current, stable_only=False):
    choices = []
    for release in releases:
        if release.get("draft"):
            continue
        tag = release.get("tag_name", "")
        try:
            version = semver(tag)
        except ValueError:
            continue
        if stable_only and (not version[3] or release.get("prerelease")):
            continue
        if version > semver(current):
            choices.append(release)
    for release in sorted(choices, key=lambda r: semver(r["tag_name"]), reverse=True):
        if exact_assets(release):
            return release
        print("Skipping", release["tag_name"], "without both exact Linux assets")
    return None


def open_url(url):
    headers = {"User-Agent": "new-api-dev-gateway-release-check"}
    # Never send the API token to release-asset hosts or redirects.
    if url.startswith(REPO + "/") and os.environ.get("GH_TOKEN"):
        headers["Authorization"] = "Bearer " + os.environ["GH_TOKEN"]
    return urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=120)


def releases():
    result = []
    for page in range(1, 101):
        with open_url(f"{REPO}/releases?per_page=100&page={page}") as response:
            batch = json.load(response)
        result.extend(batch)
        if len(batch) < 100:
            return result
    raise ValueError("release pagination limit exceeded")


def verify_elf(header, arch):
    if len(header) < 20 or header[:6] != b"\x7fELF\x02\x01":
        raise ValueError("asset is not a little-endian ELF64 executable")
    if struct.unpack_from("<H", header, 18)[0] != {"amd64": 62, "arm64": 183}[arch]:
        raise ValueError("ELF machine does not match target architecture")


def verify_asset(asset, arch, checksum=None):
    digest = asset.get("digest", "")
    if digest and not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError("unsupported release digest")
    expected = digest.removeprefix("sha256:") or checksum
    if not expected:
        raise ValueError("release has neither GitHub SHA-256 nor official checksum")
    sha, header, count = hashlib.sha256(), b"", 0
    with open_url(asset["browser_download_url"]) as response:
        while chunk := response.read(1024 * 1024):
            if not header:
                header = chunk[:64]
            count += len(chunk)
            if count > 512 * 1024 * 1024:
                raise ValueError("unexpectedly large asset")
            sha.update(chunk)
    verify_elf(header, arch)
    actual = sha.hexdigest()
    if actual != expected or (checksum and actual != checksum):
        raise ValueError("official asset SHA-256 mismatch")
    if asset.get("size") is not None and count != asset["size"]:
        raise ValueError("official asset size mismatch")
    return {"url": asset["browser_download_url"], "sha256": actual}


def verified_lock(release):
    checksums = {}
    tag = release["tag_name"]
    for asset in release.get("assets", []):
        if asset["name"] == "checksums-linux.txt":
            if asset["browser_download_url"] != DOWNLOAD + tag + "/checksums-linux.txt":
                raise ValueError("unexpected checksums URL")
            with open_url(asset["browser_download_url"]) as response:
                raw = response.read(1024 * 1024).decode()
            for line in raw.splitlines():
                match = re.fullmatch(r"([0-9a-f]{64})\s+\*?([^/\s]+)", line)
                if match:
                    checksums[match[2]] = match[1]
    result = {}
    for arch, asset in exact_assets(release).items():
        result[arch] = verify_asset(asset, arch, checksums.get(asset["name"]))
    return {"tag": tag, "assets": result}


def update(path, all_releases, stable_only=False, check=False):
    current = json.loads(path.read_text())
    selected = candidate(all_releases, current["tag"], stable_only)
    if not selected:
        print("Already at highest compatible release:", current["tag"])
        return False
    new = verified_lock(selected)  # Finish both downloads before any mutation.
    if not check:
        with tempfile.NamedTemporaryFile("w", dir=path.parent, delete=False) as temp:
            name = pathlib.Path(temp.name)
            try:
                json.dump(new, temp, indent=2)
                temp.write("\n")
                temp.flush()
                os.fsync(temp.fileno())
            except BaseException:
                name.unlink(missing_ok=True)
                raise
        try:
            os.replace(name, path)
        finally:
            name.unlink(missing_ok=True)
    print("Verified", new["tag"], "for Linux amd64 and arm64")
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lock", type=pathlib.Path, default=pathlib.Path(__file__).resolve().parents[1] / "release.json")
    parser.add_argument("--stable-only", action="store_true", help="exclude both prerelease tags and GitHub prereleases")
    parser.add_argument("--check", action="store_true", help="verify but do not write; exit 1 if an update exists")
    args = parser.parse_args()
    changed = update(args.lock, releases(), args.stable_only, args.check)
    return 1 if args.check and changed else 0


if __name__ == "__main__":
    raise SystemExit(main())
