package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash/crc64"
	"os"
	"sort"
	"time"
)

// RED4-Archiv (RDAR, Version 12): Kopf 40 Byte, Daten ab 172, Index am Ende.

type seg struct {
	off         uint64
	zsize, size uint32
}
type entry struct {
	hash uint64
	segs []seg
}
type arch struct {
	path  string
	ents  []entry
	f     *os.File
	index map[uint64]int
}

func fnv64(s string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, c := range []byte(s) {
		h ^= uint64(c)
		h *= 0x100000001b3
	}
	return h
}

func loadArchive(p string) (*arch, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	h := make([]byte, 40)
	if _, err := f.ReadAt(h, 0); err != nil || string(h[:4]) != "RDAR" {
		f.Close()
		return nil, fmt.Errorf("not a RED4 archive")
	}
	ipos := binary.LittleEndian.Uint64(h[8:])
	isize := binary.LittleEndian.Uint32(h[16:])
	idx := make([]byte, isize)
	if _, err := f.ReadAt(idx, int64(ipos)); err != nil {
		f.Close()
		return nil, err
	}
	nfiles := int(binary.LittleEndian.Uint32(idx[16:]))
	nseg := int(binary.LittleEndian.Uint32(idx[20:]))
	t := idx[28:]
	a := &arch{path: p, f: f, index: map[uint64]int{}}
	type raw struct {
		h      uint64
		s0, s1 uint32
	}
	var raws []raw
	for i := 0; i < nfiles; i++ {
		e := t[i*56:]
		raws = append(raws, raw{binary.LittleEndian.Uint64(e), binary.LittleEndian.Uint32(e[20:]), binary.LittleEndian.Uint32(e[24:])})
	}
	segs := make([]seg, nseg)
	base := nfiles * 56
	for i := 0; i < nseg; i++ {
		e := t[base+i*16:]
		segs[i] = seg{binary.LittleEndian.Uint64(e), binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])}
	}
	for i, r := range raws {
		en := entry{hash: r.h}
		if r.s1 <= uint32(nseg) && r.s0 <= r.s1 {
			en.segs = segs[r.s0:r.s1]
		}
		a.ents = append(a.ents, en)
		a.index[r.h] = i
	}
	return a, nil
}

func (a *arch) Close() { a.f.Close() }

func (a *arch) read(e entry) ([]byte, error) {
	var out []byte
	for _, s := range e.segs {
		b := make([]byte, s.zsize)
		if _, err := a.f.ReadAt(b, int64(s.off)); err != nil {
			return nil, err
		}
		if s.zsize != s.size {
			if len(b) > 8 && string(b[:4]) == "KARK" {
				d, err := oodleDecompress(b[8:], int(binary.LittleEndian.Uint32(b[4:])))
				if err != nil {
					return nil, err
				}
				b = d
			} else {
				return nil, fmt.Errorf("unknown compression")
			}
		}
		out = append(out, b...)
	}
	return out, nil
}

type modFile struct {
	Hash uint64
	Data []byte
}

func rup(n, a int) int { return (n + a - 1) / a * a }

func filetime(t time.Time) uint64 {
	return uint64(t.UnixNano()/100) + 116444736000000000
}

// writeArchive schreibt ein unkomprimiertes Mod-Archiv (grosse Dateien werden in Segmente geteilt).
var segChunk = 0 // 0 = ein Segment pro Datei

func writeArchive(path string, files []modFile) error {
	sort.Slice(files, func(i, j int) bool { return files[i].Hash < files[j].Hash })
	var body bytes.Buffer
	body.Write(make([]byte, 172))
	type fe struct {
		hash   uint64
		s0, s1 int
	}
	var fes []fe
	var segs []seg
	for _, f := range files {
		n := len(f.Data)
		step := n
		if segChunk > 0 && n > segChunk {
			step = segChunk
		}
		// Wwise braucht Bank-Speicher ausgerichtet: Dateianfang auf 4096 Byte legen
		if pad := rup(body.Len(), 4096) - body.Len(); pad > 0 {
			body.Write(make([]byte, pad))
		}
		s0 := len(segs)
		for o := 0; o < n || (n == 0 && o == 0); o += step {
			e := o + step
			if e > n {
				e = n
			}
			segs = append(segs, seg{uint64(body.Len()), uint32(e - o), uint32(e - o)})
			body.Write(f.Data[o:e])
			if n == 0 {
				break
			}
		}
		fes = append(fes, fe{f.Hash, s0, len(segs)})
	}
	ipos := rup(body.Len(), 4096)
	body.Write(make([]byte, ipos-body.Len()))
	emptySHA := sha1.Sum(nil)
	var tb bytes.Buffer
	w32 := func(v uint32) { binary.Write(&tb, binary.LittleEndian, v) }
	w64 := func(v uint64) { binary.Write(&tb, binary.LittleEndian, v) }
	w32(uint32(len(fes)))
	w32(uint32(len(segs)))
	w32(0)
	ts := filetime(time.Now())
	for _, e := range fes {
		w64(e.hash)
		w64(ts)
		w32(0)
		w32(uint32(e.s0))
		w32(uint32(e.s1))
		w32(0)
		w32(0)
		tb.Write(emptySHA[:])
	}
	for _, s := range segs {
		w64(s.off)
		w32(s.zsize)
		w32(s.size)
	}
	crc := crc64.Checksum(tb.Bytes(), crc64.MakeTable(crc64.ECMA))
	var idx bytes.Buffer
	binary.Write(&idx, binary.LittleEndian, uint32(8))
	binary.Write(&idx, binary.LittleEndian, uint32(tb.Len()+8))
	binary.Write(&idx, binary.LittleEndian, crc)
	idx.Write(tb.Bytes())
	body.Write(idx.Bytes())
	total := rup(body.Len(), 4096)
	body.Write(make([]byte, total-body.Len()))
	out := body.Bytes()
	copy(out[0:4], "RDAR")
	binary.LittleEndian.PutUint32(out[4:], 12)
	binary.LittleEndian.PutUint64(out[8:], uint64(ipos))
	binary.LittleEndian.PutUint32(out[16:], uint32(idx.Len()))
	binary.LittleEndian.PutUint64(out[32:], uint64(total))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(tmp, path)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
