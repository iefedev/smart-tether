# Smart Tether design notes

This document explains why the link between the PC and the phone works the way it does. The
code is the reference; this is the reasoning behind it.

## Components

| Part | Where | Role |
| --- | --- | --- |
| Windows service | `cmd/smart-tether-service`, `internal/pc` | Wintun TUN adapter, userspace TCP/IP stack (tun2socks/gVisor), routes, phone discovery, loopback API |
| Tray app | `windows/SmartTether` | WPF UI; talks to the service's API on `127.0.0.1:7780` |
| Link protocol | `internal/link` | Resumable, multiplexed byte stream plus datagram frames |
| Phone relay | `internal/relay`, `mobile` | Go code bound into the Android app with gomobile; opens the real internet sockets |
| Android app | `android` | Compose UI and a foreground service that binds sockets to the cellular network |

## Link protocol (PTL3)

The phone listens and the PC dials, so Windows needs no inbound firewall rule. The phone
listens on loopback (reached through `adb forward tcp:7778 tcp:7777` over USB) and on its address
in the PC's Mobile Hotspot network. It never listens on the cellular interface.

A connection starts with the magic `PTL3`, the pre-shared token, and a resume handshake that
names a session. A session is a **Resumable** byte stream: both ends keep unacknowledged bytes,
so when a transport breaks (cable pulled, Wi-Fi drops) a new connection over any transport
continues the same stream at the right offset. The session survives up to about 30 seconds
without any connection.

On top of the reliable stream, yamux carries one stream per TCP flow and per DNS client. Each
stream starts with a request (kind and destination); TCP streams get a one-byte status back
before data flows.

Only one protocol version is accepted. The PC and the phone app must come from the same release.
Readers skip unknown frame types, so a new frame type can be added without a new magic, but any
change to an existing frame format needs both sides updated together.

## UDP as datagram frames

UDP does not use streams. Each PC datagram is a Resumable **datagram frame**, sent outside the
reliable byte stream: it is never retransmitted, it is dropped while no transport is attached,
and it is dropped if it has waited longer than 300 ms. This keeps UDP's own semantics (loss is
better than lateness) and avoids head-of-line blocking behind TCP data.

On the phone, UDP state lives in a NAT table that belongs to the relay server, not to a link
session. Each PC UDP socket (family plus source port on the tunnel) gets one unconnected
cellular socket:

- **Endpoint-independent mapping:** the same public port for every destination, like a typical
  home router.
- **Address-and-port-dependent filtering:** replies are accepted only from remotes that socket
  has sent to.
- The mapping survives transport switches (USB to Wi-Fi and back) and session replacement, so
  a voice or game session keeps its public port.
- Idle sockets and remotes expire. When the cellular network changes, sockets bound to the old
  network are closed; the next datagram opens a new one (with a new public port).

Per-flow UDP streams were avoided on purpose: tying a flow to a connected socket or to a session
changes the public port on every socket error, network handover or session loss, which breaks
long-lived UDP sessions. If the phone app's process is restarted, its sockets are new and every
UDP flow gets a new public port; no code can keep ports across a process restart.

## Pacing bytes in flight

USB and Wi-Fi paths both have deep buffers (socket buffers, adbd, the adb server, Wi-Fi
aggregation). If the sender fills them, a datagram queued behind a large download waits for
the whole backlog. The pacer (`internal/link/pace.go`) caps the wire bytes in flight so the
backlog waits inside the Resumable, where datagrams can overtake it.

The estimate follows BBR's model of the path:

- **Base delay:** the shortest round trip of the last 60 seconds. Every 10 seconds the cap drops
  to its floor until the queue has drained (like BBR's ProbeRTT), so a link that is kept full
  still sees its empty-queue round trip.
- **Rate:** the highest delivery rate of the last 2 seconds, measured over 50 ms windows. The
  adb path releases and acknowledges megabytes within a millisecond or two, so a per-ack rate
  estimate reads far above the real rate; windowing fixes that.
- **Cap:** `2 x rate x (base + paceTarget)`, with `paceTarget` = 4 ms, so only about
  `base + 2 x paceTarget` of data queues beyond the sender.

Supporting rules:

- A 64 KB reserve lets datagrams in even when TCP has filled the cap, except during a datagram
  flood, when TCP keeps about half the link.
- Acknowledgements are coalesced (64 KB or 1 ms) so they cost little Wi-Fi airtime.

Approaches that do not fit this path:

- **Delay-only control (LEDBAT-like):** adb's acknowledgement jitter of a few milliseconds reads
  as queueing and keeps the cap at its floor.
- **Ack-aggregation terms (BBR's extra_acked):** no throughput gain on Wi-Fi, more latency.

The trade-off: Wi-Fi needs queued data to aggregate frames, so a lower `paceTarget` costs some
Wi-Fi throughput in exchange for lower latency for interactive traffic. Tune `paceTarget` for
that trade-off rather than the estimator, and compare against the phone's own native latency
measured at the same time, because mobile network latency varies a lot from minute to minute.

## Discovery and pairing

- **USB:** the service talks to the adb server's wire protocol on `127.0.0.1:5037` instead of
  running `adb.exe`. An adb server started by a LocalSystem service runs in session 0 and does not
  work there, so the tray app keeps the adb server running in the user's session. The service
  re-checks its `adb forward` every tick, because a restarted adb server forgets its forwards.
- **Wi-Fi:** the PC runs Windows Mobile Hotspot (`tools/mobile-hotspot.ps1`, SSID `SmartTether`,
  `192.168.137.0/24`). The phone joins it as a local-only network (`WifiNetworkSpecifier`, no
  internet capability), so Android never routes the phone's own traffic through the PC. The PC
  finds the phone in its ARP table.
- **Pairing:** the token and hotspot passphrase reach the phone either through a QR code shown by
  the tray app or, over USB, through `am start` of `UsbPairingActivity`. That activity is
  protected by `android.permission.DUMP`, which the adb shell holds and ordinary apps cannot get.

## Windows integration

- Routes `0.0.0.0/1`, `128.0.0.0/1`, `::/1` and `8000::/1` are added while the link is up and
  withdrawn after 10 seconds without a connection, so Windows falls back to its other networks.
- The TUN adapter's DNS server is answered by the service; queries go to the carrier's resolvers
  through the phone.
- Windows' connectivity probes (NCSI) are answered locally, so Windows doesn't show "No internet"
  and the probes don't use mobile data.
- QUIC (UDP/443) is dropped by default: downloads through the tunnel are faster over TCP, and
  browsers fall back to TCP immediately.
