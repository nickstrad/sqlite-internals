/* Compiles the tree's own ext/misc/vfstrace.c (copied in as vfstrace.c.inc by
** gen-amalgamation.sh) and routes its output to Go, plus a WAL-hook trampoline. */
#include <ctype.h>
#include "sqlite3.h"
/* ext/misc/vfstrace.c at 3.53.4 uses the types i64 (lines 849, 860) and u8 (lines 143, 583), which only exists when it
** is INCLUDEd into shell.c; supply it so the file compiles standalone. */
typedef sqlite3_int64 i64;
typedef unsigned char u8;
#include "vfstrace.c.inc"
#include "_cgo_export.h"
#include "glue.h"

static int traceOut(const char *z, void *pArg){ (void)pArg; goVfsTrace((char*)z); return 0; }

int sqlitec_vfstrace_register(const char *zName, int makeDefault){
  return vfstrace_register(zName, 0, traceOut, 0, makeDefault);
}

static int walHook(void *p, sqlite3 *db, const char *zDb, int nFrame){
  (void)p; (void)db; goWalHook((char*)zDb, nFrame); return SQLITE_OK;
}
void sqlitec_wal_hook_install(sqlite3 *db){ sqlite3_wal_hook(db, walHook, 0); }
