package govfs

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	sqlite3 "modernc.org/sqlite/lib"
)

const translatedPackage = "modernc.org/sqlite/lib."

// Target pairs one intercepted Slot with the fully qualified Go function name
// resolved from the underlying VFS or sqlite3_io_methods table at run time.
type Target struct {
	Slot     Slot
	Function string
}

var checkedTables = struct {
	sync.Mutex
	result  map[uintptr]error
	targets map[uintptr][]Target
}{result: map[uintptr]error{}, targets: map[uintptr][]Target{}}

func functionName(address uintptr) string {
	if address == 0 {
		return ""
	}
	f := runtime.FuncForPC(codeAddress(address))
	if f == nil {
		return ""
	}
	return f.Name()
}

// checkTarget verifies that a stored C function pointer resolves to a function
// in modernc.org/sqlite/lib whose name ends with the slot's suffix (for
// example xWrite -> _unixWrite, xLock -> _unixLock or _dotlockLock).
func checkTarget(slot Slot, address uintptr) (Target, error) {
	name := functionName(address)
	target := Target{Slot: slot, Function: name}
	want := strings.TrimPrefix(slot.Method, "x")
	short, ok := strings.CutPrefix(name, translatedPackage)
	if !ok || !strings.HasSuffix(short, want) {
		return target, fmt.Errorf("slot %s points at %q, want a function of %s ending in %q", slot.Method, name, strings.TrimSuffix(translatedPackage, "."), want)
	}
	return target, nil
}

func realVFSTargets(real uintptr) ([]Target, error) {
	table := at[sqlite3.Tsqlite3_vfs](real)
	var targets []Target
	var problems []string
	for _, slot := range vfsSlots {
		if slot.MinVersion > table.FiVersion {
			continue
		}
		address := *slot.field(table)
		if address == 0 {
			continue
		}
		target, err := checkTarget(slot.Slot, address)
		targets = append(targets, target)
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return targets, fmt.Errorf("govfs self-check of the default VFS failed: %s", strings.Join(problems, "; "))
	}
	return targets, nil
}

func checkRealVFS(real uintptr) error {
	table := at[sqlite3.Tsqlite3_vfs](real)
	if table.FiVersion < 1 || table.FszOsFile <= 0 || table.FmxPathname <= 0 {
		return fmt.Errorf("govfs self-check: default VFS looks wrong (iVersion=%d szOsFile=%d mxPathname=%d)", table.FiVersion, table.FszOsFile, table.FmxPathname)
	}
	_, err := realVFSTargets(real)
	return err
}

// checkRealMethods validates a newly encountered real sqlite3_io_methods table
// once and caches both the check result and resolved Targets for signature tests.
func checkRealMethods(real uintptr) error {
	checkedTables.Lock()
	defer checkedTables.Unlock()
	if err, ok := checkedTables.result[real]; ok {
		return err
	}
	table := at[sqlite3.Tsqlite3_io_methods](real)
	var targets []Target
	var problems []string
	if table.FiVersion < 1 || table.FiVersion > 3 {
		problems = append(problems, fmt.Sprintf("io_methods iVersion %d not in 1..3", table.FiVersion))
	}
	for _, slot := range ioSlots {
		if slot.MinVersion > table.FiVersion {
			continue
		}
		address := *slot.field(table)
		if address == 0 {
			continue
		}
		target, err := checkTarget(slot.Slot, address)
		targets = append(targets, target)
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	var err error
	if len(problems) > 0 {
		err = fmt.Errorf("govfs self-check of io_methods failed: %s", strings.Join(problems, "; "))
	}
	checkedTables.result[real] = err
	checkedTables.targets[real] = targets
	return err
}

func selfCheckDouble(x int32) int32 {
	return 2 * x
}

// checkFunctionPointerConvention verifies at startup that funcAddress, funcAt,
// and codeAddress round-trip a top-level Go function through a uintptr pointer
// on the current Go toolchain and architecture.
func checkFunctionPointerConvention() error {
	address := funcAddress(selfCheckDouble)
	if got := funcAt[func(int32) int32](address)(21); got != 42 {
		return fmt.Errorf("govfs self-check: a Go func stored as a C function pointer returned %d, want 42", got)
	}
	if name := functionName(address); !strings.HasSuffix(name, ".selfCheckDouble") {
		return fmt.Errorf("govfs self-check: function pointer resolves to %q, want selfCheckDouble", name)
	}
	return nil
}

// ForwardTargets returns every underlying modernc.org/sqlite/lib function
// resolved so far across the default VFS and all checked io_methods tables.
func ForwardTargets() []Target {
	registry.RLock()
	real := registry.realVFS
	registry.RUnlock()
	var targets []Target
	if real != 0 {
		t, _ := realVFSTargets(real)
		targets = append(targets, t...)
	}
	checkedTables.Lock()
	defer checkedTables.Unlock()
	for _, t := range checkedTables.targets {
		targets = append(targets, t...)
	}
	return targets
}
