package main

// Parser fuer Wwise-Vorbis ("stripped setup") - Aufbau nach ww2ogg (BSD, Adam Gashlin / Xiph.org).

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

//go:embed pcb_aotuv603.bin
var packedCodebooks []byte

// ---------- Bit-Ein/Ausgabe (Vorbis: LSB zuerst) ----------

type bitReader struct {
	b   []byte
	pos int
}

func (r *bitReader) read(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		byteIdx := r.pos >> 3
		var bit uint32
		if byteIdx < len(r.b) {
			bit = uint32(r.b[byteIdx]>>(uint(r.pos)&7)) & 1
		}
		v |= bit << uint(i)
		r.pos++
	}
	return v
}

type bitWriter struct {
	buf  []byte
	acc  uint64
	nacc uint
}

func (w *bitWriter) write(v uint32, n int) {
	w.acc |= uint64(v) & ((1 << uint(n)) - 1) << w.nacc
	w.nacc += uint(n)
	for w.nacc >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.nacc -= 8
	}
}

// writeCode schreibt ein Huffman-Codewort MSB zuerst.
func (w *bitWriter) writeCode(code uint32, length int) {
	for i := length - 1; i >= 0; i-- {
		w.write((code>>uint(i))&1, 1)
	}
}

func (w *bitWriter) bytes() []byte {
	out := w.buf
	if w.nacc > 0 {
		out = append(out, byte(w.acc))
	}
	return out
}

func (w *bitWriter) bitLen() int { return len(w.buf)*8 + int(w.nacc) }

func ilog(v uint32) int {
	r := 0
	for v != 0 {
		r++
		v >>= 1
	}
	return r
}

// ---------- Codebuecher ----------

type codebook struct {
	dim, entries int
	lengths      []int
	codes        []uint32
	hasLookup    bool
	vec          [][]float32 // pro Eintrag (nil wenn ungenutzt)
	used         []int
	dtbl         map[uint64]int32 // Dekodier-Tabelle (lazy, siehe vdecode.go)
}

func quantvals(entries, dim int) int {
	bits := ilog(uint32(entries))
	vals := entries >> uint((bits-1)*(dim-1)/dim)
	for {
		acc, acc1 := 1, 1
		for i := 0; i < dim; i++ {
			acc *= vals
			acc1 *= vals + 1
		}
		if acc <= entries && acc1 > entries {
			return vals
		}
		if acc > entries {
			vals--
		} else {
			vals++
		}
	}
}

func float32Unpack(v uint32) float64 {
	m := float64(v & 0x1fffff)
	s := v & 0x80000000
	e := int((v & 0x7fe00000) >> 21)
	if s != 0 {
		m = -m
	}
	return m * math.Ldexp(1, e-788)
}

func libraryBook(id int) ([]byte, error) {
	d := packedCodebooks
	if len(d) < 8 {
		return nil, errors.New("Codebuch-Bibliothek fehlt")
	}
	oo := int(binary.LittleEndian.Uint32(d[len(d)-4:]))
	n := (len(d) - oo) / 4
	if id < 0 || id >= n {
		return nil, fmt.Errorf("Codebuch-ID %d unbekannt", id)
	}
	start := int(binary.LittleEndian.Uint32(d[oo+4*id:]))
	end := oo
	if id+1 < n {
		end = int(binary.LittleEndian.Uint32(d[oo+4*(id+1):]))
	}
	return d[start:end], nil
}

func parseBook(b []byte) (*codebook, error) {
	br := &bitReader{b: b}
	cb := &codebook{}
	cb.dim = int(br.read(4))
	cb.entries = int(br.read(14))
	if cb.dim == 0 {
		return nil, errors.New("Codebuch mit 0 Dimensionen")
	}
	cb.lengths = make([]int, cb.entries)
	if br.read(1) == 1 { // ordered
		cur := 0
		l := int(br.read(5)) + 1
		for cur < cb.entries {
			c := int(br.read(ilog(uint32(cb.entries - cur))))
			for i := cur; i < cur+c && i < cb.entries; i++ {
				cb.lengths[i] = l
			}
			cur += c
			l++
		}
	} else {
		cll := int(br.read(3))
		sparse := br.read(1) == 1
		for i := 0; i < cb.entries; i++ {
			if sparse && br.read(1) == 0 {
				continue
			}
			cb.lengths[i] = int(br.read(cll)) + 1
		}
	}
	for i, l := range cb.lengths {
		if l > 0 {
			cb.used = append(cb.used, i)
		}
	}
	cb.codes = makeCodes(cb.lengths)
	if br.read(1) == 1 {
		mn := float32Unpack(br.read(32))
		dl := float32Unpack(br.read(32))
		vl := int(br.read(4)) + 1
		seq := br.read(1) == 1
		q := quantvals(cb.entries, cb.dim)
		mult := make([]float64, q)
		for i := range mult {
			mult[i] = float64(br.read(vl))
		}
		cb.hasLookup = true
		cb.vec = make([][]float32, cb.entries)
		for _, e := range cb.used {
			v := make([]float32, cb.dim)
			last := 0.0
			idiv := 1
			for i := 0; i < cb.dim; i++ {
				mo := (e / idiv) % q
				val := mult[mo]*dl + mn + last
				if seq {
					last = val
				}
				v[i] = float32(val)
				idiv *= q
			}
			cb.vec[e] = v
		}
	}
	return cb, nil
}

func makeCodes(lengths []int) []uint32 {
	var marker [33]uint32
	codes := make([]uint32, len(lengths))
	for i, l := range lengths {
		if l <= 0 {
			continue
		}
		entry := marker[l]
		codes[i] = entry
		for j := l; j > 0; j-- {
			if marker[j]&1 != 0 {
				if j == 1 {
					marker[1]++
				} else {
					marker[j] = marker[j-1] << 1
				}
				break
			}
			marker[j]++
		}
		for j := l + 1; j < 33; j++ {
			if marker[j]>>1 == entry {
				entry = marker[j]
				marker[j] = marker[j-1] << 1
			} else {
				break
			}
		}
	}
	return codes
}

// ---------- Setup-Struktur ----------

type floorClass struct {
	dim, sub, master int
	subbooks         []int
}

type floor1 struct {
	partitions int
	pcl        []int
	classes    []floorClass
	mult       int
	rangebits  int
	rng        int
	x          []int
	order      []int // Indizes nach X sortiert
	low, high  []int
}

type residue struct {
	typ, begin, end, ps, nclass, classbook int
	casc                                   []int
	books                                  [][8]int
	stages                                 int
}

type mapping struct {
	submaps  int
	coupling [][2]int
	mux      []int
	floorNum []int
	resNum   []int
}

type vsetup struct {
	channels int
	books    []*codebook
	floors   []*floor1
	residues []*residue
	mappings []*mapping
	modeFlag []bool
	modeMap  []int
	modeBits int
}

func parseSetup(data []byte, channels int) (*vsetup, error) {
	br := &bitReader{b: data}
	s := &vsetup{channels: channels}
	nb := int(br.read(8)) + 1
	for i := 0; i < nb; i++ {
		id := int(br.read(10))
		raw, err := libraryBook(id)
		if err != nil {
			return nil, err
		}
		cb, err := parseBook(raw)
		if err != nil {
			return nil, err
		}
		s.books = append(s.books, cb)
	}
	nf := int(br.read(6)) + 1
	for i := 0; i < nf; i++ {
		f := &floor1{}
		f.partitions = int(br.read(5))
		maxc := -1
		for j := 0; j < f.partitions; j++ {
			c := int(br.read(4))
			f.pcl = append(f.pcl, c)
			if c > maxc {
				maxc = c
			}
		}
		for j := 0; j <= maxc; j++ {
			fc := floorClass{master: -1}
			fc.dim = int(br.read(3)) + 1
			fc.sub = int(br.read(2))
			if fc.sub != 0 {
				fc.master = int(br.read(8))
			}
			for k := 0; k < 1<<uint(fc.sub); k++ {
				fc.subbooks = append(fc.subbooks, int(br.read(8))-1)
			}
			f.classes = append(f.classes, fc)
		}
		f.mult = int(br.read(2)) + 1
		f.rangebits = int(br.read(4))
		f.rng = []int{256, 128, 86, 64}[f.mult-1]
		f.x = []int{0, 1 << uint(f.rangebits)}
		for j := 0; j < f.partitions; j++ {
			for k := 0; k < f.classes[f.pcl[j]].dim; k++ {
				f.x = append(f.x, int(br.read(f.rangebits)))
			}
		}
		f.prepare()
		s.floors = append(s.floors, f)
	}
	nr := int(br.read(6)) + 1
	for i := 0; i < nr; i++ {
		r := &residue{}
		r.typ = int(br.read(2))
		r.begin = int(br.read(24))
		r.end = int(br.read(24))
		r.ps = int(br.read(24)) + 1
		r.nclass = int(br.read(6)) + 1
		r.classbook = int(br.read(8))
		for j := 0; j < r.nclass; j++ {
			lo := int(br.read(3))
			hi := 0
			if br.read(1) == 1 {
				hi = int(br.read(5))
			}
			r.casc = append(r.casc, hi*8+lo)
		}
		r.books = make([][8]int, r.nclass)
		for j := 0; j < r.nclass; j++ {
			for k := 0; k < 8; k++ {
				r.books[j][k] = -1
				if r.casc[j]&(1<<uint(k)) != 0 {
					r.books[j][k] = int(br.read(8))
					if k+1 > r.stages {
						r.stages = k + 1
					}
				}
			}
		}
		s.residues = append(s.residues, r)
	}
	nm := int(br.read(6)) + 1
	for i := 0; i < nm; i++ {
		m := &mapping{submaps: 1}
		if br.read(1) == 1 {
			m.submaps = int(br.read(4)) + 1
		}
		if br.read(1) == 1 {
			cs := int(br.read(8)) + 1
			for j := 0; j < cs; j++ {
				mg := int(br.read(ilog(uint32(channels - 1))))
				an := int(br.read(ilog(uint32(channels - 1))))
				m.coupling = append(m.coupling, [2]int{mg, an})
			}
		}
		if br.read(2) != 0 {
			return nil, errors.New("Mapping-Reserved-Feld ungleich 0")
		}
		m.mux = make([]int, channels)
		if m.submaps > 1 {
			for j := 0; j < channels; j++ {
				m.mux[j] = int(br.read(4))
			}
		}
		for j := 0; j < m.submaps; j++ {
			br.read(8)
			m.floorNum = append(m.floorNum, int(br.read(8)))
			m.resNum = append(m.resNum, int(br.read(8)))
		}
		s.mappings = append(s.mappings, m)
	}
	nmo := int(br.read(6)) + 1
	for i := 0; i < nmo; i++ {
		s.modeFlag = append(s.modeFlag, br.read(1) == 1)
		s.modeMap = append(s.modeMap, int(br.read(8)))
	}
	s.modeBits = ilog(uint32(nmo - 1))
	if (br.pos+7)/8 != len(data) {
		return nil, fmt.Errorf("Setup nicht vollstaendig gelesen (%d von %d Bytes)", (br.pos+7)/8, len(data))
	}
	return s, nil
}

func (f *floor1) prepare() {
	n := len(f.x)
	f.order = make([]int, n)
	for i := range f.order {
		f.order[i] = i
	}
	// einfache Sortierung nach X
	for i := 1; i < n; i++ {
		for j := i; j > 0 && f.x[f.order[j]] < f.x[f.order[j-1]]; j-- {
			f.order[j], f.order[j-1] = f.order[j-1], f.order[j]
		}
	}
	f.low = make([]int, n)
	f.high = make([]int, n)
	for i := 2; i < n; i++ {
		lo, hi := 0, 1
		for j := 0; j < i; j++ {
			if f.x[j] < f.x[i] && f.x[j] > f.x[lo] {
				lo = j
			}
			if f.x[j] > f.x[i] && f.x[j] < f.x[hi] {
				hi = j
			}
		}
		f.low[i], f.high[i] = lo, hi
	}
}
