package equal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

type Level int

const (
	E1Bytes Level = 1
)

func (l Level) String() string {
	switch l {
	case E1Bytes:
		return "E1 bytes"
	}
	return fmt.Sprintf("E%d", int(l))
}

type Result struct {
	Level              Level
	Equal              bool
	SHA256A            string
	SHA256B            string
	SizeA              int64
	SizeB              int64
	PageSize           int
	FirstDifferingPage int
	DifferingPages     int
}

func (r Result) String() string {
	if r.Equal {
		return fmt.Sprintf("%s: equal (sha256 %s)", r.Level, r.SHA256A)
	}
	text := fmt.Sprintf("%s: differ", r.Level)
	if r.FirstDifferingPage > 0 {
		text += fmt.Sprintf(", first differing page %d, %d page(s) differ", r.FirstDifferingPage, r.DifferingPages)
	}
	if r.SizeA != r.SizeB {
		text += fmt.Sprintf(", sizes %d and %d bytes", r.SizeA, r.SizeB)
	}
	return text
}

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ReaderSHA256(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum, _, err := ReaderSHA256(f)
	return sum, err
}

func CompareImages(a, b []byte, pageSize int) Result {
	r := Result{Level: E1Bytes, SHA256A: SHA256Hex(a), SHA256B: SHA256Hex(b), SizeA: int64(len(a)), SizeB: int64(len(b)), PageSize: pageSize}
	r.Equal = r.SHA256A == r.SHA256B
	if r.Equal || pageSize <= 0 {
		return r
	}
	pages := (max(len(a), len(b)) + pageSize - 1) / pageSize
	for p := range pages {
		if !bytes.Equal(pageOf(a, p, pageSize), pageOf(b, p, pageSize)) {
			if r.FirstDifferingPage == 0 {
				r.FirstDifferingPage = p + 1
			}
			r.DifferingPages++
		}
	}
	return r
}

func pageOf(image []byte, index, pageSize int) []byte {
	start := index * pageSize
	if start >= len(image) {
		return nil
	}
	return image[start:min(start+pageSize, len(image))]
}

func CompareFiles(a, b string, pageSize int) (Result, error) {
	imageA, err := os.ReadFile(a)
	if err != nil {
		return Result{}, err
	}
	imageB, err := os.ReadFile(b)
	if err != nil {
		return Result{}, err
	}
	return CompareImages(imageA, imageB, pageSize), nil
}
