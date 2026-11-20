#!/usr/bin/env python3
"""Rename "Nintendo WFC" / "Nintendo Wi-Fi Connection" to "SimpliConnection" in the served disc's UI text.

Rewrites Scene/UI/*.szs (Yaz0 + U8 + BMG) in web/dist/disc and updates disc-manifest.json sizes.
Each patched file is written to a temp name and renamed over the old one, which also breaks any
hard link to the user's extracted disc, so the original data is never modified.
Usage: patch_simpli_ui.py [dist_dir]   (idempotent)
"""
import json, os, struct, sys, glob

NEW = 'SimpliConnection'
OLD = ['Nintendo Wi-Fi Connection', 'Nintendo Wi-Fi\nConnection', 'Nintendo\nWi-Fi Connection', 'Nintendo WFC']

def yaz0_dec(d):
    n = struct.unpack('>I', d[4:8])[0]; out = bytearray(); i = 16
    while len(out) < n:
        code = d[i]; i += 1
        for b in range(8):
            if len(out) >= n: break
            if code & (0x80 >> b): out.append(d[i]); i += 1
            else:
                b1, b2 = d[i], d[i+1]; i += 2
                dist = ((b1 & 15) << 8 | b2) + 1; ln = b1 >> 4
                if ln == 0: ln = d[i] + 18; i += 1
                else: ln += 2
                for _ in range(ln): out.append(out[-dist])
    return bytes(out)

def yaz0_enc(d):
    out = bytearray(b'Yaz0' + struct.pack('>I', len(d)) + bytes(8)); n = len(d); i = 0
    tbl = {}; grp = bytearray(); bits = 0; cnt = 0
    def flush():
        nonlocal grp, bits, cnt
        if cnt:
            out.append(bits << (8 - cnt) & 0xFF); out.extend(grp)
        grp = bytearray(); bits = 0; cnt = 0
    def add(p):
        if p + 3 <= n: tbl.setdefault(d[p:p+3], []).append(p)
    while i < n:
        best = 0; bpos = 0
        if i + 3 <= n:
            for p in reversed(tbl.get(d[i:i+3], [])[-48:]):
                if i - p > 4096: break
                l = 3
                while l < 273 and i + l < n and d[p + l] == d[i + l]: l += 1
                if l > best: best, bpos = l, p
        if best >= 3:
            dist = i - bpos - 1
            if best < 18: grp += bytes([(best - 2) << 4 | dist >> 8, dist & 255])
            else: grp += bytes([dist >> 8, dist & 255, best - 18])
            bits = bits << 1; step = best
        else:
            grp.append(d[i]); bits = bits << 1 | 1; step = 1
        cnt += 1
        for k in range(step): add(i + k)
        i += step
        if cnt == 8: flush()
    flush()
    return bytes(out)

def u8_parse(d):
    off, hs = struct.unpack('>II', d[4:12]); cnt = struct.unpack('>I', d[off+8:off+12])[0]
    nodes = [struct.unpack('>III', d[off+i*12:off+i*12+12]) for i in range(cnt)]
    return off, hs, nodes

def u8_rebuild(d, repl):
    """repl: {node index: new bytes}. Returns new U8 with files re-laid-out in original order."""
    off, hs, nodes = u8_parse(d)
    head = bytearray(d[off:off + hs])   # node table + string table
    files = sorted((i for i, (t, a, b) in enumerate(nodes) if t >> 24 == 0), key=lambda i: nodes[i][1])
    data_off = (off + hs + 0x1F) & ~0x1F
    pos = data_off; blobs = []
    for i in files:
        t, a, b = nodes[i]; blob = repl.get(i, d[a:a+b])
        pos = (pos + 0x1F) & ~0x1F
        struct.pack_into('>III', head, i*12, t, pos, len(blob))
        blobs.append((pos, blob)); pos += len(blob)
    out = bytearray(b'U\xaa8-' + struct.pack('>II', off, hs) + struct.pack('>I', data_off) + bytes(16))
    out = out[:off] + head; out += bytes(data_off - len(out))
    for p, b in blobs:
        out += bytes(p - len(out)); out += b
    return bytes(out)

def bmg_patch(b):
    """Returns patched BMG bytes or None when nothing changed."""
    nsec = struct.unpack('>I', b[12:16])[0]; p = 32; sec = {}
    for _ in range(nsec):
        sec[b[p:p+4]] = (p, struct.unpack('>I', b[p+4:p+8])[0]); p += sec[b[p:p+4]][1]
    ip, isz = sec[b'INF1']; dp, dsz = sec[b'DAT1']
    n, esz = struct.unpack('>HH', b[ip+8:ip+12])
    offs = [struct.unpack('>I', b[ip+16+i*esz:ip+20+i*esz])[0] for i in range(n)]
    base = dp + 8; newdat = bytearray(); remap = {}; changed = False
    for o in sorted(set(offs)):
        s = base + o; e = s
        while b[e:e+2] != b'\0\0':   # escapes (0x001A, size byte, ...) may hold zero units: skip them whole
            e += b[e+2] if b[e:e+2] == b'\0\x1a' else 2
        raw = b[s:e + 2]
        for old in OLD:
            r = raw.replace(old.encode('utf-16-be'), NEW.encode('utf-16-be'))
            if r != raw: changed = True; raw = r
        remap[o] = len(newdat); newdat += raw
    if not changed: return None
    newdat += bytes((-len(newdat) - 8) % 32)
    out = bytearray(b)
    for i in range(n): struct.pack_into('>I', out, ip+16+i*esz, remap[offs[i]])
    out = out[:dp] + out[dp:dp+4] + struct.pack('>I', 8 + len(newdat)) + newdat + out[dp+dsz:]
    struct.pack_into('>I', out, 8, len(out))
    return bytes(out)

def patch_file(path):
    raw = open(path, 'rb').read(); d = yaz0_dec(raw) if raw[:4] == b'Yaz0' else raw
    if d[:4] != b'U\xaa8-': return None
    off, hs, nodes = u8_parse(d); repl = {}
    names = d[off + len(nodes)*12:off + hs]
    for i, (t, a, b) in enumerate(nodes):
        if t >> 24: continue
        nm = names[t & 0xffffff:].split(b'\0')[0]
        if nm.endswith(b'.bmg'):
            r = bmg_patch(d[a:a+b])
            if r is not None: repl[i] = r
    if not repl: return None
    new = yaz0_enc(u8_rebuild(d, repl))
    tmp = path + '.tmp'; open(tmp, 'wb').write(new); os.replace(tmp, path)   # os.replace breaks hard links
    return len(new)

if __name__ == '__main__':
    dist = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), '..', 'dist')
    man_p = os.path.join(dist, 'disc-manifest.json'); man = json.load(open(man_p)); n = 0
    for f in sorted(glob.glob(os.path.join(dist, 'disc/files/Scene/UI/*.szs'))):
        sz = patch_file(f)
        if sz is not None:
            man['files'][os.path.relpath(f, os.path.join(dist, 'disc')).replace(os.sep, '/')] = sz; n += 1
    man['totalBytes'] = sum(man['files'].values())
    json.dump(man, open(man_p, 'w'), indent=1)
    print('patched', n, 'archives')
