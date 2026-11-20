# Fork patches in doubletake-samsung

This repo is a fork of [omarroth/doubletake](https://github.com/omarroth/doubletake) at commit
`5e2f1cb` (2026-10-29) and carries seven local patches. Patches 1–4 and 6 are probe hooks of the `DOUBLETAKE_*`
env-var style; none changes default behavior when unset. Patches 5 and 7 change default behavior
(the audio capture source, and receiver-volume handling). Upstreaming them would be welcome — see upstream issue #30.

## 1. `DOUBLETAKE_TIMING=NTP|PTP` — internal/airplay/mirror.go

`setupMirrorSession` reads `policy.timing` from the receiver-compatibility table: PTP is chosen
when the session is encrypted, the receiver advertises feature 41, and `srcvers >= 354.54.6`.
The Samsung 58CU7000 (`srcvers=377.30.02`, Tizen) satisfies all three, but its SETUP response
omits `timingPeerInfo.ClockID`, so `mediaClock.configureFromSetup` fails hard
(`configure PTP media clock: SETUP response omitted timingPeerInfo.ClockID`) and no frame is
ever sent. `configureFromLocalClock` also requires the receiver ClockID, so there is no fallback.

The env override forces the timing protocol after the policy decision, before the media clock
allocation, making the whole downstream setup consistent (UDP timing probes, audio/video plists).

Observed with NTP: control/audio/video SETUP all 200, RECORD 200, NTP timing pings answered on
UDP 40924, IDR + delta frames streamed on TCP data port, `mirror session ready`.

Same root cause as upstream issue #30 Finding 3 (Roku TVs); the reporter's suggested upstream
fix — keep requesting PTP and fall back to NTP when SETUP omits ClockID — would subsume this
hook.

## 2. `DOUBLETAKE_PAIR_TYPE=<int>` — internal/airplay/pairing.go

Overrides `effectivePairType()` (the `X-Apple-HKP` header value; default 5 = screen-capture
pairing, 3 = SystemPairing, legacy = 0). Used for differential testing against receivers that
reject SRP under a specific pairing type (upstream #30 Finding 1). The CU7000 pairs fine with
the stock decision path (type 3, selected by `PrefersLegacyPairing()`), so this hook is unused
for this TV and kept for future receivers.

## 3. Debug instrumentation (same commit, harmless)

- `exchangePairSetupM1`: under `-debug`, dumps the M2 TLV inventory (`0x02 salt`, `0x03
  server public key`, lengths). Used to rule out TLV mis-parsing (CU7000 M2 is a plain 409-byte
  HAP M2: salt 16 B + pubkey 384 B + one 1-byte flag TLV).
- `completeSRPExchange`: under `-debug`, logs salt/pubkey lengths, PIN length, effective pair
  type. Used to pin down the PIN-expiry failure mode: PIN entered ~45 s after `pair-pin-start`
  → M4 error 2 (authentication); ~17 s → success. Samsung codes are short-lived; enter fast.

## 4. `DOUBLETAKE_RESTORE_SCOPE=<name>` — internal/airplay/credentials.go (+ daemon/cmd read sites)

`SavedCredentials` stored exactly one Wayland screencast restore token per device
(`restore_token`). samsung-cast's extend mode streams a virtual Hyprland headless output
instead of the laptop panel, so a single token makes each mode silently restore the other
mode's monitor — extend would clone the laptop screen instead of extending. With the env var
set, `CredentialStore.SaveRestoreToken` writes into a new per-device `restore_tokens` map
keyed by scope, and the three read sites (daemon connect, daemon HEVC fallback, standalone
`prepareCapture`) read via `SavedCredentials.RestoreTokenForScope(RestoreScope())`. Unset
(empty scope) keeps the legacy field and the previous behavior byte-for-byte. samsung-cast
starts the daemon with `DOUBLETAKE_RESTORE_SCOPE=extend` in extend mode and without it for
mirroring, so both capture targets restore silently and independently (one picker each, once
ever). Covered by `credentials_scope_test.go`.

## 5. Default-output audio via `@DEFAULT_MONITOR@` — internal/airplay/audio.go

Upstream resolved `pactl get-default-sink` at capture start and pinned
`pulsesrc device=<sink>.monitor`. When the session's default sink is a handler's
virtual device (here the PipeWire echo-cancel sink), mirrored audio stayed bound
to that handler's monitor even after the user re-routed default output — and a
failed `pactl` lookup silently fell back to a bare `pipewiresrc`, which attaches
to the default *source* (the microphone). The fork instead passes the
sound-server alias `device=@DEFAULT_MONITOR@`: the server itself resolves it at
every stream start, so audio always follows the current default output device,
whichever handler provides it.

This is the one fork patch that changes default behavior. It also drops the
`pulseaudio-utils` (`pactl`) runtime dependency on the audio path. Verified on
this machine: `pulsesrc device=@DEFAULT_MONITOR@` negotiates 44.1 kHz stereo,
and `StartAudioCapture` produces compressed ALAC frames through the full
timestamped pipeline.

## 6. `DOUBLETAKE_MONITOR=<output>` — internal/airplay/capture.go

On the X11 fallback path, `ximagesrc` grabs the full combined X screen and the crop
(`detectMonitor`, formerly `detectPrimaryMonitor`) hardcoded primary-then-first-connected, so on
an extended multi-monitor X11 setup there was no way to send a non-primary output. With the env
var set to an xrandr output name (e.g. `DP-1`), a first pass selects the connected output whose
first whitespace field matches; a name that matches nothing connected (or only outputs without
parseable geometry, e.g. disconnected lines) logs a debug warning and falls back to the
primary/first logic — the stream never hard-fails. Empty/unset is upstream behavior
byte-for-byte. Strictly the X11 branch: the read site is the `detectMonitor` call inside the
`ximagesrc` argument setup, and the Wayland/portal path (restore tokens, `DOUBLETAKE_RESTORE_SCOPE`)
is untouched. Covered by `TestDetectMonitorSelectsRequestedOutput`,
`TestDetectMonitorFallsBackToFirstConnectedWithoutPrimary`, and `TestParseXrandrGeometry` in
`capture_x11_test.go`, which put a fake `xrandr` on `PATH`.

## 7. Never write the receiver's volume at setup — internal/airplay/mirror.go

Upstream sent `SET_PARAMETER volume: 0.000000` twice after RECORD on every
audio-carrying session. In AirPlay `0.000000` is 0 dB — full scale — and
doubletake has no volume UI, so this discarded the user's speaker setting on
every connect. On the Samsung 58CU7000 it did worse: Tizen snapshots the
speaker volume when an AirPlay session starts and restores that snapshot when
the session closes, so the cast ended with the TV jumping to 100% no matter
what the user had set before. (This was previously misattributed to pure
firmware behavior — the HTTP 500 evidence in the note below came from a
mid-session *mute* command, not from the setup-time write, which the TV
applies.)

The setup block is deleted outright, with a comment recording why. The
daemon's explicit `SetAudioMuted` (user-initiated mute/unmute) is unchanged;
`-no-audio` sessions were already skipping the write and still negotiate the
audio stream as before. `TestSetupMirrorWithAudioLeavesReceiverVolumeUntouched`
pins the exact RTSP method sequence so a SET_PARAMETER cannot sneak back into
setup.

## 8. `DOUBLETAKE_RELAY_BUDGET_MS=<ms>` — internal/airplay/capture_broadcast.go

`BroadcastSink` detaches a receiver whose encoded fan-out queue holds more than
`broadcastSinkFrameQueueDuration` (250 ms) of nominal frames. That guard produced
`read timestamped capture: broadcast sink backlog limit exceeded` and ended the cast whenever the
TV's video socket stalled. The budget is too tight for Wi-Fi links with real jitter: the CU7000 on
a 5 GHz DFS channel answers ICMP at ~45 ms average with ~300 ms maxima and 0 % loss, and one spike
necessarily fills the queue past 250 ms.

The hook is read per sink (positive integer milliseconds; unset keeps 250 ms, so stock behavior —
and the four duration tests, which now pin the env to empty — is unchanged). mirrorling passes
`MIRRORLING_RELAY_BUDGET_MS` (default 5000 ms) as `DOUBLETAKE_RELAY_BUDGET_MS`. The byte and chunk
bounds still cap memory independently, so a longer budget cannot grow a queue past 8 MiB; a
receiver that truly stops draining is still detached, just after the budget instead of a quarter
second.

## 9. Video frame write deadline follows the relay budget — internal/airplay/mirror.go

`sendFrame` bounded every frame write with a hard 2 s deadline, so a receiver that stalled for two
seconds failed the session with `send non-IDR: writev tcp …: i/o timeout` even when it resumed a
moment later. The deadline now comes from `videoWriteDeadline()`: the relay budget with a 2 s
floor, i.e. the same knob as patch 8. TCP is reliable — a late frame is decodable when the receiver
resumes, and it drops superseded frames by PTS — so surviving the stall is strictly better than
disconnecting. The queue budget still bounds memory, and a session whose receiver never resumes
still ends (through the sink) once the budget expires.

## 10. Audio survives transient routing errors — internal/airplay/audio.go

One `sendto: network is unreachable` from the audio data socket ended audio for the rest of the
session (`audio streaming error`) while video kept playing. The audio sockets are wildcard-bound
(`[::]:port` from the port-range allocator), and a route flap or an interface re-addressing — a
Wi-Fi roam, an IPv6 router-advertisement change — makes `sendto` fail once even though the link is
back milliseconds later. `writeToReceiver` retries `ENETUNREACH`, `EHOSTUNREACH`, `ENETDOWN` and
`EADDRNOTAVAIL` with exponential backoff (50 ms doubling, six attempts ≈ 1.5 s, covered by the
receiver's jitter buffer) and only gives up on errors that will not heal. Both the plain and the
RFC 2198 redundant paths go through it.

## Not patched (observed, non-fatal)

- Mid-session `SET_PARAMETER volume` (daemon `mute`) → HTTP 500 on this
  firmware — unfixable sender-side; setup-time volume writes were removed
  instead (patch 7). Confirmed on a live 4K HEVC stream
  (daemon `mute` → `set audio muted=true: HTTP 500`).
- H.264 NAL type 12 (filler) units are ignored by the stream packer (expected).
- `[EVENT] channel` quirks seen on other receivers were not observed here.
