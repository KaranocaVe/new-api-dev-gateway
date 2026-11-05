import hashlib
import importlib.util
import io
import json
import pathlib
import struct
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("updater", pathlib.Path(__file__).resolve().parents[1] / "scripts/update_release.py")
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)


def release(tag, arches=("amd64", "arm64")):
    assets = []
    for arch in arches:
        name = ("new-api-arm64-" if arch == "arm64" else "new-api-") + tag
        assets.append({"name": name, "browser_download_url": updater.DOWNLOAD + tag + "/" + name})
    return {"tag_name": tag, "assets": assets, "prerelease": False}


def elf(arch):
    value = bytearray(64)
    value[:6] = b"\x7fELF\x02\x01"
    struct.pack_into("<H", value, 18, {"amd64": 62, "arm64": 183}[arch])
    return bytes(value)


class UpdaterTests(unittest.TestCase):
    def test_order_and_policy(self):
        self.assertLess(updater.semver("v1.0.0-rc.9"), updater.semver("v1.0.0-rc.48"))
        self.assertLess(updater.semver("v1.0.0-rc.48"), updater.semver("v1.0.0"))
        self.assertIsNone(updater.candidate([release("v1.0.0-rc.19-i18nfix.2")], "v1.0.0-rc.48"))
        self.assertLess(updater.semver("v1.0.0-beta.99"), updater.semver("v1.0.0-rc.1"))
        options = [release("v1.0.0-rc.49"), release("v1.0.0"), release("v1.1.0", ("amd64",))]
        self.assertEqual(updater.candidate(options, "v1.0.0-rc.48")["tag_name"], "v1.0.0")
        self.assertIsNone(updater.candidate(options, "v1.1.0"))
        self.assertIsNone(updater.candidate([release("v1.0.0-rc.49")], "v1.0.0-rc.48", True))

    def test_no_linux_macos_confusion(self):
        item = release("v1.0.1", ("amd64",))
        item["assets"].append({"name": "new-api-macos-v1.0.1", "browser_download_url": updater.DOWNLOAD + "v1.0.1/new-api-macos-v1.0.1"})
        self.assertIsNone(updater.exact_assets(item))
        item = release("v1.0.1")
        item["assets"][0]["browser_download_url"] = "https://evil.example/file"
        with self.assertRaises(ValueError):
            updater.exact_assets(item)

    def test_verified_bytes_and_arch(self):
        asset = release("v1.0.1")["assets"][0]
        data = elf("amd64")
        asset["digest"] = "sha256:" + hashlib.sha256(data).hexdigest()
        with patch.object(updater, "open_url", return_value=io.BytesIO(data)):
            self.assertEqual(updater.verify_asset(asset, "amd64")["sha256"], asset["digest"][7:])
        with patch.object(updater, "open_url", return_value=io.BytesIO(data)):
            with self.assertRaises(ValueError):
                updater.verify_asset(asset, "arm64")
        with patch.object(updater, "open_url", return_value=io.BytesIO(data)):
            with self.assertRaises(ValueError):
                updater.verify_asset(asset, "amd64", "0" * 64)
        with self.assertRaises(ValueError):
            updater.verify_elf(b"Mach-O", "arm64")

    def test_failure_never_changes_lock(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "release.json"
            path.write_text(json.dumps({"tag": "v1.0.0"}))
            before = path.read_bytes()
            with patch.object(updater, "verified_lock", side_effect=ValueError("wrong arch")):
                with self.assertRaises(ValueError):
                    updater.update(path, [release("v1.0.1")])
            self.assertEqual(path.read_bytes(), before)
            with patch.object(updater, "verified_lock", return_value={"tag": "v1.0.1"}):
                self.assertTrue(updater.update(path, [release("v1.0.1")], check=True))
                self.assertEqual(path.read_bytes(), before)
                self.assertTrue(updater.update(path, [release("v1.0.1")]))
                self.assertEqual(json.loads(path.read_text())["tag"], "v1.0.1")


if __name__ == "__main__":
    unittest.main()
