#!/usr/bin/env python3
"""pagediff.py A B [--ignore-header]: page-by-page diff of two db files.
Prints page size, page counts, and for each differing page the byte ranges that differ.
--ignore-header masks header bytes 24-27 (change counter), 40-43 (schema cookie), 92-99
(version-valid-for, SQLITE_VERSION_NUMBER) before comparing page 1."""
import sys, struct
args=[a for a in sys.argv[1:] if not a.startswith('--')]
ign='--ignore-header' in sys.argv
a=open(args[0],'rb').read(); b=open(args[1],'rb').read()
ps=struct.unpack('>H',a[16:18])[0]; ps=65536 if ps==1 else ps
na,nb=len(a)//ps,len(b)//ps
print(f"page_size={ps} pages A={na} B={nb}")
MASK=[(24,28),(40,44),(92,100)]
diffs=0
for p in range(max(na,nb)):
    pa=a[p*ps:(p+1)*ps]; pb=b[p*ps:(p+1)*ps]
    if p==0 and ign:
        pa=bytearray(pa); pb=bytearray(pb)
        for s,e in MASK: pa[s:e]=b'\0'*(e-s); pb[s:e]=b'\0'*(e-s)
    if pa!=pb:
        diffs+=1
        rng=[];i=0
        while i<min(len(pa),len(pb)):
            if pa[i]!=pb[i]:
                j=i
                while j<min(len(pa),len(pb)) and pa[j]!=pb[j]: j+=1
                rng.append(f"{i}-{j-1}"); i=j
            else: i+=1
        if diffs<=12: print(f" page {p+1} differs: bytes {','.join(rng[:8])}{' ...' if len(rng)>8 else ''} ({len(rng)} ranges)")
print(f"differing pages: {diffs}")
sys.exit(1 if diffs else 0)
