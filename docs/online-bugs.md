# Online mode: two-tab test log

Test rig: two isolated Chrome profiles (Alice :9222, Bob :9223) on one 8-core Mac, driven over CDP,
gateway + wfc-server + Postgres local. Status values: FIXED (verified), OPEN, WONTFIX (with reason).

| # | Bug | Evidence | Status |
|---|-----|----------|--------|
| 1 | A tab that goes `hidden` mid-race freezes the whole guest, peers time it out and it gets "You were disconnected from the other players" | console `[vi] page hidden: pausing the game` for ~5 s in both tabs, gateway shows both clients still connected, wfc-server QR2 session shutdown | FIXED (build 7; verified with ~54 s hide, both tabs stayed in room) |

| 2 | Hidden tab: page timers clamp to ~1/s, guest starves, peers time it out | `setTimeout` throttling; net frames stalled in hidden tab | FIXED (Worker-backed `MkwHost.sleep` in `library_mkw_host.js`; hidden tab held ~60 fps / ~62 net frames/s) |
| 3 | False log "OSSleepThread ... unsatisfiable for 561s": wedge timer never reset | console in long race | FIXED (`os_sleep.cpp` `lastSeen` stale reset, build 8/9; not re-checked in console) |
| 4 | Stale gateway session after navigating away (bfcache keeps WS open) -> next join refused (409 / "invite rejected") | `/status` listed old Alice session; relaunch refused | FIXED (`main.js`: `pagehide` closes WS, `pageshow` persisted reloads). Verified: navigating Alice's tab dropped her session within 4 s |
| 5 | Idle (no input) player dropped from race after ~40-45 s | disconnect message only for idle player | FIXED (build 10: `patch_simpli_game.py` raises the idle limits in StaticR.rel to 0xFFFF frames). Second AFK check (race code 0x8053EF78, kart stationary >3600 frames) patched in build 11. Verified: both tabs idle and stationary, race clock 2:11, nobody dropped |
| 6 | Alice FPS dips 44-52 vs Bob 60 (two tabs + helper on one 8-core Mac, host CPU contention) | per-second GPU submits: Alice 113-120, Bob 120 | OPEN: needs solo-tab baseline; likely test-rig contention, not port |
| 7 | aurora `SetScissor without an active render pass` at boot | console | WONTFIX: cosmetic |
| 8 | `SSL_READ ... FAILED -6 (peer closed)` at scene exit | console | WONTFIX: Sake server closes after response |
| 9 | `OSSleepThread could not switch away` one-time retry | console | WONTFIX: resolves on retry |
| 10 | Both Miis show name "Player" in-game | in-game | WONTFIX: cosmetic; identity is per Mii/licence |
| 11 | Gateway closes malformed/oversize frames with 1006 | fuzz run | FIXED (`gw.go` sends 1009 / 1003; unit tests pass, live fuzz not re-run) |

| 12 | Stale browser cache of patched `.szs` -> "ARCInitHandle: bad archive format" | file shorter than manifest size | FIXED (gateway `no-cache` on `/disc/`) |
| 13 | BMG rename truncated text ("91010ario Kart Channel") | BMG escapes contain zero units | FIXED (escape-aware scan in `patch_simpli_ui.py`) |
| 14 | Nintendo branding and WFCMainMenu funnel | in-game | FIXED: all strings say SimpliConnection; `GetNextPage` patched to skip 0x8b -> 0x8c. Verified in 2 tabs. First-time friend-code/terms pages (0x85/0x86) and VS/Battle select remain |
| 15 | Peer exits mid-race | closed Bob's tab mid-race | FIXED/verified: Alice's race ended with "Results have been determined", then Next Race/Quit; Next Race with nobody left shows "disconnected" popup, A returns to the Worldwide lobby. No crash |

Untested: 13th real client (new room), mid-race reload+rejoin, same-name clients, admin/revoke, offline Single Player regression.
