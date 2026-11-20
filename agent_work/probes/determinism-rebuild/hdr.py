#!/usr/bin/env python3
"""Print the variable fields of a SQLite database header (first 100 bytes)."""
import struct, sys
F = [(18,1,'write_version'),(19,1,'read_version'),(24,4,'file_change_counter'),(28,4,'db_size_pages'),
     (32,4,'first_freelist_trunk'),(36,4,'freelist_count'),(40,4,'schema_cookie'),(44,4,'schema_format'),
     (48,4,'default_cache_size'),(52,4,'largest_root_page(autovac)'),(56,4,'text_encoding'),(60,4,'user_version'),
     (64,4,'incremental_vacuum'),(68,4,'application_id'),(92,4,'version_valid_for'),(96,4,'sqlite_version_number')]
for path in sys.argv[1:]:
    h = open(path,'rb').read(100)
    vals = []
    for off,n,name in F:
        v = h[off] if n==1 else struct.unpack('>I',h[off:off+4])[0]
        vals.append(f"{name}@{off}={v}")
    print(path, ' '.join(vals))
