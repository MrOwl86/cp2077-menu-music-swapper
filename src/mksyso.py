# Builds rsrc_windows_amd64.syso (icon, version info, manifest) for the Go linker.
# Usage: python mksyso.py icon.png out.syso "Product name" "File description"
import sys, struct, io
from PIL import Image

def png(im, s):
    b = io.BytesIO(); im.resize((s, s), Image.LANCZOS).save(b, 'PNG', optimize=True); return b.getvalue()

def u16(s): return s.encode('utf-16-le') + b'\x00\x00'
def pad4(b): return b + b'\x00' * ((-len(b)) % 4)

def vblock(key, value=b'', vlen=0, typ=1, children=b''):
    head = struct.pack('<HHH', 0, vlen, typ) + u16(key)
    head = pad4(head)
    body = head + value
    body = pad4(body) + children
    body = struct.pack('<H', len(body)) + body[2:]
    return pad4(body)

def version_info(product, desc, filename):
    ffi = struct.pack('<IIIIIIIIIIIII', 0xFEEF04BD, 0x00010000, 0x00010000, 0, 0x00010000, 0, 0x3F, 0, 0x00040004, 1, 0, 0, 0)
    strs = [('CompanyName', 'Menu Music Mod'), ('FileDescription', desc), ('FileVersion', '1.0.0.0'),
            ('InternalName', filename), ('LegalCopyright', 'Free mod tool'), ('OriginalFilename', filename),
            ('ProductName', product), ('ProductVersion', '1.0.0.0')]
    entries = b''
    for k, v in strs:
        val = u16(v)
        entries += vblock(k, val, len(val) // 2, 1)
    table = vblock('040904B0', b'', 0, 1, entries)
    sfi = vblock('StringFileInfo', b'', 0, 1, table)
    var = vblock('Translation', struct.pack('<I', 0x04B00409), 4, 0)
    vfi = vblock('VarFileInfo', b'', 0, 1, var)
    return vblock('VS_VERSION_INFO', ffi, 52, 0, sfi + vfi)

MANIFEST = b'''<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
 <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3"><security><requestedPrivileges>
  <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
 </requestedPrivileges></security></trustInfo>
 <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1"><application>
  <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
  <supportedOS Id="{1f676c76-80e1-4239-95bb-83d0f6d0da78}"/>
 </application></compatibility>
 <application xmlns="urn:schemas-microsoft-com:asm.v3"><windowsSettings>
  <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true/pm</dpiAware>
 </windowsSettings></application>
</assembly>
'''

def build(img, out, product, desc, filename):
    im = img.convert('RGBA')
    sizes = [16, 24, 32, 48, 64, 128, 256]
    icons = [png(im, s) for s in sizes]
    grp = struct.pack('<HHH', 0, 1, len(sizes))
    for i, (s, b) in enumerate(zip(sizes, icons)):
        grp += struct.pack('<BBBBHHIH', 0 if s >= 256 else s, 0 if s >= 256 else s, 0, 0, 1, 32, len(b), i + 1)
    # (type, id, data)
    res = [(3, i + 1, b) for i, b in enumerate(icons)] + [(14, 1, grp), (16, 1, version_info(product, desc, filename)), (24, 1, MANIFEST)]
    types = sorted(set(t for t, _, _ in res))
    def d(entries, n):  # entries: list of (id, off)
        out_ = struct.pack('<IIHHHH', 0, 0, 0, 0, 0, n)
        for i, o in entries: out_ += struct.pack('<II', i, o)
        return out_
    # layout offsets
    root = 16 + 8 * len(types)
    off = root
    type_off = {}
    for t in types:
        type_off[t] = off; off += 16 + 8 * sum(1 for x in res if x[0] == t)
    id_off = {}
    for t, i, _ in res:
        id_off[(t, i)] = off; off += 24
    de_off = {}
    for t, i, _ in res:
        de_off[(t, i)] = off; off += 16
    blob_off = {}; p = off
    for t, i, b in res:
        blob_off[(t, i)] = p; p += (len(b) + 3) & ~3
    raw = bytearray(p)
    def put(o, b): raw[o:o + len(b)] = b
    put(0, d([(t, 0x80000000 | type_off[t]) for t in types], len(types)))
    for t in types:
        ids = [i for tt, i, _ in res if tt == t]
        put(type_off[t], d([(i, 0x80000000 | id_off[(t, i)]) for i in ids], len(ids)))
    relocs = []
    for t, i, b in res:
        put(id_off[(t, i)], d([(0x409, de_off[(t, i)])], 1))
        put(de_off[(t, i)], struct.pack('<IIII', blob_off[(t, i)], len(b), 0, 0))
        relocs.append(de_off[(t, i)])
        put(blob_off[(t, i)], b)
    total = len(raw); hdr = 60
    reloc_off = hdr + total; sym_off = reloc_off + 10 * len(relocs)
    coff = struct.pack('<HHIIIHH', 0x8664, 1, 0, sym_off, 1, 0, 0)
    sec = struct.pack('<8sIIIIIIHHI', b'.rsrc', 0, 0, total, hdr, reloc_off, 0, len(relocs), 0, 0xC0300040)
    rel = b''.join(struct.pack('<IIH', r, 0, 3) for r in relocs)
    sym = struct.pack('<8sIhHBB', b'.rsrc', 0, 1, 0, 3, 0)
    open(out, 'wb').write(coff + sec + bytes(raw) + rel + sym + struct.pack('<I', 4))

if __name__ == '__main__':
    build(Image.open(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5])
