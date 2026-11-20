/* Minimal fault-injection VFS shim over the default ("unix") VFS. os_unix.c still does all
** real work; this layer only counts xWrite/xSync across all files and can
**   - fail the Nth xWrite with SQLITE_IOERR_WRITE,
**   - fail the Nth xSync with SQLITE_IOERR_FSYNC,
**   - _exit(9) at the start of the Nth xSync (a deterministic "kill -9 before fsync").
** Feasibility probe only: not thread-safe counters, no power-loss (unsynced-write drop) model. */
#include "sqlite3.h"
#include <string.h>
#include <unistd.h>
#include "glue.h"

typedef struct FFile { sqlite3_file base; sqlite3_file *pReal; } FFile;
static sqlite3_vfs *pRoot;
static sqlite3_vfs fvfs;
static int nWrite, nSync, failWriteAt, failSyncAt, exitAtSync;

#define REAL(f) (((FFile*)(f))->pReal)
static int fClose(sqlite3_file *f){ int rc = REAL(f)->pMethods->xClose(REAL(f)); return rc; }
static int fRead(sqlite3_file *f, void *b, int n, sqlite3_int64 o){ return REAL(f)->pMethods->xRead(REAL(f), b, n, o); }
static int fWrite(sqlite3_file *f, const void *b, int n, sqlite3_int64 o){
  if( ++nWrite==failWriteAt ) return SQLITE_IOERR_WRITE;
  return REAL(f)->pMethods->xWrite(REAL(f), b, n, o);
}
static int fTruncate(sqlite3_file *f, sqlite3_int64 s){ return REAL(f)->pMethods->xTruncate(REAL(f), s); }
static int fSync(sqlite3_file *f, int fl){
  ++nSync;
  if( nSync==exitAtSync ) _exit(9);
  if( nSync==failSyncAt ) return SQLITE_IOERR_FSYNC;
  return REAL(f)->pMethods->xSync(REAL(f), fl);
}
static int fFileSize(sqlite3_file *f, sqlite3_int64 *p){ return REAL(f)->pMethods->xFileSize(REAL(f), p); }
static int fLock(sqlite3_file *f, int l){ return REAL(f)->pMethods->xLock(REAL(f), l); }
static int fUnlock(sqlite3_file *f, int l){ return REAL(f)->pMethods->xUnlock(REAL(f), l); }
static int fCheckRes(sqlite3_file *f, int *p){ return REAL(f)->pMethods->xCheckReservedLock(REAL(f), p); }
static int fFcntl(sqlite3_file *f, int op, void *a){ return REAL(f)->pMethods->xFileControl(REAL(f), op, a); }
static int fSector(sqlite3_file *f){ return REAL(f)->pMethods->xSectorSize(REAL(f)); }
static int fDevChar(sqlite3_file *f){ return REAL(f)->pMethods->xDeviceCharacteristics(REAL(f)); }
static int fShmMap(sqlite3_file *f, int i, int s, int w, void volatile **pp){ return REAL(f)->pMethods->xShmMap(REAL(f), i, s, w, pp); }
static int fShmLock(sqlite3_file *f, int o, int n, int fl){ return REAL(f)->pMethods->xShmLock(REAL(f), o, n, fl); }
static void fShmBarrier(sqlite3_file *f){ REAL(f)->pMethods->xShmBarrier(REAL(f)); }
static int fShmUnmap(sqlite3_file *f, int d){ return REAL(f)->pMethods->xShmUnmap(REAL(f), d); }
static int fFetch(sqlite3_file *f, sqlite3_int64 o, int n, void **pp){ return REAL(f)->pMethods->xFetch(REAL(f), o, n, pp); }
static int fUnfetch(sqlite3_file *f, sqlite3_int64 o, void *p){ return REAL(f)->pMethods->xUnfetch(REAL(f), o, p); }

static const sqlite3_io_methods fMethods = {
  3, fClose, fRead, fWrite, fTruncate, fSync, fFileSize, fLock, fUnlock, fCheckRes,
  fFcntl, fSector, fDevChar, fShmMap, fShmLock, fShmBarrier, fShmUnmap, fFetch, fUnfetch
};

static int fOpen(sqlite3_vfs *v, const char *z, sqlite3_file *f, int fl, int *pOut){
  FFile *p = (FFile*)f;
  int rc;
  (void)v;
  p->pReal = (sqlite3_file*)&p[1];
  rc = pRoot->xOpen(pRoot, z, p->pReal, fl, pOut);
  p->base.pMethods = p->pReal->pMethods ? &fMethods : 0;
  return rc;
}
static int fDelete(sqlite3_vfs *v, const char *z, int d){ (void)v; return pRoot->xDelete(pRoot, z, d); }
static int fAccess(sqlite3_vfs *v, const char *z, int fl, int *r){ (void)v; return pRoot->xAccess(pRoot, z, fl, r); }
static int fFull(sqlite3_vfs *v, const char *z, int n, char *o){ (void)v; return pRoot->xFullPathname(pRoot, z, n, o); }
static int fRand(sqlite3_vfs *v, int n, char *o){ (void)v; return pRoot->xRandomness(pRoot, n, o); }
static int fSleep(sqlite3_vfs *v, int us){ (void)v; return pRoot->xSleep(pRoot, us); }
static int fTime(sqlite3_vfs *v, double *t){ (void)v; return pRoot->xCurrentTime(pRoot, t); }
static int fTime64(sqlite3_vfs *v, sqlite3_int64 *t){ (void)v; return pRoot->xCurrentTimeInt64(pRoot, t); }
static int fLastErr(sqlite3_vfs *v, int n, char *z){ (void)v; return pRoot->xGetLastError(pRoot, n, z); }

int sqlitec_faultvfs_register(const char *zName, int makeDefault){
  pRoot = sqlite3_vfs_find(0);
  if( pRoot==0 ) return SQLITE_NOTFOUND;
  memset(&fvfs, 0, sizeof(fvfs));
  fvfs.iVersion = 2;
  fvfs.szOsFile = sizeof(FFile) + pRoot->szOsFile;
  fvfs.mxPathname = pRoot->mxPathname;
  fvfs.zName = sqlite3_mprintf("%s", zName);
  fvfs.xOpen = fOpen; fvfs.xDelete = fDelete; fvfs.xAccess = fAccess;
  fvfs.xFullPathname = fFull; fvfs.xRandomness = fRand; fvfs.xSleep = fSleep;
  fvfs.xCurrentTime = fTime; fvfs.xGetLastError = fLastErr; fvfs.xCurrentTimeInt64 = fTime64;
  return sqlite3_vfs_register(&fvfs, makeDefault);
}
void sqlitec_fault_set(int w, int s, int e){ failWriteAt = w; failSyncAt = s; exitAtSync = e; nWrite = nSync = 0; }
void sqlitec_fault_counts(int *w, int *s){ *w = nWrite; *s = nSync; }
