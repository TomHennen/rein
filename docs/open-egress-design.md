# Open egress: rein as the sandbox's egress proxy

Successor to #163 ("allow-all not expressible on srt 0.0.63"). Issue #185.
Design of record, revised 2026-09-06 after the security review (its seven
must-fix items are folded in and marked "[review N]"). Implemented in the
same PR: `internal/proxy/{egress,tcp,peercred_linux}.go`, `internal/srt`
external-proxy shape + `probe_net.go`, `cmd/rein/{egress,expose}.go`; journeys
`open_egress` (new) and `egress_preset` (regenerated).

## Problem

The agent cannot reach arbitrary websites (research, docs, `WebFetch`, curl).
Egress is srt's job today: `network.allowedDomains` is enforced by srt's own
in-process proxy, and its schema rejects a bare `*` (and `*.com`), so rein cannot
express "allow everything". srt's only allow-all path (`allowedDomains`
undefined) also disables the MITM hook rein needs for GitHub credential
injection. #163 stopped there.

## The route: srt's external proxy

srt (pinned 0.0.63; still in 0.0.75) has `network.httpProxyPort` and
`network.socksProxyPort`: "an external proxy to use instead of starting a local
one; the external proxy must handle domain filtering". With BOTH set to the same
port srt starts no proxy at all (`needLocalProxy` false), mints no proxy auth
token, and the Linux bridge becomes: in-sandbox `socat TCP-LISTEN:3128` (and
`1080`) -> one unix socket -> host `socat` -> `TCP:localhost:<port>`. Everything
else is unchanged: `--unshare-net`, `--unshare-pid`, the seccomp AF_UNIX block,
the filesystem denies. `allowedDomains` must still be a defined array
(`hasNetworkConfig`) so the bridge is wired; rein passes `[]` plus
`deniedDomains: ["*"]`, and the content is inert.

The only thing that moves is WHO decides whether a CONNECT may proceed: rein,
instead of srt. rein already TLS-terminates and injects for the GitHub hosts and
answers its own virtual hosts; it gains a forward-proxy leg for every other host
and owns egress policy end to end. Allow-all becomes a rein policy flag.

## Design

### Listener and client authentication [review 1, 2, 5]

- rein binds `127.0.0.1:0` (the literal address, so socat's `TCP:localhost`
  matches) and passes the port as both `httpProxyPort` and `socksProxyPort`.
  The per-run unix socket stays for tests and the direct-TLS shape; srt never
  uses it in this mode.
- **Proxy secret.** srt sets no proxy auth for an external proxy, and its
  bridge socket in `$TMPDIR` is created with the process umask (group- or
  world-connectable), so a TCP peer-uid check alone passes by construction for
  anyone who can reach that socket. rein therefore mints a per-run secret and
  requires `Proxy-Authorization: Basic base64("srt:" + secret)` on every
  CONNECT that arrives on the TCP listener; anything else gets 407 and the
  connection closes. The secret reaches the agent the way srt's own token did:
  `rein sandbox-exec` (now the launch wrapper for EVERY sandboxed run, not only
  with `expose_ports`) rewrites `HTTP_PROXY`/`HTTPS_PROXY`/`http_proxy`/
  `https_proxy`/`ALL_PROXY`/`all_proxy` to `http://srt:<secret>@localhost:3128`
  and sets `GIT_CONFIG_PARAMETERS='http.proxyAuthMethod=basic'`. The secret is
  delivered to the wrapper as `REIN_PROXY_AUTH` in the sandbox env (rein
  controls that env; the agent is the intended client, exactly as with srt's
  token). `rein declare` and `rein expose` already honour the URL userinfo. The
  launch self-test probe reads `REIN_PROXY_AUTH` directly.
- **Peer-uid check (defense in depth).** On accept, before reading a byte, rein
  matches the connection's full 4-tuple against `/proc/net/tcp` AND
  `/proc/net/tcp6` (hex little-endian IPv4; IPv4-mapped IPv6 `::ffff:7f00:1`),
  requiring `local == peer`, `rem == listener`, state `01` (ESTABLISHED);
  exactly one row must match and its `uid` must be rein's own; zero or several
  rows, or a foreign uid, refuse. TIME_WAIT rows (uid 0) never match state 01.
- **SOCKS.** The first byte of a TCP connection is sniffed: `0x04`/`0x05` is a
  SOCKS greeting and is answered `05 FF` + close, audited
  `refused-egress-socks`. srt advertises `ALL_PROXY` as the HTTP URL, so nothing
  defaults to SOCKS; this just makes srt's leftover mux impossible (there is
  none) and the port single-protocol.
- **CONNECT only on TCP.** The direct-TLS-without-CONNECT shape is accepted only
  on the unix socket.

### Request handling: two keys [review 6, 8, 12]

The CONNECT target `host:port` is the POLICY key and selects the class. For
terminated classes the TLS SNI is the IDENTITY key, as today; a CONNECT host
whose class differs from the SNI's class is refused (never falls through to a
raw tunnel).

Port check comes first for every class:

| CONNECT target | port | treatment |
|---|---|---|
| GitHub inject hosts (`github.com`, `api.github.com`, `uploads.github.com`) | 443 only | unchanged: TLS-terminate with the rein CA, inject, relay (the existing `handleConn` path) |
| rein virtual hosts (`declare.`, `expose.`, `probe.rein.internal`) | 443 only | unchanged: answered locally |
| GitHub CDN hosts | 443 only | always-allowed RAW tunnel (as srt dials them today: the agent sees GitHub's real certificate; `classPassthrough` stays for the unix-socket shape only) |
| any other host | policy gate | RAW tunnel: dial and splice; no TLS termination, no token, rein cannot see inside |

There is NO plain-HTTP absolute-URI relay: `GET http://...` through the proxy is
refused (`refused-egress-plaintext`). Nothing in the dev preset needs port 80.

### Policy gate [review 3, 4]

Evaluated in this order on the CONNECT `host:port`:

1. **Host syntax.** srt-equivalent validation: length <= 255, DNS charset, no
   `%`/control characters; a bracketed IPv6 literal parses via
   `net.SplitHostPort`; IP-literal shorthand (`127.1`, `0x7f000001`,
   `2130706433`) is canonicalised by resolving.
2. **Port.** 443 only, unless an operator `allow_domains` entry names the port
   (`host:port`, srt's own syntax). Open mode does not widen ports.
   `host:<port>` for an inject/CDN/virtual host is rejected at config
   validation.
3. **Never-route, unconditional for every name** — presets, defaults,
   `allow_domains`, `REIN_ALLOW_DOMAINS`, open mode alike. `host` is resolved
   once; if ANY address is in the set below the CONNECT is refused; the dial
   uses the checked addresses and a `net.Dialer.Control` hook re-checks the
   concrete `ip:port` of every attempt (Happy Eyeballs fallbacks included), on
   every upstream dial rein makes, inject transport included. The set:
   loopback (`127/8`, `::1`), `0.0.0.0/8`, `::/128`, RFC1918, ULA `fc00::/7`,
   CGNAT `100.64/10` (Tailscale, Alibaba IMDS), link-local `169.254/16` (AWS/GCP
   metadata) and `fe80::/10`, site-local `fec0::/10`, multicast, `240/4`,
   broadcast, IPv4-mapped/NAT64 `64:ff9b::/96` and `64:ff9b:1::/48`, 6to4
   `2002::/16` and Teredo `2001::/32` (embedded IPv4 extracted and checked),
   **every address currently assigned to a host interface**
   (`net.InterfaceAddrs()`), rein's own listener, and every `expose_ports`
   listener. `localhost` and `*.localhost` names refuse without resolving.
   - **Internal hosts** are a separate opt-in: `allow_internal_hosts:
     ["build.corp:443"]` (session file only, never env), exempt from the
     private/ULA/CGNAT ranges but never from loopback, the host's own
     addresses, link-local, or metadata. Named in the banner when non-empty.
4. **Mode.**
   - `restricted` (default; today's behavior): the union of the built-in
     defaults, the egress preset, `REIN_ALLOW_DOMAINS`, and `allow_domains`,
     matched exactly as srt does (`host` or strict `*.suffix`). Unmatched ->
     refuse.
   - `open` (`open_egress: true` in the session file, or `rein session
     open-egress`; there is deliberately no env switch, a shell-rc-persistent
     machine-wide open mode is too quiet): any host passing steps 1-3.
5. **Refusal** is a local 403 on the CONNECT with a machine-readable body naming
   the remedy for the HUMAN, on the host: `rein session allow-domain <host>`
   (the session file is deny-read in-sandbox; the agent cannot run it).

Every decision is audited with `host:port`: `allowed-egress-list`,
`allowed-egress-open`, `allowed-egress-cdn`, `refused-egress-<reason>`. rein now
records every host the agent contacts; it does not see inside raw tunnels.

### Tunnel bounds [review 9]

Dial timeout 15 s; at most 256 concurrent raw tunnels per run (429 beyond);
tunnel idle timeout 10 min (either direction resets it); `TCP_NODELAY`;
half-close via the existing `splice`. rein NEVER uses a parent proxy for the
forward leg: the host's `HTTPS_PROXY` is not consulted (it would move the
never-route check to a third party).

### srt config

`httpProxyPort` and `socksProxyPort` both = rein's port; no `mitmProxy`;
`allowedDomains: []`; `deniedDomains: ["*"]`; `strictAllowlist: true`.
`Config.Validate()` accepts exactly two shapes — the mitm shape (unix socket,
non-empty allowlist) and the external-proxy shape (both ports, no mitm, empty
allowlist) — and rejects a mix.

### Launch self-test [review 7]

The existing deny-read + seccomp + tty probe stays. Added, inside the same
in-sandbox probe:

1. CONNECT `probe.rein.internal:443` through the in-sandbox proxy with
   `REIN_PROXY_AUTH`; rein answers 200 with `X-Rein-Probe: <nonce>` (nonce passed
   in the probe's argv). Anything else — a 403 from a leftover srt proxy, a
   stale rein on a reused port without the nonce — fails the launch closed.
2. A raw `connect()` to a public `ip:443` must fail (unshare-net holds).
3. A raw `connect()` to `127.0.0.1:<rein port>` from inside must fail (the
   arbitrary-loopback-port control from the nono work).

Policy behavior (restricted refuses `example.com`; open allows it) is unit and
journey territory, not a launch gate.

### Human and agent surfaces [review 10, 11]

- Banner, restricted: unchanged. Open: a fixed loud block, not suppressible:
  the agent can send anything it can read (your checkout, its own transcript,
  its Anthropic credential which is necessarily in its env) to any host on the
  internet, and everything it reads from the open web is untrusted input; GitHub
  credential hiding and write brokering are unchanged; every contacted host is
  in the audit log; the session file is the source of the setting.
- Contract NETWORK section: restricted unchanged; open says egress is open to
  the web on 443, loopback/private/link-local targets and other ports are still
  refused, and `allow_internal_hosts`/`allow_domains` are host-side commands for
  the human.
- The `expose_ports` page runs in the human's unsandboxed browser; rein's
  never-route refusal does not apply to it (README already says so).

### Tests

- Unit: policy table (ports; every never-route form incl. `localhost`,
  `foo.localhost`, `127.1`, `0x7f000001`, `2130706433`, `[::ffff:127.0.0.1]`,
  `[::ffff:7f00:1]`, `[64:ff9b::7f00:1]`, `[fe80::1%25eth0]`, `0.0.0.0`,
  `100.64.0.1`, an interface address, rein's own port, a name resolving to one
  public and one private address; list vs open; internal-host exemption
  boundaries), peer-uid match incl. a v6-mapped client, proxy-auth 407, SOCKS
  reject, CONNECT-only on TCP, port/class ordering, plaintext refusal, config
  shapes, dial Control pin.
- Journeys: `egress_preset` regenerated (refusals now come from rein), new
  `open_egress` (open reaches a public host; `localhost`, a private address and
  the host's own address are refused in open mode; restricted still refuses;
  the audit records the host).
- `run-journeys.sh --sandbox`: the probe's three checks.

### Residuals, stated

- Domain fronting is unchanged: a CONNECT to an allowed host may carry another
  SNI; the TCP peer is still the allowed host's checked address.
- srt's bridge socket permissions follow the process umask; the proxy secret is
  what protects the port, the uid check is a second wall. rein does not change
  the umask (it would leak into every file the agent creates).
- Same-uid host processes remain #7.
- Open mode IS a data-exfiltration surface by definition; see the banner text.

## Size

~1,000-1,300 lines of production Go plus tests and two journeys. Three
checkpoints: (1) `internal/proxy`: policy gate + never-route + dial pin, TCP
listener with proxy auth, peer-uid check, SOCKS reject, raw tunnel with bounds,
`probe.rein.internal`; (2) srt external-proxy config shape, `sandbox-exec`
env rewrite, self-test probe, launch wiring — restricted mode behavior
identical; (3) `open_egress` / `allow_internal_hosts`, `rein session
open-egress`, banner/contract, journeys, README.
