package trace

import "fmt"

type SyncRecord struct {
	Path           string
	Syncs          int
	LastSync       int
	Unsynced       []Syscall
	FirstUnsynced  int64
	HasUnsynced    bool
	UnfinishedSeen bool
}

func (r SyncRecord) String() string {
	if !r.HasUnsynced {
		return fmt.Sprintf("%d sync(s); no write after the last one", r.Syncs)
	}
	return fmt.Sprintf("%d sync(s); %d write(s) after the last one, the lowest at offset %d", r.Syncs, len(r.Unsynced), r.FirstUnsynced)
}

func isSync(name string) bool { return name == "fsync" || name == "fdatasync" }

func isFileWrite(name string) bool {
	switch name {
	case "pwrite64", "pwrite", "write", "writev", "ftruncate":
		return true
	}
	return false
}

func SyncsOf(calls []Syscall, path string) SyncRecord {
	record := SyncRecord{Path: path, LastSync: -1}
	for i, call := range calls {
		if call.Path != path {
			continue
		}
		if call.Unfinished {
			record.UnfinishedSeen = true
			continue
		}
		if isSync(call.Name) && call.Succeeded() {
			record.Syncs++
			record.LastSync = i
			record.Unsynced = nil
			continue
		}
		if isFileWrite(call.Name) && call.Succeeded() {
			record.Unsynced = append(record.Unsynced, call)
		}
	}
	record.HasUnsynced = len(record.Unsynced) > 0
	for i, call := range record.Unsynced {
		offset := call.Offset
		if call.Name == "ftruncate" {
			offset = call.Length
		}
		if i == 0 || offset < record.FirstUnsynced {
			record.FirstUnsynced = offset
		}
	}
	return record
}
