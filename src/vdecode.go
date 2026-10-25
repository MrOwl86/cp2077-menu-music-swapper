package main

// Pure-Go Dekoder fuer Wwise-Vorbis ("modded"/"stripped setup"): liest exakt die Pakete,
// die vencode.go/vwem.go erzeugen, in umgekehrter Richtung. Kein externes Hilfsprogramm,
// kein Prozessstart - nur Mathematik, damit die Vorhoer-Funktion ohne Antivirus-Risiko
// (siehe listen.go) wieder funktioniert.
//
// Die Bit-Reihenfolge und alle Formeln sind exakt an vencode.go/vsetup.go gespiegelt:
// Codebuecher, Floor1 und Residue werden hier in der Gegenrichtung gelesen statt
// geschrieben. Die inverse MDCT wurde unabhaengig herleitet und per Testprogramm
// (Rundlauf-Test: codieren -> dekodieren -> Vergleich mit Original) auf
// Maschinengenauigkeit bestaetigt, da ein Hoertest hier nicht moeglich ist.

import (
	"errors"
	"fmt"
)

// ---------- Codebuch-Decodierung (Huffman) ----------

// ensureDecodeTable baut (einmalig) eine Tabelle Laenge->Code->Eintrag fuer die
// Huffman-Dekodierung. Die Codes sind kanonisch (siehe makeCodes), daher reicht
// ein einfaches "ein Bit nach dem anderen lesen und nachschlagen".
func (b *codebook) ensureDecodeTable() {
	if b.dtbl != nil {
		return
	}
	b.dtbl = make(map[uint64]int32)
	for e, l := range b.lengths {
		if l <= 0 {
			continue
		}
		key := uint64(l)<<32 | uint64(b.codes[e])
		b.dtbl[key] = int32(e)
	}
}

var errBadStream = errors.New("ungueltiger oder unbekannter Audiostream")

// readCode liest ein Huffman-Codewort (MSB zuerst, siehe writeCode) und liefert den Eintrag.
func (b *codebook) readCode(r *bitReader) (int, error) {
	b.ensureDecodeTable()
	var code uint64
	for l := 1; l <= 32; l++ {
		code = code<<1 | uint64(r.read(1))
		if e, ok := b.dtbl[uint64(l)<<32|code]; ok {
			return int(e), nil
		}
	}
	return 0, errBadStream
}

// ---------- Floor1 Dekodierung (Spiegelbild von floor1.encode) ----------

// decode liest den Floor eines Kanals; liefert finalY/step2 wie encode, oder ok=false,
// wenn der Floor fuer diesen Block als "ungenutzt" markiert ist (Stille).
func (f *floor1) decode(r *bitReader, books []*codebook) (finalY []int, step2 []bool, ok bool, err error) {
	if r.read(1) == 0 {
		return nil, nil, false, nil
	}
	n := len(f.x)
	finalY = make([]int, n)
	step2 = make([]bool, n)
	rb := ilog(uint32(f.rng - 1))
	finalY[0] = int(r.read(rb))
	finalY[1] = int(r.read(rb))
	step2[0], step2[1] = true, true
	offset := 2
	for p := 0; p < f.partitions; p++ {
		cl := f.classes[f.pcl[p]]
		cdim, cbits := cl.dim, cl.sub
		csub := (1 << uint(cbits)) - 1
		cval := 0
		if cbits > 0 {
			mb := books[cl.master]
			e, err2 := mb.readCode(r)
			if err2 != nil {
				return nil, nil, false, err2
			}
			cval = e
		}
		subs := make([]int, cdim)
		for j := 0; j < cdim; j++ {
			subs[j] = (cval >> uint(cbits*j)) & csub
		}
		tmpY := make([]int, n)
		copy(tmpY, finalY)
		vals := make([]int, cdim)
		for j := 0; j < cdim; j++ {
			i := offset + j
			if i >= n {
				return nil, nil, false, fmt.Errorf("Floor: Index ausserhalb (%d/%d)", i, n)
			}
			pred := f.predict(i, tmpY)
			bk := cl.subbooks[subs[j]]
			if bk < 0 {
				tmpY[i] = pred
				continue
			}
			b := books[bk]
			e, err2 := b.readCode(r)
			if err2 != nil {
				return nil, nil, false, err2
			}
			vals[j] = e
			tmpY[i] = decodeFloorVal(e, pred, f.rng)
		}
		copy(finalY, tmpY)
		for j := 0; j < cdim; j++ {
			if vals[j] != 0 {
				i := offset + j
				step2[i] = true
				step2[f.low[i]] = true
				step2[f.high[i]] = true
			}
		}
		offset += cdim
	}
	return finalY, step2, true, nil
}

// ---------- Residue Dekodierung (Spiegelbild von residue.encode) ----------

// decode liest die Residuen fuer "vecs" (ein Eintrag pro logischem Kanal/Submap) und
// addiert sie direkt in die uebergebenen Zielvektoren (Laenge jeweils = begrenzte Spektrumsgroesse).
func (r *residue) decode(br *bitReader, books []*codebook, vecs [][]float32) error {
	if len(vecs) == 0 {
		return nil
	}
	limit := len(vecs[0])
	end := r.end
	if end > limit {
		end = limit
	}
	n := end - r.begin
	if n <= 0 {
		return nil
	}
	partvals := n / r.ps
	cbk := books[r.classbook]
	cdim := cbk.dim
	classes := make([][]int, len(vecs))
	for j := range vecs {
		classes[j] = make([]int, partvals)
	}
	for s := 0; s < r.stages; s++ {
		for i := 0; i < partvals; i += cdim {
			if s == 0 {
				for j := range vecs {
					e, err := cbk.readCode(br)
					if err != nil {
						return err
					}
					rem := e
					for k := cdim - 1; k >= 0; k-- {
						c := rem % r.nclass
						rem /= r.nclass
						if i+k < partvals {
							classes[j][i+k] = c
						}
					}
				}
			}
			for k := 0; k < cdim && i+k < partvals; k++ {
				off := r.begin + (i+k)*r.ps
				for j, v := range vecs {
					cl := classes[j][i+k]
					bk := r.books[cl][s]
					if bk < 0 {
						continue
					}
					b := books[bk]
					d := b.dim
					cnt := r.ps / d
					for c := 0; c < cnt; c++ {
						e, err := b.readCode(br)
						if err != nil {
							return err
						}
						if !b.hasLookup || b.vec[e] == nil {
							continue
						}
						vec := b.vec[e]
						base := off + c*d
						for kk := 0; kk < d && base+kk < len(v); kk++ {
							v[base+kk] += vec[kk]
						}
					}
				}
			}
		}
	}
	return nil
}

// ---------- Inverse Kanalkopplung (Spiegelbild der Kopplung in encodeBlock) ----------

func inverseCoupling(mp *mapping, res [][]float32, m int) {
	for _, st := range mp.coupling {
		mg, an := res[st[0]], res[st[1]]
		for k := 0; k < m; k++ {
			mag, ang := mg[k], an[k]
			var a, b float32
			if mag > 0 {
				if ang > 0 {
					a, b = mag, mag-ang
				} else {
					b, a = mag, mag+ang
				}
			} else {
				if ang > 0 {
					a, b = mag, mag+ang
				} else {
					b, a = mag, mag-ang
				}
			}
			mg[k], an[k] = a, b
		}
	}
}

// ---------- Inverse MDCT ----------
// Siehe Herleitung/Verifikation in der Sitzung: u = dct4(X); y wird durch Transponieren
// der Faltung aus vencode.go's forward() zurueckgewonnen; anschliessend Fenster
// multiplizieren und per Overlap-Add zusammenfuegen. Numerisch auf Maschinengenauigkeit
// gegen einen Rundlauf-Test (codieren->dekodieren) verifiziert.
func (t *mdct) inverse(x []float64, out []float64) {
	m := t.m
	h := m / 2
	u := t.u // wiederverwenden als Zwischenpuffer (Laenge m)
	t.dct4(x, u)
	for i := range out[:t.n] {
		out[i] = 0
	}
	for i := 0; i < h; i++ {
		out[3*h-1-i] = -u[i]
		out[3*h+i] = -u[i]
		out[i] = u[h+i]
		out[m-1-i] = -u[h+i]
	}
}

// ---------- Dekoder ----------

// modeInfo: pro Modus (kurz/lang) die zugehoerige Abbildung und Blockgroesse.
type modeInfo struct {
	mp       *mapping
	n, m     int
	floorFor []int
	submapOf []int
}

type vdecoder struct {
	s        *vsetup
	modes    []modeInfo // indiziert wie s.modeFlag/s.modeMap
	bs0, bs1 int
	std      bool // Standard-Vorbis-Pakete (Paketkopf mit Packet-Type-Bit) statt Wwise-Modified
}

func buildModeInfo(s *vsetup, n int) modeInfo {
	mi := modeInfo{n: n, m: n / 2}
	return mi
}

func newDecoder(s *vsetup, bs0, bs1 int, std bool) (*vdecoder, error) {
	if len(s.modeFlag) == 0 {
		return nil, errors.New("Setup enthaelt keine Modi")
	}
	d := &vdecoder{s: s, std: std, bs0: bs0, bs1: bs1}
	d.modes = make([]modeInfo, len(s.modeFlag))
	for i, long := range s.modeFlag {
		n := 1 << uint(bs0)
		if long {
			n = 1 << uint(bs1)
		}
		mi := buildModeInfo(s, n)
		mi.mp = s.mappings[s.modeMap[i]]
		mi.floorFor = make([]int, s.channels)
		mi.submapOf = make([]int, s.channels)
		for c := 0; c < s.channels; c++ {
			sm := mi.mp.mux[c]
			mi.submapOf[c] = sm
			mi.floorFor[c] = mi.mp.floorNum[sm]
		}
		d.modes[i] = mi
	}
	return d, nil
}

// peekMode liest nur die Modus-Nummer eines Pakets (fuer die Fenster-Uebergangslogik),
// ohne das Paket vollstaendig zu dekodieren.
func (d *vdecoder) peekMode(pkt []byte) (modeNum int, long bool, ok bool) {
	if len(pkt) == 0 {
		return 0, false, false
	}
	r := &bitReader{b: pkt}
	if d.std {
		r.read(1)
	}
	modeNum = int(r.read(d.s.modeBits))
	if modeNum < 0 || modeNum >= len(d.s.modeFlag) {
		return 0, false, false
	}
	return modeNum, d.s.modeFlag[modeNum], true
}

// decodeBlock dekodiert ein Audiopaket vollstaendig (Floor, Residue, inverse Kopplung,
// inverse MDCT) und liefert das ungefensterte Zeitbereichssignal (Laenge = Blockgroesse
// dieses Modus) sowie dessen Blockgroesse. prevLong/nextLong geben an, ob der jeweils
// benachbarte Block (in Dekodierreihenfolge) ein langer Block ist/war - das steuert bei
// langen Bloecken die Fenster-Uebergaenge an den Raendern (Vorbis-Spezifikation 4.3.3).
func (d *vdecoder) decodeBlock(pkt []byte, tmShort, tmLong *mdct, prevLong, nextLong bool) (pcm [][]float64, n int, err error) {
	s := d.s
	C := s.channels
	r := &bitReader{b: pkt}
	if d.std {
		if r.read(1) != 0 {
			return nil, 0, errBadStream
		}
	}
	modeNum := int(r.read(s.modeBits))
	if modeNum < 0 || modeNum >= len(s.modeFlag) {
		return nil, 0, errBadStream
	}
	mi := d.modes[modeNum]
	long := s.modeFlag[modeNum]
	tm := tmShort
	if long {
		tm = tmLong
	}
	if d.std && long {
		r.read(1)
		r.read(1)
	}
	m := mi.m
	spec := make([][]float32, C)
	for c := 0; c < C; c++ {
		spec[c] = make([]float32, m)
	}
	// Floors
	curves := make([][]float64, C)
	floorOK := make([]bool, C)
	for c := 0; c < C; c++ {
		f := s.floors[mi.floorFor[c]]
		fy, st, ok2, err2 := f.decode(r, s.books)
		if err2 != nil {
			return nil, 0, err2
		}
		floorOK[c] = ok2
		curves[c] = make([]float64, m)
		if ok2 {
			f.curve(fy, st, curves[c])
		}
	}
	// Residue je Submap - Teilbaender ohne jede gueltige Floor werden gar nicht erst
	// gelesen (Vorbis "do not decode"-Flag, Spezifikation 6.2), sonst wuerde man bei
	// unseren eigenen Platzhalter-Kurzpaketen (siehe silentShortPacket) ueber das
	// Paketende hinaus lesen.
	for sm := 0; sm < mi.mp.submaps; sm++ {
		rn := mi.mp.resNum[sm]
		rr := s.residues[rn]
		var chans []int
		anyUsed := false
		for c := 0; c < C; c++ {
			if mi.submapOf[c] == sm {
				chans = append(chans, c)
				if floorOK[c] {
					anyUsed = true
				}
			}
		}
		if !anyUsed {
			continue
		}
		if rr.typ == 2 {
			iv := make([]float32, m*len(chans))
			if err2 := rr.decode(r, s.books, [][]float32{iv}); err2 != nil {
				return nil, 0, err2
			}
			for k := 0; k < m; k++ {
				for ci, c := range chans {
					spec[c][k] = iv[k*len(chans)+ci]
				}
			}
		} else {
			var vecs [][]float32
			for _, c := range chans {
				vecs = append(vecs, spec[c])
			}
			if err2 := rr.decode(r, s.books, vecs); err2 != nil {
				return nil, 0, err2
			}
		}
	}
	// Inverse Kopplung
	inverseCoupling(mi.mp, spec, m)
	// Floorkurve anwenden (nur Kanaele mit gueltigem Floor - sonst Stille)
	fspec := make([][]float64, C)
	for c := 0; c < C; c++ {
		fspec[c] = make([]float64, m)
		if floorOK[c] {
			for k := 0; k < m; k++ {
				fspec[c][k] = float64(spec[c][k]) * curves[c][k]
			}
		}
	}
	// Inverse MDCT + Fenster-Uebergang (Vorbis-Spezifikation 4.3.3): bei einem langen
	// Block haengt die Fensterform an den Raendern davon ab, ob der Nachbarblock ebenfalls
	// lang ist (voller Ueberlapp) oder kurz (nur Blockgroesse/4-Ueberlapp, Rest Null).
	prevSize, nextSize := mi.n, mi.n
	if long {
		if !prevLong {
			prevSize = tmShort.n
		}
		if !nextLong {
			nextSize = tmShort.n
		}
	}
	out := make([][]float64, C)
	for c := 0; c < C; c++ {
		raw := make([]float64, mi.n)
		tm.inverse(fspec[c], raw)
		applyTransitionWindow(raw, mi.n, prevSize, nextSize, tmLong.win, tmShort.win)
		out[c] = raw
	}
	return out, mi.n, nil
}

// applyTransitionWindow wendet das Fenster an - mit verkuerztem Ueberlapp an den Raendern,
// wenn der Nachbarblock kuerzer ist (sonst Standardfenster ueber die volle Blockhaelfte).
// Portiert nach der Logik aus jfreymuth/vorbis (window.go, MIT-Lizenz), angepasst auf
// unsere eigenen (identischen) Fensterarrays.
func applyTransitionWindow(samples []float64, size, prevSize, nextSize int, longWin, shortWin []float64) {
	center := size / 2
	prevOffset := size/4 - prevSize/4
	nextOffset := size/4 - nextSize/4
	winFor := func(sz int) []float64 {
		if sz == len(longWin) {
			return longWin
		}
		return shortWin
	}
	pw := winFor(prevSize)
	nw := winFor(nextSize)
	for i := 0; i < prevOffset && i < len(samples); i++ {
		samples[i] = 0
	}
	for i := 0; i < prevSize/2; i++ {
		idx := prevOffset + i
		if idx >= 0 && idx < len(samples) && i < len(pw) {
			samples[idx] *= pw[i]
		}
	}
	for i := 0; i < nextSize/2; i++ {
		idx := center + nextOffset + i
		wIdx := nextSize/2 + i
		if idx >= 0 && idx < len(samples) && wIdx < len(nw) {
			samples[idx] *= nw[wIdx]
		}
	}
	for i := size - nextOffset; i < size && i < len(samples); i++ {
		if i >= 0 {
			samples[i] = 0
		}
	}
}

// ---------- Streaming-Overlap (exakt nach jfreymuth/vorbis decode.go portiert, MIT-Lizenz) ----------
//
// Ein naiver "globaler Akkumulator mit fester Hop-Groesse" scheitert an Uebergaengen
// zwischen kurzen und langen Bloecken, weil die Ueberlapp-Breite dort von BEIDEN
// benachbarten Blockgroessen abhaengt (siehe Entwicklungsnotiz: fester 320-Sample-Versatz
// bei einem Wechsel lang->kurz->lang). Diese Zustandsmaschine haelt deshalb - genau wie
// ein echter Streaming-Dekoder - einen Ueberlapp-Puffer fester Groesse (=lange Blockgroesse)
// und haengt bei jedem Block nur die tatsaechlich fertigen, bereits ueberlapp-addierten
// neuen Samples an die Ausgabe an.
type overlapState struct {
	buf        []float64 // Laenge = lange Blockgroesse (n1)
	hasOverlap bool
	short      bool // true, wenn der zuletzt verarbeitete Block kurz war
}

func newOverlapStates(channels, n1 int) []*overlapState {
	st := make([]*overlapState, channels)
	for c := range st {
		st[c] = &overlapState{buf: make([]float64, n1)}
	}
	return st
}

// feed verarbeitet einen (bereits gefensterten, ungefensterten raw-)Block und liefert die
// neu fertigen Ausgabesamples (kann leer sein). windowPrev/windowNext entsprechen den
// gleichnamigen Vorbis-Flags (hier aus prevLong/nextLong abgeleitet).
func (st *overlapState) feed(raw []float64, long bool, windowPrev bool, n1, n0 int) []float64 {
	size := len(raw)
	center := size / 2
	offset := n1/4 - n0/4
	n := 0
	if st.hasOverlap {
		n = center
		if long && !windowPrev {
			n -= offset
		}
		if !long && !st.short {
			n += offset
		}
	}
	out := make([]float64, n)
	if long {
		start := 0
		if !windowPrev {
			start = offset
		}
		if st.hasOverlap {
			for i := 0; i < center-start && i < n; i++ {
				out[i] = raw[start+i] + st.buf[start+i]
			}
		}
		st.short = false
	} else {
		if st.hasOverlap {
			if st.short {
				for i := 0; i < center && i < n; i++ {
					out[i] = raw[i] + st.buf[offset+i]
				}
			} else {
				for i := 0; i < offset && i < n; i++ {
					out[i] = st.buf[i]
				}
				for i := offset; i < offset+center && i < n; i++ {
					out[i] = raw[i-offset] + st.buf[i]
				}
			}
		}
		st.short = true
	}
	if !st.hasOverlap {
		n = 0
		out = out[:0]
	}
	overlapCenter := n1 / 4
	oStart := overlapCenter - center/2
	oEnd := overlapCenter + center/2
	for i := 0; i < oStart && i < len(st.buf); i++ {
		st.buf[i] = 0
	}
	for i := oStart; i < oEnd && i < len(st.buf); i++ {
		st.buf[i] = raw[center+i-oStart]
	}
	for i := oEnd; i < len(st.buf); i++ {
		st.buf[i] = 0
	}
	st.hasOverlap = true
	return out
}

// decodeWemToPCM dekodiert alle Audiopakete einer Wwise-Vorbis-WEM zu PCM (float32, interleaved).
func decodeWemToPCM(raw []byte) (*PCM, error) {
	t, err := parseWemTemplate(raw)
	if err != nil {
		return nil, err
	}
	dec, err := newDecoder(t.setup, t.bs0, t.bs1, !t.modPackets)
	if err != nil {
		return nil, err
	}
	C := t.channels
	tmLong := newMDCT(1 << uint(t.bs1))
	var tmShort *mdct
	if t.bs0 == t.bs1 {
		tmShort = tmLong
	} else {
		tmShort = newMDCT(1 << uint(t.bs0))
	}

	// Audiopakete einlesen ([uint16 Laenge][Daten], direkt nach dem Setup-Paket).
	var packets [][]byte
	pos := t.dataOff + t.firstAudio
	for pos+2 <= t.dataOff+t.dataSize {
		sz := int(leU16(raw, pos))
		pos += 2
		if pos+sz > len(raw) {
			break
		}
		packets = append(packets, raw[pos:pos+sz])
		pos += sz
	}
	if len(packets) == 0 {
		return nil, errors.New("keine Audiopakete gefunden")
	}

	n1 := 1 << uint(t.bs1)
	n0 := 1 << uint(t.bs0)
	states := newOverlapStates(C, n1)
	chans := make([][]float64, C)
	for c := range chans {
		chans[c] = make([]float64, 0, t.samples+n1)
	}
	for i, pkt := range packets {
		_, curLong, _ := dec.peekMode(pkt)
		prevLong := true
		if i > 0 {
			_, prevLong, _ = dec.peekMode(packets[i-1])
		}
		nextLong := true
		if i+1 < len(packets) {
			_, nextLong, _ = dec.peekMode(packets[i+1])
		}
		out, _, err := dec.decodeBlock(pkt, tmShort, tmLong, prevLong, nextLong)
		if err != nil {
			return nil, fmt.Errorf("Dekodierfehler bei Paket %d: %w", i, err)
		}
		for c := 0; c < C; c++ {
			emitted := states[c].feed(out[c], curLong, prevLong, n1, n0)
			chans[c] = append(chans[c], emitted...)
		}
	}
	n := t.samples
	if n <= 0 || n > len(chans[0]) {
		n = len(chans[0])
	}
	data := make([]float32, n*C)
	for i := 0; i < n; i++ {
		for c := 0; c < C; c++ {
			data[i*C+c] = float32(chans[c][i])
		}
	}
	return &PCM{Rate: t.rate, Ch: C, Data: data}, nil
}

func leU16(b []byte, off int) uint16 {
	return uint16(b[off]) | uint16(b[off+1])<<8
}
