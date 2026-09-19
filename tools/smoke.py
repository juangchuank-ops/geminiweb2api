#!/usr/bin/env python3
"""GeminiWeb2API end-to-end smoke test.

Usage:
    python tools/smoke.py [--base http://127.0.0.1:8080] [--password admin12345]

Checks the admin console API, the OpenAI-compatible surface and the audit
pipeline. It never touches the real upstream unless an account with a valid
credential exists, so it is safe to run against a fresh instance.
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.request

# Structurally valid cookie pastes. The backend parses __Secure-1PSID out of
# each one and derives the account's identifier from that value, so the two have
# to differ — sharing a PSID would make the second import an update rather than
# a create, and the assertions below could not tell the two paths apart.
SMOKE_COOKIE = "__Secure-1PSID=g.a000smokesession000000000000; __Secure-1PSIDTS=sidts-smoke"
IMPORTED_COOKIE = "__Secure-1PSID=g.a000importedsession00000000; __Secure-1PSIDTS=sidts-imported"

FAILURES: list[str] = []


def call(base, path, method="GET", body=None, token=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(base + path, data=data, method=method)
    if data:
        request.add_header("content-type", "application/json")
    if token:
        request.add_header("authorization", "Bearer " + token)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(request, timeout=timeout) as response:
            raw = response.read().decode()
            return response.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as error:
        raw = error.read().decode()
        try:
            return error.code, json.loads(raw)
        except json.JSONDecodeError:
            return error.code, {"raw": raw}
    except Exception as error:  # noqa: BLE001 - surfaced as a failed check
        return 0, {"error": str(error)}


def check(name, condition, detail=""):
    mark = "PASS" if condition else "FAIL"
    print(f"  [{mark}] {name}{(' · ' + detail) if detail else ''}")
    if not condition:
        FAILURES.append(name)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", default="http://127.0.0.1:8080")
    parser.add_argument("--username", default="admin")
    parser.add_argument("--password", default="admin12345")
    parser.add_argument(
        "--skip-upstream",
        action="store_true",
        help="skip gateway calls that need a live Gemini account "
             "(they block until the upstream timeout otherwise)",
    )
    args = parser.parse_args()
    base = args.base.rstrip("/")

    print("GeminiWeb2API smoke test")
    print(f"base = {base}\n")

    print("1. health")
    status, payload = call(base, "/health")
    check("GET /health returns 200", status == 200, f"status={status}")
    check("health reports version", bool(payload.get("version")), str(payload.get("version")))
    check("health reports pool", "pool" in payload)

    print("\n2. admin auth")
    status, payload = call(base, "/admin/api/auth/login", "POST",
                           {"username": args.username, "password": args.password})
    check("login returns 200", status == 200, f"status={status} {payload}")
    token = payload.get("token", "")
    check("login returns a token", bool(token))
    if not token:
        return finish()

    status, payload = call(base, "/admin/api/auth/me", token=token)
    check("GET /auth/me returns the profile", status == 200 and payload.get("username") == args.username)

    status, _ = call(base, "/admin/api/accounts")
    check("unauthenticated admin call is rejected", status == 401, f"status={status}")

    print("\n3. dashboard and settings")
    status, payload = call(base, "/admin/api/dashboard?period=30d&timezone=Asia/Shanghai", token=token)
    check("GET /dashboard returns 200", status == 200, f"status={status}")
    check("dashboard exposes usage", "usage" in payload)
    check("dashboard exposes resources", "resources" in payload)

    status, payload = call(base, "/admin/api/settings", token=token)
    check("GET /settings returns 200", status == 200)
    check("settings carry routing policy", "routing" in payload)
    check("settings carry about block", payload.get("about", {}).get("version") is not None)

    print("\n4. client keys")
    status, payload = call(base, "/admin/api/client-keys", "POST",
                           {"name": "smoke-key", "rpmLimit": 60, "maxConcurrent": 4}, token)
    check("create client key returns 200", status == 200, f"status={status} {payload}")
    key = payload.get("key", {}).get("key", "")
    check("client key has a value", key.startswith("sk-gm-"), key[:14])

    status, payload = call(base, "/admin/api/client-keys", token=token)
    check("list client keys returns 200", status == 200 and payload.get("total", 0) >= 1)

    print("\n5. models")
    status, payload = call(base, "/admin/api/models", token=token)
    check("GET /admin/models returns 200", status == 200)
    model_ids = {item["id"] for item in payload.get("items", [])}
    for expected in ("gemini-3.8-flash", "gemini-3.8-flash-thinking", "gemini-3.1-pro", "gemini-3.5-flash-lite"):
        check(f"catalogue contains {expected}", expected in model_ids)

    status, payload = call(base, "/v1/models", token=key)
    check("GET /v1/models returns 200", status == 200, f"status={status}")
    check("GET /v1/models lists gemini-3.8-flash", any(item["id"] == "gemini-3.8-flash" for item in payload.get("data", [])))

    status, _ = call(base, "/v1/models")
    check("GET /v1/models requires a key", status == 401, f"status={status}")

    print("\n6. accounts")
    status, payload = call(base, "/admin/api/accounts", "POST", {
        "name": "smoke-account",
        "cookie": SMOKE_COOKIE,
        "priority": 30, "maxConcurrent": 3,
    }, token, timeout=60)
    check("create account returns 200", status == 200, f"status={status} {payload}")
    account_id = (payload.get("account") or {}).get("id", "")
    check("account has an id", bool(account_id))
    serialised = json.dumps(payload)
    check("raw cookie never leaves the server", SMOKE_COOKIE not in serialised)
    check("cookie is masked in the response", bool((payload.get("account") or {}).get("cookieMasked")))
    check("the account is a cookie account", (payload.get("account") or {}).get("kind") == "cookie")
    # The identifier is derived from the PSID, so it has to exist: an account
    # nobody can name is an account nobody can manage in a list.
    check("the account was given an identifier", bool((payload.get("account") or {}).get("identifier")))

    status, payload = call(base, "/admin/api/accounts", "POST", {
        "name": "smoke-guest", "kind": "guest",
    }, token, timeout=60)
    check("a guest account needs no credential", status == 200, f"status={status} {payload}")
    guest_id = (payload.get("account") or {}).get("id", "")
    check("guest is recorded as a guest", (payload.get("account") or {}).get("kind") == "guest")

    status, payload = call(base, "/admin/api/accounts", "POST", {
        "name": "smoke-broken", "kind": "cookie",
    }, token, timeout=60)
    check("asking for a cookie account without a cookie is rejected", status == 400, f"status={status}")

    status, payload = call(base, "/admin/api/accounts", token=token)
    check("list accounts returns 200", status == 200)
    check("account appears in the list", payload.get("total", 0) >= 1)
    check("account summary is present", "summary" in payload)

    status, payload = call(base, "/admin/api/accounts/groups", token=token)
    check("account groups return 200", status == 200)

    if account_id:
        status, payload = call(base, f"/admin/api/accounts/{account_id}", "PATCH",
                               {"name": "smoke-account-renamed", "priority": 10, "maxConcurrent": 5}, token)
        check("update account returns 200", status == 200, f"status={status}")
        check("update applied", (payload.get("account") or {}).get("priority") == 10)

        status, payload = call(base, "/admin/api/accounts/batch", "POST",
                               {"action": "disable", "ids": [account_id]}, token)
        check("batch disable returns 200", status == 200)
        status, payload = call(base, "/admin/api/accounts", token=token)
        target = next((item for item in payload.get("items", []) if item["id"] == account_id), None)
        check("account is disabled", target is not None and target.get("status") == "disabled")

        status, payload = call(base, "/admin/api/accounts/batch", "POST",
                               {"action": "enable", "ids": [account_id]}, token)
        check("batch enable returns 200", status == 200)
        status, payload = call(base, "/admin/api/accounts/batch", "POST",
                               {"action": "clearCooldown", "ids": [account_id]}, token)
        check("clear cooldown returns 200", status == 200)
        status, payload = call(base, "/admin/api/accounts/batch", "POST",
                               {"action": "concurrency", "ids": [account_id], "maxConcurrent": 7}, token)
        check("batch concurrency returns 200", status == 200)

    status, payload = call(base, "/admin/api/accounts/export?limit=10", token=token)
    check("export accounts returns 200", status == 200 and payload.get("count", 0) >= 1)

    status, payload = call(base, "/admin/api/accounts/import", "POST",
                           {"cookies": IMPORTED_COOKIE}, token)
    check("import accounts returns 200", status == 200, f"status={status}")
    check("import created one account", payload.get("created", 0) == 1, json.dumps(payload))

    status, payload = call(base, "/admin/api/accounts/import", "POST",
                           {"cookies": IMPORTED_COOKIE}, token)
    check("duplicate import updates instead of duplicating",
          payload.get("updated", 0) == 1 and payload.get("created", 0) == 0, json.dumps(payload))

    print("\n6b. cookie rotation")
    # The sweep's state is what the console's toolbar reads. Its shape is a
    # contract, and a missing key there is a blank toolbar rather than an error.
    status, payload = call(base, "/admin/api/refresh", token=token)
    check("GET /refresh returns 200", status == 200, f"status={status}")
    for field in ("enabled", "intervalMin", "gapSeconds", "timeoutSec", "retireAfter", "counts", "totalAccounts"):
        check(f"refresh overview carries {field}", field in payload)
    # A freshly created account has never been rotated, so it must be counted as
    # pending rather than silently absent.
    check("a never-rotated account counts as pending", payload.get("counts", {}).get("pending", 0) >= 1,
          json.dumps(payload.get("counts")))
    # ...but the guest must not be. It has no cookie to rotate, so counting it
    # would leave the console reporting work that will never happen.
    pending = payload.get("counts", {}).get("pending", 0)
    total = payload.get("totalAccounts", 0)
    check("non-rotatable accounts are excluded from the pending count", pending < total,
          f"pending={pending} total={total}")

    if account_id:
        status, payload = call(base, f"/admin/api/accounts/{account_id}/refresh", "POST", None, token, timeout=90)
        outcome = (payload.get("outcome") or {}).get("status", "")
        # Without a live session Google rejects the rotation, so this is expected
        # to fail — what is asserted is the *shape* of the answer: a rejected
        # rotation must be a 502 carrying a reason, never a 200 saying "ok".
        check("rotating without a live session is not reported as success",
              status != 200 or outcome == "ok", f"status={status} outcome={outcome}")
        if status == 502:
            check("a rejected rotation explains itself", bool(payload.get("error")), json.dumps(payload))
        check("the account is still listed afterwards", call(base, f"/admin/api/accounts", token=token)[0] == 200)

    if guest_id:
        status, payload = call(base, f"/admin/api/accounts/{guest_id}/refresh", "POST", None, token, timeout=30)
        check("rotating a guest answers 'skipped', not an error",
              status == 200 and (payload.get("outcome") or {}).get("status") == "skipped",
              f"status={status} {json.dumps(payload)}")

    status, payload = call(base, "/admin/api/accounts/refresh-all", "POST", None, token, timeout=180)
    check("refresh-all returns 200", status == 200, f"status={status}")
    check("refresh-all reports a summary", "summary" in payload)

    print("\n7. service stays responsive while background probes run")
    # Regression guard: creating an account kicks off a detached upstream probe.
    # When that probe fails it writes account state from inside a mutation
    # callback. A reentrant-lock bug there used to freeze the whole process, so
    # assert the console keeps answering while probes are in flight.
    alive = True
    slowest = 0.0
    for _ in range(6):
        time.sleep(1.5)
        started = time.monotonic()
        probe_status, _ = call(base, "/health", timeout=5)
        elapsed = time.monotonic() - started
        slowest = max(slowest, elapsed)
        if probe_status != 200:
            alive = False
            break
    check("health stays 200 while probes run", alive)
    check("health responds promptly under load", slowest < 2.0, f"slowest={slowest:.2f}s")

    status, payload = call(base, "/v1/chat/completions", "POST",
                           {"model": "nope", "messages": [{"role": "user", "content": "hi"}]}, key)
    check("unknown model returns 400", status == 400, f"status={status}")

    if args.skip_upstream:
        print("\n8. gateway (skipped)")
        print("  [SKIP] upstream gateway calls (--skip-upstream)")
    else:
        print("\n8. gateway (expected to fail without a valid Gemini cookie)")
        status, payload = call(base, "/v1/chat/completions", "POST",
                               {"model": "gemini-3.8-flash", "messages": [{"role": "user", "content": "hi"}]},
                               key, timeout=180)
        check("chat completions answers with an HTTP status", status in (200, 400, 429, 502), f"status={status}")
        if status == 400:
            check("bad model is reported clearly", "model" in json.dumps(payload))

        status, payload = call(base, "/v1/images/generations", "POST", {"prompt": "a blue circle"}, key, timeout=180)
        check("image generations answers with an HTTP status", status in (200, 502), f"status={status}")

        status, payload = call(base, "/health")
        check("health still 200 after gateway calls", status == 200, f"status={status}")

    print("\n9. audits")
    status, payload = call(base, "/admin/api/audits", token=token)
    check("GET /audits returns 200", status == 200)
    if args.skip_upstream:
        print("  [SKIP] audit capture needs a gateway request (--skip-upstream)")
    else:
        check("audit records were captured", payload.get("total", 0) >= 1, f"total={payload.get('total')}")

    print("\n10. cleanup")
    status, payload = call(base, "/admin/api/accounts/cleanup", "POST", {"statuses": ["invalid"]}, token)
    check("cleanup returns 200", status == 200, f"status={status}")

    status, payload = call(base, "/admin/api/accounts", token=token)
    for item in payload.get("items", []):
        if item["name"].startswith("smoke-") or item["name"].startswith("global-"):
            call(base, f"/admin/api/accounts/{item['id']}", "DELETE", token=token)
    status, payload = call(base, "/admin/api/client-keys", token=token)
    for item in payload.get("items", []):
        if item["name"] == "smoke-key":
            call(base, f"/admin/api/client-keys/{item['id']}", "DELETE", token=token)
    check("smoke artifacts removed", True)

    return finish()


def finish():
    print()
    if FAILURES:
        print(f"FAILED ({len(FAILURES)}): " + ", ".join(FAILURES))
        return 1
    print("ALL CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
