# Cache Verification

fireactions may share its host with unrelated services — the runner cache is designed to be a
polite tenant: it touches only its own two guarded ports (the cache's `bind_address`, and the
firewall guard that scopes the existing gRPC/metrics ports away from the CNI bridge), never a
broad drop of guest traffic. This checklist is run on the real host after a deploy, with a booted
runner VM available. It is not part of the automated test suite — two of the five checks below
(real-VM reachability, real-host firewall behavior) cannot be exercised from a CI runner or a
developer's macOS machine, which is why they are a documented manual procedure instead of a Go
test.

Each section below maps one-to-one onto a Phase 1 success criterion from `.planning/ROADMAP.md`.

## 1. Disabled cache is invisible

With no `cache:` block in the server YAML (or `enabled: false`), restart fireactions and confirm
no new listener and no new firewall chain exist.

```bash
ss -ltnp | grep fireactions
```

Expected: only the gRPC and metrics ports are listed. No cache port.

```bash
iptables -L FIREACTIONS-CACHE-GUARD -n
```

Expected: `iptables: No chain/target/match by that name.` — the guard chain does not exist at all
when the cache is disabled.

## 2. Reachable from a VM, control plane is not

With the cache enabled and `bind_address` set to the bridge gateway (e.g. `192.168.128.1:5050`),
first confirm on the host that the bridge name matches what fireactions guards:

```bash
ip link show fireactions-br0
```

Expected: the interface exists (state UP once at least one VM has booted).

Then, from **inside a booted runner VM**:

```bash
curl -sS --connect-timeout 2 http://192.168.128.1:5050/healthz
```

Expected: `ok`.

```bash
curl -sS --connect-timeout 2 http://192.168.128.1:8080/
curl -sS --connect-timeout 2 http://192.168.128.1:8081/
```

Expected: both time out or are refused — the unauthenticated gRPC and metrics ports are not
reachable from the guest.

**The in-VM `curl` above is the authoritative check, not `iptables -L` output alone.** A rule
that exists but was installed into the wrong chain (e.g. `FORWARD` instead of `INPUT`) looks
correct on inspection but silently does nothing — only a real connection attempt from inside the
bridge proves the rule is in the chain that actually processes this traffic.

Host-side confirmations, useful for diagnosing a failure in the check above:

```bash
iptables -L FIREACTIONS-CACHE-GUARD -n -v
```

Expected: two DROP rules, both scoped `-i fireactions-br0`, one per guarded port.

```bash
iptables -L INPUT -n --line-numbers | head -5
```

Expected: the jump to `FIREACTIONS-CACHE-GUARD` is at position 1.

## 3. Contents survive a restart

From inside the VM (or via SSH to the host), place a file directly under the cache's data
directory and confirm it round-trips through the cache listener, then again after a restart:

```bash
echo "hello" > /var/lib/fireactions/cache/manual/hello.txt
```

From inside the VM:

```bash
curl http://192.168.128.1:5050/cache/manual/hello.txt
```

Expected: `hello`.

Restart the fireactions service on the host, then repeat the same `curl` from inside the VM.

Expected: identical content — disk is the only source of truth, so a restart does not lose it.

## 4. Usage stays under the cap

Temporarily set `max_size: 10MB` and restart, then fill `data_dir` past the cap:

```bash
for i in $(seq 1 20); do
  dd if=/dev/urandom of=/var/lib/fireactions/cache/manual/blob-$i bs=1M count=1 2>/dev/null
done
```

Wait one sweep interval (5 minutes by default), then confirm usage dropped back down and the
metrics reflect it:

```bash
du -sb /var/lib/fireactions/cache
```

Expected: at or under roughly 9 MB (90 percent of the 10 MB cap).

```bash
curl -s localhost:8081/metrics | grep fireactions_cache_
```

Expected: `fireactions_cache_disk_bytes_used` matches the `du` figure above, and
`fireactions_cache_evictions_total` has increased since before the fill.

Momentary overshoot between sweeps is expected (D-09) and is not a failure — this check is about
the cap being restored, not about it never being crossed.

## 5. No credentials by construction

Confirm the cache's config schema and store code path carry no field or mechanism for
configuring or forwarding upstream credentials:

```bash
cat docs/reference/configuration.md
```

Expected: the `cache:` block documents exactly four fields (`enabled`, `bind_address`,
`max_size`, `data_dir`) — no `username`, `password`, `token`, or `api_key`.

```bash
grep -riE 'password|token|secret|api_key' server/cache/
```

Expected: no matches, or matches only inside comments explicitly forbidding such fields (e.g. the
CORE-07 doc comment on `cache.Config`).

This is a design invariant, not a one-time check — re-run it in Phases 3 and 4, when real
upstream fetchers (registry pull-through, package proxies) are added and the temptation to add a
shared upstream credential field will be highest.

## Operator notes

**After a non-graceful termination** (`kill -9`, an OOM kill, or a supervisor force-stopping the
process) the guard chain outlives the process — `SIGKILL` bypasses the graceful shutdown path
entirely, so the chain and its jump rule are never removed. Check with:

```bash
iptables -L FIREACTIONS-CACHE-GUARD -n
```

A subsequent clean start with the cache enabled reuses the existing chain idempotently rather
than duplicating rules. If the cache is being disabled permanently instead, remove the stale
chain manually:

```bash
iptables -D INPUT -j FIREACTIONS-CACHE-GUARD
iptables -F FIREACTIONS-CACHE-GUARD
iptables -X FIREACTIONS-CACHE-GUARD
```

**If the CNI bridge is not named `fireactions-br0`** on this host, the guard rules still install
successfully — `iptables` accepts a rule referencing an interface name that doesn't exist yet or
doesn't match anything — but they match no traffic. Step 2's in-VM `curl` against `:8080` is what
catches this: a timeout means the guard is working as intended, while a connection or an HTTP
response means the interface name is wrong and must be reported as a bug, since `iptables -L`
output alone would still look correct.
