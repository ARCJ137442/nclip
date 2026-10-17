'''Exercise only self-created slugs on an isolated/staged nclip instance.

Uses the Python standard library. --large performs the 32-item/near-limit case;
run that under the real container memory limit before production replacement.
🤖 Agent-authored verification helper; not yet reviewed by a human.
⏪ Uses Python 3.10-compatible annotations for the existing deployment host runtime.
'''

from __future__ import annotations

import argparse
import concurrent.futures
import email.policy
import hashlib
import io
import http.client
import json
from email.message import Message
from email.parser import BytesParser
import urllib.error
import urllib.request
import urllib.parse
import uuid
import zipfile


HTTP_TIMEOUT: int = 30
'''Maximum seconds for a single verification request.'''
FixtureItem = tuple[str, str, list[tuple[str, bytes]]]
'''Logical kind, original filename, and MIME/bytes representations.'''


def main() -> None:
    '''Run the staged instance checks and remove only slugs created by this run.'''
    parser = argparse.ArgumentParser()
    parser.add_argument("base")
    parser.add_argument("--large", action="store_true")
    args = parser.parse_args()
    base = args.base.rstrip("/")
    created: set[str] = set()

    def request(
        path: str,
        method: str = 'GET',
        body: bytes | None = None,
        headers: dict[str, str] | None = None,
    ) -> tuple[int, Message, bytes]:
        req = urllib.request.Request(
            base + path, data=body, method=method, headers=headers if headers else {}
        )
        try:
            with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT) as response:
                return response.status, response.headers, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.headers, error.read()

    def create(items: list[FixtureItem], burn: bool = False) -> str:
        boundary = "nclip-" + uuid.uuid4().hex
        fields, manifest = [], {"version": 1, "items": []}
        for index, item in enumerate(items):
            entry = {"kind": item[0], "name": item[1], "parts": []}
            for part_index, (mime, data) in enumerate(item[2]):
                key = f"p{index}-{part_index}"
                entry["parts"].append({"key": key, "mime": mime})
                fields.append((key, data))
            manifest["items"].append(entry)
        fields.append(("manifest", json.dumps(manifest).encode()))
        body = b"".join(
            b"--"
            + boundary.encode()
            + b'\r\nContent-Disposition: form-data; name="'
            + key.encode()
            + b'"\r\n\r\n'
            + data
            + b"\r\n"
            for key, data in fields
        )
        body += b"--" + boundary.encode() + b"--\r\n"
        status, _, result = request(
            "/api/v1/shares",
            "POST",
            body,
            {
                "Content-Type": f"multipart/form-data; boundary={boundary}",
                "X-Burn": str(burn).lower(),
            },
        )
        assert status == 201, (status, result[:300])
        result = json.loads(result)
        assert isinstance(result['slug'], str)
        created.add(result["slug"])
        return result["slug"]

    def content(slug: str, burn: bool = False) -> dict[str, bytes]:
        suffix = "/receive" if burn else "/content"
        status, headers, data = request(
            f"/api/v1/shares/{slug}{suffix}",
            "POST" if burn else "GET",
            headers={"X-Nclip-Confirm": "1"} if burn else None,
        )
        assert status == 200, (status, data[:300])
        message = BytesParser(policy=email.policy.default).parsebytes(
            b"MIME-Version: 1.0\r\nContent-Type: "
            + headers["Content-Type"].encode()
            + b"\r\n\r\n"
            + data
        )
        fields: dict[str, bytes] = {}
        for part in message.iter_parts():
            name = part.get_param('name', header='Content-Disposition')
            payload = part.get_payload(decode=True)
            assert isinstance(name, str) and isinstance(payload, bytes)
            fields[name] = payload
        return fields

    def check_oversize_rejection() -> None:
        '''Keep the early HTTP response even if the server closes during upload.'''
        target = urllib.parse.urlsplit(base)
        connection = http.client.HTTPConnection(target.hostname, target.port, timeout=HTTP_TIMEOUT)
        try:
            try:
                connection.request(
                    'POST',
                    '/api/v1/shares',
                    b'x' * (11 * 1024 * 1024),
                    {'Content-Type': 'multipart/form-data; boundary=oversize'},
                )
            except (BrokenPipeError, ConnectionResetError):
                # A valid early 413 can arrive while the OS is still sending the body.
                pass
            response = connection.getresponse()
            assert response.status == 413, response.status
            response.read()
        finally:
            connection.close()

    try:
        assert request("/health")[0] == 200
        status, _, data = request("/api/v1/ui-config")
        config = json.loads(data)
        assert (
            status == 200
            and config["upload_max_bytes"] == 10485760
            and config["default_ttl_seconds"] == 259200
        )
        text = "  中文与换行\n\n".encode()
        slug = create([("clipboard", "text.txt", [("text/plain", text)])])
        assert request("/raw/" + slug)[2] == text
        assert request("/" + slug, headers={"User-Agent": "curl"})[2] == text
        assert content(slug)["p0-0"] == text
        print("PASS text bytes, raw and CLI compatibility")

        items = [
            ("clipboard", "rich.html", [("text/plain", text), ("text/html", b"<b>hello</b>")]),
            ("file", "报告.txt", [("text/plain", b"report")]),
        ]
        slug = create(items, burn=True)
        assert request("/" + slug, headers={"Accept": "text/html"})[0] == 200
        assert request(f"/api/v1/shares/{slug}/content")[0] == 409
        assert request(f"/api/v1/shares/{slug}")[0] == 200
        fields = content(slug, burn=True)
        assert fields["p0-0"] == text and fields["p1-0"] == b"report"
        with zipfile.ZipFile(io.BytesIO(fields["archive"])) as archive:
            assert "manifest.json" in archive.namelist()
        assert request(f"/api/v1/shares/{slug}")[0] == 404
        print("PASS rich batch, confirmation, deletion and retained ZIP")

        slug = create([("clipboard", "once.txt", [("text/plain", b"one winner")])], burn=True)

        def race(index: int) -> int:
            if index % 2:
                return request("/raw/" + slug)[0]
            return request(
                f"/api/v1/shares/{slug}/receive", "POST", headers={"X-Nclip-Confirm": "1"}
            )[0]

        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
            codes = list(executor.map(race, range(20)))
        assert codes.count(200) == 1 and all(code in (200, 404) for code in codes), codes
        print("PASS 20 competing destructive readers, one winner")

        if args.large:
            payload = b"x" * (300 * 1024)
            batch = [
                ("file", f"item-{i}.bin", [("application/octet-stream", payload)])
                for i in range(32)
            ]
            slug = create(batch, burn=True)
            fields = content(slug, burn=True)
            assert len(fields) == 34  # manifest, 32 parts, original ZIP
            digest = hashlib.sha256(payload).digest()
            assert all(hashlib.sha256(fields[f"p{i}-0"]).digest() == digest for i in range(32))
            assert request("/health")[0] == 200
            check_oversize_rejection()
            print("PASS 32 items / 9.375 MiB one-time batch and 11 MiB rejection")
    finally:
        for slug in created:
            request("/" + slug, "DELETE")


if __name__ == "__main__":
    main()
