package recipe

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/sqlite"
)

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

var ChangeCounterRanges = [][2]int{{24, 28}, {92, 96}}

func MaskChangeCounter(image []byte) []byte {
	masked := append([]byte(nil), image...)
	for _, span := range ChangeCounterRanges {
		for i := span[0]; i < span[1] && i < len(masked); i++ {
			masked[i] = 0
		}
	}
	return masked
}

type FileHashes struct {
	Exact  string
	Masked string
}

func HashFile(path string) (FileHashes, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return FileHashes{}, err
	}
	return HashImage(image), nil
}

func HashImage(image []byte) FileHashes {
	exact := sha256.Sum256(image)
	masked := sha256.Sum256(MaskChangeCounter(image))
	return FileHashes{Exact: hex.EncodeToString(exact[:]), Masked: hex.EncodeToString(masked[:])}
}

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
	return fmt.Sprintf("v%d-%s-seed%d-%s", FormatVersion, sqlite.Current().FossilID[:16], r.Seed, r.JournalMode)
}

func CacheDir() (string, error) {
	base, err := sqlitex.WorkBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "recipe-cache"), nil
}

func (r Recipe) CachePath() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, r.cacheKey()+".txt"), nil
}

func (r Recipe) StepHashes(ctx context.Context) ([]FileHashes, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	cachePath, err := r.CachePath()
	if err != nil {
		return nil, err
	}
	if cached, err := readHashCache(cachePath); err == nil && len(cached) > r.Steps {
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
	if err := writeHashCache(cachePath, hashes); err != nil {
		return nil, err
	}
	return hashes, nil
}

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

type Match struct {
	Step  int
	Exact bool
}

func (m Match) Found() bool {
	return m.Step >= 0
}

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
	return MatchHashes(hashes, h), nil
}

func isSHA256Hex(text string) bool {
	_, err := hex.DecodeString(text)
	return err == nil && len(text) == sha256.Size*2
}

func readHashCache(path string) ([]FileHashes, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var hashes []FileHashes
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return nil, fmt.Errorf("recipe cache %s: malformed line %q", path, scanner.Text())
		}
		k, err := strconv.Atoi(fields[0])
		if err != nil || k != len(hashes) {
			return nil, fmt.Errorf("recipe cache %s: line %q out of order", path, scanner.Text())
		}
		if !isSHA256Hex(fields[1]) || !isSHA256Hex(fields[2]) {
			return nil, fmt.Errorf("recipe cache %s: bad hash on line %q", path, scanner.Text())
		}
		hashes = append(hashes, FileHashes{Exact: fields[1], Masked: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return hashes, nil
}

func writeHashCache(path string, hashes []FileHashes) error {
	var b strings.Builder
	for k, h := range hashes {
		fmt.Fprintf(&b, "%d %s %s\n", k, h.Exact, h.Masked)
	}
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
	return os.Rename(temp.Name(), path)
}
