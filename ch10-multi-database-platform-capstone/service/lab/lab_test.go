package lab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

// TestTrackerReadsTracedOutput checks NoteOutput and Note record bounds, problems, and replies while ignoring an incomplete trailing line.
func TestTrackerReadsTracedOutput(t *testing.T) {
	var tracker Tracker
	replies := tracker.NoteOutput("a", "ready\nok write db-1 local-durable: x\nbounds descriptors=1 memory=2 ok\nproblem evict db-1: boom\nrefused write db-2 store-durable: not the owner\nbounds descriptors=1 memory=2 EXCEEDED: too many\nok wri")
	if len(replies) != 2 || tracker.Replies != 2 || len(tracker.Problems) != 1 || len(tracker.OverBounds) != 1 {
		t.Fatalf("%q %+v", replies, tracker)
	}
	tracker.Note("b", node.Reply{Line: "ok", Bounds: "bounds descriptors=0 memory=0 ok", Problems: []string{"p"}})
	if tracker.Replies != 3 || len(tracker.Problems) != 2 || len(tracker.OverBounds) != 1 {
		t.Fatalf("%+v", tracker)
	}
}

// TestParsePassIsStrict verifies ParsePass accepts canonical checker pass replies and rejects mismatched failure counts, padding, or unknown lines.
func TestParsePassIsStrict(t *testing.T) {
	good := node.Reply{Line: "ok pass: databases 2, restore points 9, failed 1, bytes read 100", Problems: []string{"failed db-2: unrecoverable past position 3 (last good restore point 3): gap"}}
	pass, err := ParsePass(good)
	if err != nil || pass.Databases != 2 || pass.RestorePoints != 9 || pass.Failed != 1 || pass.BytesRead != 100 || len(pass.FailedLines) != 1 {
		t.Fatalf("%+v %v", pass, err)
	}
	for _, bad := range []node.Reply{
		{Line: "ok pass: databases 2, restore points 9, failed 1, bytes read 100"},
		{Line: "ok pass: databases 2, restore points 9, failed 0, bytes read 100", Problems: []string{"noise"}},
		{Line: "ok pass: databases 02, restore points 9, failed 0, bytes read 100"},
		{Line: "refused pass: error: x"},
		{Line: "ok pass: databases 2, restore points 9, failed 0"},
	} {
		if _, err := ParsePass(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// TestMatchImageNamesRecipeStates verifies MatchImage accepts an unmodified recipe step, rejects a 1-bit flip or truncated image, and checks Ids formatting.
func TestMatchImageNamesRecipeStates(t *testing.T) {
	ctx := context.Background()
	r := recipe.Default()
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "r.db")
	if err := r.Build(ctx, path, 2); err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := MatchImage(hashes, image)
	if err != nil || !state.RecipeState || state.UserVersion != 2 || state.Match.Step != 2 {
		t.Fatalf("%+v %v", state, err)
	}
	changed := append([]byte(nil), image...)
	changed[len(changed)-1] ^= 1
	if state, err := MatchImage(hashes, changed); err != nil || state.RecipeState {
		t.Fatalf("a changed image matched: %+v %v", state, err)
	}
	if _, err := MatchImage(hashes, image[:50]); err == nil {
		t.Fatal("a short image matched")
	}
	if ids := Ids("db", 3); len(ids) != 3 || ids[0] != "db-0001" || ids[2] != "db-0003" {
		t.Fatal(ids)
	}
}

// TestDatabasesInRefusesForeignKeys checks DatabasesIn extracts sorted database ids from fenced slot keys and rejects keys outside dbs/<id>/log/<pos>.
func TestDatabasesInRefusesForeignKeys(t *testing.T) {
	ctx := context.Background()
	store, err := walship.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dbs/db-2/log/000000000001", "dbs/db-1/log/000000000001", "dbs/db-1/log/000000000002"} {
		if err := store.PutIfAbsent(ctx, key, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := DatabasesIn(ctx, store)
	if err != nil || len(ids) != 2 || ids[0] != "db-1" || ids[1] != "db-2" {
		t.Fatalf("%v %v", ids, err)
	}
	if err := store.PutIfAbsent(ctx, "dbs/db-3/other", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := DatabasesIn(ctx, store); err == nil {
		t.Fatal("a foreign key was accepted")
	}
}
