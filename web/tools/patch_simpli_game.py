#!/usr/bin/env python3
"""Instruction patches applied to the staged Assets/StaticR.rel before translation (called by web/build.sh).

Addresses are PAL RMCP01 guest addresses; REL file offset = addr - 0x805102E0 (verified against the
expected original word, so a wrong/changed REL fails loudly). Idempotent. Also updates the pinned
StaticR.rel sha256 in projects/mkwii/recomp.yml, since the translator checks it.
Usage: patch_simpli_game.py <Assets dir> <recomp.yml>
"""
import hashlib, re, sys

BASE = 0x805102E0
PATCHES = [  # (addr, original word, new word, why)
    (0x8053F23C, 0x38A00708, 0x38A0FFFF, 'online VS idle limit A (1800 frames) -> never'),
    (0x8053F240, 0x38C00E10, 0x38C0FFFF, 'online VS idle limit B (3600 frames) -> never'),
    (0x805394F8, 0x38A01C20, 0x38A0FFFF, 'battle idle limit A -> never'),
    (0x805394FC, 0x38C00E10, 0x38C0FFFF, 'battle idle limit B -> never'),
    (0x8053EF78, 0x28000E10, 0x2800FFFF, 'race AFK: kart stationary >3600 frames -> never (cmplwi r0,0xffff)'),
    (0x80647958, 0x3860008B, 0x3860008C, 'WFCConnect::GetNextPage: skip WFCMainMenu (0x8b), go to mode select (0x8c)'),
    (0x8054FB2C, 0x9421FFD0, 0x38600001, 'unlock all: IsVehicleTypeUnlocked -> li r3,1'),
    (0x8054FB30, 0x7C0802A6, 0x4E800020, 'unlock all: IsVehicleTypeUnlocked -> blr'),
    (0x8054FC44, 0x9421FFA0, 0x38600001, 'unlock all: IsCupUnlocked -> li r3,1'),
    (0x8054FC48, 0x7C0802A6, 0x4E800020, 'unlock all: IsCupUnlocked -> blr'),
    (0x8054FED8, 0x9421FFB0, 0x38600001, 'unlock all: IsCharacterUnlocked -> li r3,1'),
    (0x8054FEDC, 0x7C0802A6, 0x4E800020, 'unlock all: IsCharacterUnlocked -> blr'),
    (0x805500CC, 0x9421FFD0, 0x38600001, 'unlock all: IsKartUnlocked -> li r3,1'),
    (0x805500D0, 0x7C0802A6, 0x4E800020, 'unlock all: IsKartUnlocked -> blr'),
    (0x80550468, 0x9421FF70, 0x38600001, 'unlock all: isFastStaffGhostUnlocked -> li r3,1'),
    (0x8055046C, 0x7C0802A6, 0x4E800020, 'unlock all: isFastStaffGhostUnlocked -> blr'),
    (0x8081CFB4, 0x2C040001, 0x38600001, 'unlock all: IsKartUnlocked(menu) -> li r3,1'),
    (0x8081CFB8, 0x4182002C, 0x4E800020, 'unlock all: IsKartUnlocked(menu) -> blr'),
]

def main(assets, yml):
    path = f'{assets}/StaticR.rel'
    d = bytearray(open(path, 'rb').read())
    for addr, old, new, why in PATCHES:
        o = addr - BASE
        cur = int.from_bytes(d[o:o+4], 'big')
        if cur == new: continue
        assert cur == old, f'{addr:#x}: expected {old:#x}, found {cur:#x} ({why})'
        d[o:o+4] = new.to_bytes(4, 'big')
    open(path, 'wb').write(d)
    sha = hashlib.sha256(d).hexdigest()
    y = open(yml).read()
    i = y.index('path: Assets/StaticR.rel')
    j = y.index('sha256: ', i)
    y = y[:j] + 'sha256: ' + sha + y[j+8+64:]
    open(yml, 'w').write(y)
    print('StaticR.rel patched, sha256', sha)

if __name__ == '__main__':
    main(*sys.argv[1:3])
