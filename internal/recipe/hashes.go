package recipe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/sqlite"
)

// FileSHA256 returns the lowercase hex SHA-256 digest of the file at path.
// Never call it on a file currently held open by a SQLite connection in the
// same process: closing the extra file descriptor drops the process's POSIX
// locks on that file.
func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ChangeCounterRanges lists the half-open byte offsets in page 1's 100-byte
// header updated by pager_write_changecounter (src/pager.c:3130-3143):
// bytes 24..27 (file change counter) and 92..95 (version-valid-for number).
var ChangeCounterRanges = [][2]int{{24, 28}, {92, 96}}

// MaskChangeCounter returns a copy of image with the change-counter header
// bytes zeroed so two WAL databases built with different connection session
// counts can still be compared byte for byte.
func MaskChangeCounter(image []byte) []byte {
	masked := append([]byte(nil), image...)
	for _, span := range ChangeCounterRanges {
		for i := span[0]; i < span[1] && i < len(masked); i++ {
			masked[i] = 0
		}
	}
	return masked
}

// FileHashes holds the SHA-256 digests of a database file both verbatim and
// with ChangeCounterRanges zeroed.
type FileHashes struct {
	Exact  string
	Masked string
}

// HashFile reads path from disk and returns its exact and masked SHA-256
// hashes. The database must already be closed.
func HashFile(path string) (FileHashes, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return FileHashes{}, err
	}
	return HashImage(image), nil
}

// HashImage returns the exact and change-counter-masked SHA-256 hashes of a
// database file image in memory.
func HashImage(image []byte) FileHashes {
	exact := sha256.Sum256(image)
	masked := sha256.Sum256(MaskChangeCounter(image))
	return FileHashes{Exact: hex.EncodeToString(exact[:]), Masked: hex.EncodeToString(masked[:])}
}

// ComputeStepHashes builds r inside dir from step 0 through r.Steps and
// returns the at-rest FileHashes after every step.
func (r Recipe) ComputeStepHashes(ctx context.Context, dir string) ([]FileHashes, error) {
	path := filepath.Join(dir, "recipe-step-hashes.db")
	defer RemoveDatabase(path)
	var hashes []FileHashes
	err := r.BuildEach(ctx, path, r.Steps, func(k int) error {
		h, err := HashFile(path)
		if err != nil {
			return err
		}
		hashes = append(hashes, h)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hashes, nil
}

func (r Recipe) cacheKey() string {
	// Include FormatVersion and the pinned Fossil check-in so a generator or
	// SQLite upgrade never reuses stale hashes.
	return fmt.Sprintf("v%d-%s-seed%d-%s", FormatVersion, sqlite.Current().FossilID[:16], r.Seed, r.JournalMode)
}

// CacheDir returns the directory under the course work base where computed
// per-step hash lists are cached across experiment runs.
func CacheDir() (string, error) {
	base, err := sqlitex.WorkBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "recipe-cache"), nil
}

// CachePath returns the cache file path for r's seed, mode, format version,
// and pinned SQLite check-in.
func (r Recipe) CachePath() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, r.cacheKey()+".txt"), nil
}

// StepHashes returns the per-step hashes for steps 0..r.Steps, reusing the
// cached file on disk only when it has at least r.Steps+1 entries, its trailing
// SHA-256 checksum matches r's key, SQLite options, plan, and hashes, and a
// fresh step-0 rebuild matches entry 0. Otherwise it rebuilds and replaces the
// cache atomically.
func (r Recipe) StepHashes(ctx context.Context) ([]FileHashes, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	cachePath, err := r.CachePath()
	if err != nil {
		return nil, err
	}
	if cached, err := r.readVerifiedCache(ctx, cachePath); err == nil && len(cached) > r.Steps {
		return cached[:r.Steps+1], nil
	}
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	buildDir, err := os.MkdirTemp(dir, "build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(buildDir)
	hashes, err := r.ComputeStepHashes(ctx, buildDir)
	if err != nil {
		return nil, err
	}
	if err := r.writeHashCache(cachePath, hashes); err != nil {
		return nil, err
	}
	return hashes, nil
}

// ExpectedHash returns the exact at-rest SHA-256 digest for step k of r.
func (r Recipe) ExpectedHash(ctx context.Context, k int) (string, error) {
	if k < 0 || k > r.Steps {
		return "", fmt.Errorf("recipe: step %d out of range 0..%d", k, r.Steps)
	}
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		return "", err
	}
	return hashes[k].Exact, nil
}

// Match identifies which recipe step a database file equals. Step is -1 when
// no step matches; Exact is true for a verbatim SHA-256 match and false when
// only the change-counter-masked hash matched.
type Match struct {
	Step  int
	Exact bool
}

// Found reports whether m matched a step of the recipe.
func (m Match) Found() bool {
	return m.Step >= 0
}

// String describes m for experiment output.
func (m Match) String() string {
	switch {
	case !m.Found():
		return "no recipe step"
	case m.Exact:
		return fmt.Sprintf("step %d (exact bytes)", m.Step)
	default:
		return fmt.Sprintf("step %d (equal except the change counter at bytes 24-27 and 92-95)", m.Step)
	}
}

// MatchHashes searches hashes for h, preferring an exact byte match before
// falling back to a change-counter-masked match.
func MatchHashes(hashes []FileHashes, h FileHashes) Match {
	for k, candidate := range hashes {
		if candidate.Exact == h.Exact {
			return Match{Step: k, Exact: true}
		}
	}
	for k, candidate := range hashes {
		if candidate.Masked == h.Masked {
			return Match{Step: k}
		}
	}
	return Match{Step: -1}
}

// MatchStep checks that path is at rest, hashes it, and reports which step of
// r it equals. A masked-only match is accepted only when the two header change
// counters agree (ChangeCountersAgree).
func (r Recipe) MatchStep(ctx context.Context, path string) (Match, error) {
	if err := CheckAtRest(path); err != nil {
		return Match{Step: -1}, err
	}
	h, err := HashFile(path)
	if err != nil {
		return Match{Step: -1}, err
	}
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		return Match{Step: -1}, err
	}
	match := MatchHashes(hashes, h)
	if match.Found() && !match.Exact {
		agree, err := fileCountersAgree(path)
		if err != nil {
			return Match{Step: -1}, err
		}
		if !agree {
			return Match{Step: -1}, nil
		}
	}
	return match, nil
}

func fileCountersAgree(path string) (bool, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return ChangeCountersAgree(image), nil
}

// ChangeCountersAgree reports whether image is at least 96 bytes long and its
// file change counter (bytes 24..27) equals its version-valid-for number
// (bytes 92..95), which SQLite always updates together in
// pager_write_changecounter (src/pager.c:3130-3143).
func ChangeCountersAgree(image []byte) bool {
	first, second := ChangeCounterRanges[0], ChangeCounterRanges[1]
	if len(image) < second[1] {
		return false
	}
	return bytes.Equal(image[first[0]:first[1]], image[second[0]:second[1]])
}

func isSHA256Hex(text string) bool {
	_, err := hex.DecodeString(text)
	return err == nil && len(text) == sha256.Size*2
}

const cacheChecksumPrefix = "checksum "

// readHashCache parses a v2 step-hash cache file ("k exact masked" lines
// followed by a final "checksum <sha256>" line) without verifying the digest.
func readHashCache(path string) ([]FileHashes, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	var hashes []FileHashes
	var body strings.Builder
	checksum := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if checksum != "" {
			return nil, "", fmt.Errorf("recipe cache %s: line %q after the checksum", path, line)
		}
		if rest, ok := strings.CutPrefix(line, cacheChecksumPrefix); ok {
			if !isSHA256Hex(rest) {
				return nil, "", fmt.Errorf("recipe cache %s: bad checksum line %q", path, line)
			}
			checksum = rest
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, "", fmt.Errorf("recipe cache %s: malformed line %q", path, line)
		}
		k, err := strconv.Atoi(fields[0])
		if err != nil || k != len(hashes) {
			return nil, "", fmt.Errorf("recipe cache %s: line %q out of order", path, line)
		}
		if !isSHA256Hex(fields[1]) || !isSHA256Hex(fields[2]) {
			return nil, "", fmt.Errorf("recipe cache %s: bad hash on line %q", path, line)
		}
		hashes = append(hashes, FileHashes{Exact: fields[1], Masked: fields[2]})
		body.WriteString(line)
		body.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	if checksum == "" {
		return nil, "", fmt.Errorf("recipe cache %s: no checksum line", path)
	}
	return hashes, checksum, nil
}

// cacheChecksum hashes the cache key, SQLite options, every planned statement
// and bound argument, and all cached step hashes so any drift invalidates the
// on-disk cache.
func (r Recipe) cacheChecksum(hashes []FileHashes) (string, error) {
	if len(hashes) == 0 {
		return "", errors.New("recipe cache: no hashes")
	}
	plan, err := r.WithSteps(len(hashes) - 1).Plan()
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\n%s\n%s\n", r.cacheKey(), r.SQLiteOptions().File, r.SQLiteOptions().Conn)
	for _, s := range plan {
		for _, statement := range s.Statements {
			fmt.Fprintf(sum, "%d %q %#v\n", s.Number, statement.SQL, statement.Args)
		}
	}
	for k, h := range hashes {
		fmt.Fprintf(sum, "%d %s %s\n", k, h.Exact, h.Masked)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// readVerifiedCache parses path, checks its checksum against r, and rebuilds
// step 0 in a temporary directory to confirm entry 0 matches the live SQLite
// build and sqlitex open path.
func (r Recipe) readVerifiedCache(ctx context.Context, path string) ([]FileHashes, error) {
	hashes, checksum, err := readHashCache(path)
	if err != nil {
		return nil, err
	}
	want, err := r.cacheChecksum(hashes)
	if err != nil {
		return nil, err
	}
	if checksum != want {
		return nil, fmt.Errorf("recipe cache %s: checksum mismatch", path)
	}
	fresh, err := r.freshStepZeroHash(ctx, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if fresh != hashes[0] {
		return nil, fmt.Errorf("recipe cache %s: step 0 rebuilds to %s, the cache says %s", path, fresh.Exact, hashes[0].Exact)
	}
	return hashes, nil
}

// freshStepZeroHash builds step 0 in an isolated temporary directory under dir
// and returns its exact and masked hashes.
func (r Recipe) freshStepZeroHash(ctx context.Context, dir string) (FileHashes, error) {
	buildDir, err := os.MkdirTemp(dir, "verify-")
	if err != nil {
		return FileHashes{}, err
	}
	defer os.RemoveAll(buildDir)
	path := filepath.Join(buildDir, "step-0.db")
	if err := r.Build(ctx, path, 0); err != nil {
		return FileHashes{}, err
	}
	return HashFile(path)
}

func (r Recipe) writeHashCache(path string, hashes []FileHashes) error {
	checksum, err := r.cacheChecksum(hashes)
	if err != nil {
		return err
	}
	var b strings.Builder
	for k, h := range hashes {
		fmt.Fprintf(&b, "%d %s %s\n", k, h.Exact, h.Masked)
	}
	b.WriteString(cacheChecksumPrefix + checksum + "\n")
	// Write to a sibling temp file and rename so concurrent readers never see
	// a partially written cache file.
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	if _, err := temp.WriteString(b.String()); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		os.Remove(temp.Name())
		return err
	}
	return nil
}
