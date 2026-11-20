/* libopts.c -- print sqlite_source_id() and PRAGMA compile_options of the INSTALLED
** library (/usr/local/lib/libsqlite3.so), which is what a cgo Go lab links against.
** The sqlite3 CLI embeds its own copy of the library, so its compile_options differ.
** Build: gcc -I/usr/local/include libopts.c -L/usr/local/lib -lsqlite3 -Wl,-rpath,/usr/local/lib -o /tmp/.../libopts
*/
#include <stdio.h>
#include <sqlite3.h>
static int cb(void *u, int n, char **v, char **c){ (void)u;(void)n;(void)c; printf("%s\n", v[0]); return 0; }
int main(void){
  sqlite3 *db; char *err = 0;
  printf("source_id=%s\n", sqlite3_sourceid());
  if( sqlite3_open(":memory:", &db) ) return 1;
  sqlite3_exec(db, "PRAGMA compile_options", cb, 0, &err);
  const char *probes[] = {"SELECT count(*) FROM bytecode('select 1')", "SELECT count(*) FROM sqlite_dbpage",
    "SELECT count(*) FROM dbstat", "SELECT count(*) FROM sqlite_stmt", "PRAGMA vdbe_trace", 0};
  for(int i=0; probes[i]; i++){
    err = 0; int rc = sqlite3_exec(db, probes[i], 0, 0, &err);
    printf("probe %-45s rc=%d %s\n", probes[i], rc, err ? err : "");
    sqlite3_free(err);
  }
  sqlite3_close(db); return 0;
}
