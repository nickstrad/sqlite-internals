/* vfstrace from the pinned tree, registered into whichever SQLite the binary links. */
#include <ctype.h>
#include "sqlite3.h"
typedef sqlite3_int64 i64;   /* vfstrace.c at 3.53.4 needs these when compiled standalone */
typedef unsigned char u8;
#include "vfstrace.c.inc"
#include "_cgo_export.h"
static int out(const char *z, void *p){ (void)p; goShimTrace((char*)z); return 0; }
int mattnshim_register(void){ return vfstrace_register("trace", 0, out, 0, 0); }
