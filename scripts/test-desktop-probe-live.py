#!/usr/bin/env python3
r"""Run the single-host Desktop probe against a running, isolated Work API + MySQL.

Run as an OS test user with a working system credential store and no existing
Work pairing. The API needs an existing password-authenticated test admin.
The script removes its device and credential; Work may retain audit/task history.

python3 scripts/test-desktop-probe-live.py --work-url http://127.0.0.1:18089 \
    --probe /path/to/work-bridge-probe --admin test-admin

The password is prompted after the health check, or read from
PINABLE_E2E_ADMIN_PASSWORD. No Docker services or databases are provisioned.
"""

import argparse
import getpass
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
from http.cookiejar import CookieJar
from urllib import error, parse, request
from uuid import uuid4


class Failed(Exception):
    pass


def check(condition, message):
    if not condition:
        raise Failed(message)


class NoRedirect(request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


class Work:
    def __init__(self, url):
        parts = parse.urlsplit(url)
        try:
            local = ipaddress.ip_address(parts.hostname or "").is_loopback
            valid_port = 0 < (parts.port or 0) <= 65535
        except ValueError:
            local = valid_port = False
        check(local and valid_port and parts.scheme in ("http", "https") and
              parts.path in ("", "/") and parts.username is None and parts.password is None
              and "%" not in parts.netloc and "?" not in url and "#" not in url,
              "--work-url must be an explicit loopback IP and port with no path")
        self.url = url.rstrip("/")
        self.http = request.build_opener(request.ProxyHandler({}), NoRedirect(),
                                         request.HTTPCookieProcessor(CookieJar()))

    def call(self, method, path, body=None, expected=200):
        payload = json.dumps(body).encode() if body is not None else None
        req = request.Request(self.url + "/api/" + path, method=method, data=payload,
                              headers={"Content-Type": "application/json", "Origin": self.url})
        try:
            with self.http.open(req, timeout=5) as res:
                check(res.status == expected, f"{method} /api/{path}: HTTP {res.status}")
                return json.load(res)
        except error.HTTPError as exc:
            raise Failed(f"{method} /api/{path}: HTTP {exc.code}") from None
        except (OSError, ValueError) as exc:
            raise Failed(f"{method} /api/{path}: {exc}") from None

    def mine(self, name):
        return [r for r in self.call("GET", "runners?mine=1") if r["name"] == name]


def cli(args, mode, name, code=None):
    env = dict(os.environ)
    env.pop("PINABLE_E2E_ADMIN_PASSWORD", None)
    env.pop("PINABLE_WORK_BIND_CODE", None)
    if code:
        env["PINABLE_WORK_BIND_CODE"] = code
    return subprocess.Popen([args.probe, "-work-url", args.work_url, "-mode", mode,
                             "-name", name, "-timeout", "35s"], env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, encoding="utf-8")


def finish(process, label, reject=False):
    try:
        out, err = process.communicate(timeout=45)
    except subprocess.TimeoutExpired:
        process.kill()
        process.communicate()
        raise Failed(label + ": Desktop CLI timed out") from None
    if reject:
        check(process.returncode != 0 and "HTTP 401" in err,
              label + ": expected an explicit HTTP 401 for the saved session")
    else:
        # Desktop's public CLI diagnostics omit secrets; never log response bodies or env.
        check(process.returncode == 0, f"{label}: Desktop CLI exit {process.returncode}: {err.strip()}")
    return out


def matched_id(value, label):
    check(isinstance(value, str) and re.fullmatch(r"[a-zA-Z0-9_-]{1,128}", value),
          f"{label}: invalid ID")
    return value


def test(args):
    api = Work(args.work_url)
    health = api.call("GET", "health")
    check(health.get("status") == "ok" and health.get("role") == "api", "Work API is not ready")
    print("PASS Work API reachable over loopback TCP", flush=True)
    check(args.probe.is_file() and os.access(args.probe, os.X_OK), "Desktop probe binary unavailable")
    password = os.environ.get("PINABLE_E2E_ADMIN_PASSWORD") or getpass.getpass("Work test admin password: ")
    user = api.call("POST", "auth/password", {"login": args.admin, "password": password})["user"]
    check(user.get("loginName") == args.admin and user.get("isPlatformAdmin") is True,
          "authenticated user must be the specified test admin")
    name = "desktop-e2e-" + uuid4().hex
    check(not api.mine(name), "test device name already exists")
    print("PASS authenticated Work administrator session", flush=True)
    code = api.call("POST", "runners/bind-codes", {})["code"]
    check(isinstance(code, str) and code.startswith("bind-"), "invalid Work bind code")

    paired = revoked = False
    child = None
    try:
        out = finish(cli(args, "pair", name, code), "pair")
        paired = True
        rows = api.mine(name)
        check(len(rows) == 1 and rows[0]["status"] == "offline", "pair must leave one offline device")
        device = matched_id(rows[0]["id"], "pair")
        check(f'runnerId = "{device}"' in out, "Desktop and Work device IDs differ")
        print(f"PASS persistent pair and disconnect: {device}", flush=True)

        child = cli(args, "resume", name)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            rows = api.mine(name)
            if len(rows) == 1 and rows[0]["id"] == device and rows[0]["status"] == "online":
                break
            if child.poll() is not None:
                finish(child, "resume")
                raise Failed("resume exited before device became online")
            time.sleep(0.25)
        else:
            raise Failed("resume: original device did not become online")
        job = api.call("POST", "runners/tasks", {"runnerId": device, "provider": "pinable-desktop",
                                                "action": "desktop_probe_v1"}, expected=201)
        task = matched_id(job.get("id"), "enqueue")
        check(job.get("runnerId") == device and job.get("status") == "queued", "unexpected enqueue receipt")
        out = finish(child, "resume")
        child = None
        check(f'runnerId = "{device}"' in out and f'任务 "{task}" 已探测成功' in out,
              "Desktop did not report the expected device and completed task")
        check(api.call("GET", f"runners/tasks/{task}/status") == {"id": task, "status": "success"},
              "Work and Desktop terminal receipts disagree")
        print(f"PASS restarted CLI and matching Work / Desktop receipt: {task}", flush=True)

        check(api.call("DELETE", "runners/" + device).get("ok") is True,
              "Work did not acknowledge device removal")
        revoked = True
        out = finish(cli(args, "resume", name), "revoked resume", reject=True)
        check(f'runnerId = "{device}"' in out and not api.mine(name),
              "revoked device was replaced or registered again")
        print("PASS revoked credential received HTTP 401 and device stayed removed", flush=True)
    finally:
        if child is not None and child.poll() is None:
            child.kill()
            child.communicate()
        # Never forget a pre-existing pairing when pair was refused. The unique
        # device name also lets us clean up if the server registered it but the
        # CLI exited before its response was saved.
        in_failure = sys.exc_info()[0] is not None
        try:
            if not revoked:
                for row in api.mine(name):
                    api.call("DELETE", "runners/" + matched_id(row["id"], "cleanup"))
                check(not api.mine(name), f"test device {name} still exists after cleanup")
                revoked = True
            if paired:
                finish(cli(args, "forget", name), "forget")
        except (Failed, OSError, KeyError, TypeError, ValueError) as exc:
            if revoked:
                recovery = "remote removal confirmed; retry the CLI with -mode forget"
            elif paired:
                recovery = "remote removal unconfirmed; local credential retained. Retry the CLI with -mode unpair"
            else:
                recovery = "check this test device in Work; no local credential has been cleared"
            message = f"cleanup {name}: {exc}. {recovery} using the original test account and Work URL."
            if not in_failure:
                raise Failed(message) from exc
            print(message, file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--work-url", required=True)
    parser.add_argument("--probe", required=True, type=lambda s: Path(s).resolve())
    parser.add_argument("--admin", required=True)
    args = parser.parse_args()
    try:
        test(args)
    except (Failed, OSError, KeyError, EOFError, ValueError, TypeError) as exc:
        print(f"FAIL {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
