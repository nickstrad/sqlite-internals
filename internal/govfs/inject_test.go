package govfs

import (
	"sync"
	"testing"
)

func TestInjectedCallIsCompleteAtDisarm(t *testing.T) {
	v := &VFS{recording: true}
	if err := v.Arm(Fault{Method: "xWrite", Nth: 2, Code: CodeIOErr}); err != nil {
		t.Fatal(err)
	}
	first := Call{Method: "xWrite", File: "a.db", Kind: KindMainDB, Amount: 4096}
	if _, ok := v.injectFault(first); ok {
		t.Fatal("fired on the first call")
	}
	v.record(first)
	second := Call{Method: "xWrite", File: "a.db", Kind: KindMainDB, Amount: 4096, Offset: 4096}
	code, ok := v.injectFault(second)
	if !ok || code != CodeIOErr {
		t.Fatalf("code=%d ok=%t", code, ok)
	}
	report := v.Disarm()
	if !report.Fired || report.Call.Method != "xWrite" || report.Call.Offset != 4096 || !report.Call.Injected || report.Call.Result != CodeIOErr || report.Call.Seq != 2 {
		t.Fatalf("%+v", report)
	}
	calls := v.Calls()
	if len(calls) != 2 || calls[1] != report.Call {
		t.Fatalf("%+v", calls)
	}
}

func TestInjectedCallWithoutRecording(t *testing.T) {
	v := &VFS{}
	if err := v.Arm(Fault{Method: "xSync", Nth: 1, Code: CodeIOErr}); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.injectFault(Call{Method: "xSync", File: "a.db"}); !ok {
		t.Fatal("did not fire")
	}
	report := v.Disarm()
	if !report.Fired || report.Call.Method != "xSync" || report.Call.Seq != 0 || v.seq != 0 || len(v.Calls()) != 0 {
		t.Fatalf("report=%+v seq=%d", report, v.seq)
	}
}

func TestDisarmRacingInjectSeesWholeCall(t *testing.T) {
	for range 200 {
		v := &VFS{recording: true}
		if err := v.Arm(Fault{Method: "xRead", Nth: 1, Code: CodeIOErr}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var report FaultReport
		wg.Add(2)
		go func() {
			defer wg.Done()
			v.injectFault(Call{Method: "xRead", File: "a.db"})
		}()
		go func() {
			defer wg.Done()
			report = v.Disarm()
		}()
		wg.Wait()
		if report.Fired && report.Call.Method != "xRead" {
			t.Fatalf("fired with partial call %+v", report.Call)
		}
	}
}
