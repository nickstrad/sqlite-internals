/* C helpers shared by the Go files of package sqlitec. */
int sqlitec_vfstrace_register(const char *zName, int makeDefault);
int sqlitec_faultvfs_register(const char *zName, int makeDefault);
void sqlitec_fault_set(int failWriteAt, int failSyncAt, int exitAtSync);
void sqlitec_fault_counts(int *nWrite, int *nSync);
void sqlitec_wal_hook_install(sqlite3 *db);
