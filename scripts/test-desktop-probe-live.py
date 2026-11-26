#!/usr/bin/env python3
r"""Run the single-host Desktop probe against a running, isolated Work API.

The default persistent test needs a dedicated OS test user with a working
system credential store and no existing Work pairing. Add --temporary for a
single CLI session that uses an in-memory pairing. Both modes need a
password-authenticated Work test admin and remove their own device afterward.
Work may retain audit/task history.

python3 scripts/test-desktop-probe-live.py --work-url http://127.0.0.1:18089 \
    --probe /path/to/work-bridge-probe --admin test-admin

The password is prompted after the health check, or read from
PINABLE_E2E_ADMIN_PASSWORD. With --verify-mysql, the test also uses Go and
AGENTFLOW_MYSQL_DSN to check that this exact receipt is in the expected Work
database after device cleanup. No Docker services or databases are provisioned.
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


# Only return known CLI diagnostics. Unexpected stderr can include secrets.
def cli_diagnostic(stderr):
    for reason in ("无法定位本机配对锁目录", "无法准备本机配对锁目录", "无法锁定本机配对", "另一个进程正在使用本机配对",
                   "系统凭据库不可用", "系统凭据库或设备锁不可用", "无法读取系统凭据库", "无法写入系统凭据库",
                   "无法清理系统凭据库", "系统凭据保存失败", "本机已经配对", "本机已绑定其他 Work 地址",
                   "本机配对记录无效", "上次配对未完成", "没有已保存的设备", "context deadline exceeded",
                   "Work 连接失败或超时", "已拒绝不符合只读探针协议的任务"):
        if reason in stderr:
            return reason
    status = re.search(r"\bHTTP [1-5][0-9]{2}\b", stderr)
    return status.group() if status else "unrecognized CLI diagnostic"


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
    # The Desktop process needs OS paths, TLS settings and the OS credential
    # service in persistent mode. It never needs the administrator or DB secrets.
    keys = ("PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TEMP", "TMP", "SYSTEMROOT",
            "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SSL_CERT_FILE", "SSL_CERT_DIR")
    if mode != "temporary":
        keys += ("XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "DISPLAY", "WAYLAND_DISPLAY",
                 "XAUTHORITY", "SECURITYSESSIONID")
    env = {key: os.environ[key] for key in keys if key in os.environ}
    if code:
        env["PINABLE_WORK_BIND_CODE"] = code
    return subprocess.Popen([args.probe, "-work-url", args.work_url, "-mode", mode,
                             "-name", name, "-timeout", "35s"], env=env,
                            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, encoding="utf-8")


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
    elif process.returncode != 0:
        raise Failed(f"{label}: Desktop CLI exit {process.returncode}: {cli_diagnostic(err)}")
    return out


def matched_id(value, label):
    check(isinstance(value, str) and re.fullmatch(r"[a-zA-Z0-9_-]{1,128}", value),
          f"{label}: invalid ID")
    return value


def wait_online(api, name, child, mode, expected_device):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        rows = api.mine(name)
        if len(rows) == 1 and rows[0]["status"] == "online":
            device = matched_id(rows[0]["id"], mode)
            check(expected_device is None or device == expected_device, "resumed a different device")
            return device
        if child.poll() is not None:
            finish(child, mode)
            raise Failed(f"{mode} exited before the device became online")
        time.sleep(0.25)
    raise Failed(f"{mode}: device did not become online")


def test(args):
    if args.verify_mysql:
        check(os.environ.get("AGENTFLOW_MYSQL_DSN", "").strip(),
              "--verify-mysql requires AGENTFLOW_MYSQL_DSN for the target Work database")
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
    device = None
    try:
        if args.temporary:
            mode = "temporary"
            print("SKIP persistent pairing, restart and revoked-credential reconnect (--temporary)", flush=True)
            child = cli(args, mode, name, code)
        else:
            mode = "resume"
            out = finish(cli(args, "pair", name, code), "pair")
            paired = True
            rows = api.mine(name)
            check(len(rows) == 1 and rows[0]["status"] == "offline", "pair must leave one offline device")
            device = matched_id(rows[0]["id"], "pair")
            check(f'runnerId = "{device}"' in out, "Desktop and Work device IDs differ")
            print(f"PASS persistent pair and disconnect: {device}", flush=True)
            child = cli(args, mode, name)

        device = wait_online(api, name, child, mode, device)
        print(f"PASS {mode} device online: {device}", flush=True)
        target = {"runnerId": device, "provider": "pinable-desktop", "action": "desktop_probe_v1"}
        job = api.call("POST", "runners/tasks", target, expected=201)
        task = matched_id(job.get("id"), "enqueue")
        check(all(job.get(k) == v for k, v in target.items()) and job.get("status") == "queued",
              "unexpected enqueue receipt")
        print(f"PASS Work queued desktop_probe_v1 for {device}: {task}", flush=True)
        out = finish(child, mode)
        child = None
        check(f'runnerId = "{device}"' in out and f'任务 "{task}" 已探测成功' in out,
              "Desktop did not report the expected device and completed task")
        check(api.call("GET", f"runners/tasks/{task}/status") == {"id": task, "status": "success"},
              "Work and Desktop terminal receipts disagree")
        print(f"PASS {mode} Work / Desktop success runnerId={device} taskId={task}", flush=True)
        rows = api.mine(name)
        check(len(rows) == 1 and rows[0]["id"] == device and rows[0]["status"] == "offline",
              "CLI exit must leave the same device offline")
        print(f"PASS device offline: {device}", flush=True)

        check(api.call("DELETE", "runners/" + device).get("ok") is True,
              "Work did not acknowledge device removal")
        if args.temporary:
            check(not api.mine(name), "temporary device still exists after removal")
            revoked = True
            print(f"PASS temporary device removed: {device}", flush=True)
        else:
            out = finish(cli(args, "resume", name), "revoked resume", reject=True)
            check(f'runnerId = "{device}"' in out and not api.mine(name),
                  "revoked device was replaced or registered again")
            revoked = True
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
            if args.temporary:
                recovery = "check removal of this temporary device in Work"
            elif revoked:
                recovery = "remote removal confirmed; retry the CLI with -mode forget"
            elif paired:
                recovery = "remote removal unconfirmed; local credential retained. Retry the CLI with -mode unpair"
            else:
                recovery = "check this test device in Work; no local credential has been cleared"
            message = f"cleanup {name}: {exc}. {recovery} using the original test account and Work URL."
            if not in_failure:
                raise Failed(message) from exc
            print(message, file=sys.stderr)

    if args.verify_mysql:
        env = os.environ.copy()
        env.pop("PINABLE_E2E_ADMIN_PASSWORD", None)
        env.pop("PINABLE_WORK_BIND_CODE", None)
        result = subprocess.run(
            ["go", "run", "-mod=readonly", "./scripts/testdata/desktop-probe/mysql-receipt", task, device],
            cwd=Path(__file__).resolve().parents[1], env=env, stdin=subprocess.DEVNULL,
        )
        check(result.returncode == 0, "MySQL receipt was not verified")
        print(f"PASS MySQL receipt after device removal runnerId={device} taskId={task}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--work-url", required=True)
    parser.add_argument("--probe", required=True, type=lambda s: Path(s).resolve())
    parser.add_argument("--admin", required=True)
    parser.add_argument("--temporary", action="store_true",
                        help="test one in-memory pairing without opening the system credential store")
    parser.add_argument("--verify-mysql", action="store_true",
                        help="read and verify the resulting task directly using AGENTFLOW_MYSQL_DSN")
    args = parser.parse_args()
    try:
        test(args)
    except (Failed, OSError, KeyError, EOFError, ValueError, TypeError) as exc:
        print(f"FAIL {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
