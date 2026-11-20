// walreplay: parse a SQLite -wal file and replay its committed frames onto a copy of the base
// database file, without using SQLite. Mirrors walDecodeFrame/walChecksumBytes in src/wal.c.
//
//	walreplay frames WALFILE            # print WAL header and one line per valid frame
//	walreplay apply BASE.db WALFILE OUT # copy BASE to OUT, apply committed frames, truncate
//
// Rules implemented (all from src/wal.c at 3.53.4):
//   - WAL header is 32 bytes: magic, version, page size, checkpoint seq, salt1, salt2, cksum1, cksum2.
//   - magic 0x377f0682 => checksums computed over little-endian u32 words, 0x377f0683 => big-endian.
//   - header checksum covers header bytes 0..23; it seeds the frame checksum chain.
//   - each frame = 24-byte header (pgno, commit dbsize, salt1, salt2, cksum1, cksum2) + page.
//     cumulative checksum over frame-header bytes 0..7 then the page data.
//   - a frame is valid only if pgno!=0, its salts equal the header salts and the cumulative
//     checksum matches; the first invalid frame ends the log.
//   - only frames up to and including the LAST valid commit frame (dbsize!=0) are applied;
//     later frames belong to an uncommitted / torn transaction.
//   - pages are written in frame order (later frames of the same page win) and the output
//     is truncated to commit-dbsize * pagesize.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

type frame struct {
	idx    int // 1-based frame number
	pgno   uint32
	commit uint32 // db size in pages if commit frame, else 0
	data   []byte
}

type walFile struct {
	magic, version, pageSize, ckptSeq, salt1, salt2 uint32
	frames                                        []frame // valid frames only
	lastCommit                                    int     // index into frames of last commit frame, -1 if none
	stopReason                                    string
}

func cksum(bigEndian bool, b []byte, s1, s2 uint32) (uint32, uint32) {
	for i := 0; i < len(b); i += 8 {
		var x, y uint32
		if bigEndian {
			x, y = binary.BigEndian.Uint32(b[i:]), binary.BigEndian.Uint32(b[i+4:])
		} else {
			x, y = binary.LittleEndian.Uint32(b[i:]), binary.LittleEndian.Uint32(b[i+4:])
		}
		s1 += x + s2
		s2 += y + s1
	}
	return s1, s2
}

func parse(path string) (*walFile, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(buf) < 32 {
		return nil, fmt.Errorf("wal too short (%d bytes)", len(buf))
	}
	be := binary.BigEndian
	w := &walFile{magic: be.Uint32(buf[0:]), version: be.Uint32(buf[4:]), pageSize: be.Uint32(buf[8:]),
		ckptSeq: be.Uint32(buf[12:]), salt1: be.Uint32(buf[16:]), salt2: be.Uint32(buf[20:]), lastCommit: -1}
	if w.magic&^1 != 0x377f0682 {
		return nil, fmt.Errorf("bad magic %#x", w.magic)
	}
	bigEnd := w.magic&1 == 1
	s1, s2 := cksum(bigEnd, buf[0:24], 0, 0)
	if s1 != be.Uint32(buf[24:]) || s2 != be.Uint32(buf[28:]) {
		return nil, fmt.Errorf("wal header checksum mismatch")
	}
	ps := int(w.pageSize)
	for off, i := 32, 1; ; off, i = off+24+ps, i+1 {
		if off+24+ps > len(buf) {
			w.stopReason = fmt.Sprintf("end of file after %d frames", i-1)
			break
		}
		h := buf[off : off+24]
		pg := buf[off+24 : off+24+ps]
		if be.Uint32(h[0:]) == 0 {
			w.stopReason = fmt.Sprintf("frame %d: pgno 0", i)
			break
		}
		if be.Uint32(h[8:]) != w.salt1 || be.Uint32(h[12:]) != w.salt2 {
			w.stopReason = fmt.Sprintf("frame %d: salt mismatch (stale frame from earlier WAL generation)", i)
			break
		}
		s1, s2 = cksum(bigEnd, h[0:8], s1, s2)
		s1, s2 = cksum(bigEnd, pg, s1, s2)
		if s1 != be.Uint32(h[16:]) || s2 != be.Uint32(h[20:]) {
			w.stopReason = fmt.Sprintf("frame %d: checksum mismatch", i)
			break
		}
		f := frame{idx: i, pgno: be.Uint32(h[0:]), commit: be.Uint32(h[4:]), data: pg}
		w.frames = append(w.frames, f)
		if f.commit != 0 {
			w.lastCommit = len(w.frames) - 1
		}
	}
	return w, nil
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: walreplay frames WAL | walreplay apply BASE WAL OUT")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "frames":
		w, err := parse(os.Args[2])
		must(err)
		fmt.Printf("magic=%#x version=%d pagesize=%d ckptseq=%d salt1=%#08x salt2=%#08x\n",
			w.magic, w.version, w.pageSize, w.ckptSeq, w.salt1, w.salt2)
		for _, f := range w.frames {
			fmt.Printf("frame %d pgno=%d commit=%d sha=%x\n", f.idx, f.pgno, f.commit, sha256.Sum256(f.data))
		}
		fmt.Printf("valid=%d lastcommit_frame=%d stop: %s\n", len(w.frames), w.lastCommit+1, w.stopReason)
	case "apply":
		w, err := parse(os.Args[3])
		must(err)
		in, err := os.Open(os.Args[2])
		must(err)
		out, err := os.Create(os.Args[4])
		must(err)
		_, err = io.Copy(out, in)
		must(err)
		in.Close()
		if w.lastCommit < 0 {
			fmt.Println("no committed frames; output = base copy")
			must(out.Close())
			return
		}
		ps := int64(w.pageSize)
		for _, f := range w.frames[:w.lastCommit+1] {
			_, err = out.WriteAt(f.data, int64(f.pgno-1)*ps)
			must(err)
		}
		dbsize := w.frames[w.lastCommit].commit
		must(out.Truncate(int64(dbsize) * ps))
		must(out.Close())
		fmt.Printf("applied %d frames (of %d valid), dbsize=%d pages; ignored %d frames after last commit; %s\n",
			w.lastCommit+1, len(w.frames), dbsize, len(w.frames)-w.lastCommit-1, w.stopReason)
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand")
		os.Exit(2)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
