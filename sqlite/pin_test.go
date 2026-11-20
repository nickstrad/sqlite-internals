package sqlite

import (
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

// TestCurrentPinParses checks that the embedded sqlite/PIN file parses to release 3.53.4 with matching IDs.
func TestCurrentPinParses(t *testing.T) {
	pin := Current()
	if pin.Release != "3.53.4" || pin.Tag != "version-3.53.4" {
		t.Fatalf("unexpected pin %+v", pin)
	}
	if !strings.HasSuffix(pin.SourceID, pin.FossilID) {
		t.Fatalf("source id %q does not carry fossil id %q", pin.SourceID, pin.FossilID)
	}
}

// TestPinMatchesGoMod verifies that go.mod requires the driver version recorded
// in sqlite/PIN, and checks debug.ReadBuildInfo when dependency metadata is present.
func TestPinMatchesGoMod(t *testing.T) {
	data, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	pin := Current()
	want := pin.DriverModule + " " + pin.DriverVersion
	if !strings.Contains(string(data), want) {
		t.Fatalf("go.mod does not require %q", want)
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == pin.DriverModule && dep.Version != pin.DriverVersion {
				t.Fatalf("build uses %s %s, PIN says %s", dep.Path, dep.Version, pin.DriverVersion)
			}
		}
	}
}

// TestParsePinRejectsBadInput ensures malformed lines, duplicates, missing keys, and mismatched IDs fail to parse.
func TestParsePinRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"missing equals": "release 3.53.4\n",
		"duplicate key":  "release=1\nrelease=2\n",
		"missing keys":   "release=3.53.4\n",
	}
	for name, text := range cases {
		if _, err := ParsePin(text); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	mismatched := strings.Replace(pinFile, "source_id=2026-07-24 19:02:57 bf7c", "source_id=2026-07-24 19:02:57 aaaa", 1)
	if _, err := ParsePin(mismatched); err == nil {
		t.Errorf("mismatched source_id and fossil_id: expected an error")
	}
}
