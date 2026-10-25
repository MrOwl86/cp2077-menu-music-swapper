package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

type wemChunk struct {
	id        string
	off, size int // Nutzdaten
}

type wemTemplate struct {
	raw         []byte
	chunks      []wemChunk
	fmtOff      int
	dataOff     int
	dataSize    int
	channels    int
	rate        int
	samples     int
	modPackets  bool
	setupOff    int
	firstAudio  int
	bs0, bs1    int
	setupPacket []byte // inkl. 2-Byte-Laenge
	setup       *vsetup
}

func parseWemTemplate(b []byte) (*wemTemplate, error) {
	if len(b) < 64 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("keine WEM")
	}
	t := &wemTemplate{raw: b}
	pos := 12
	riffEnd := int(binary.LittleEndian.Uint32(b[4:])) + 8
	if riffEnd > len(b) {
		riffEnd = len(b)
	}
	t.fmtOff, t.dataOff = -1, -1
	for pos+8 <= riffEnd {
		id := string(b[pos : pos+4])
		sz := int(binary.LittleEndian.Uint32(b[pos+4:]))
		if pos+8+sz > len(b) {
			return nil, errors.New("WEM-Chunk abgeschnitten")
		}
		t.chunks = append(t.chunks, wemChunk{id, pos + 8, sz})
		switch id {
		case "fmt ":
			t.fmtOff = pos + 8
		case "data":
			t.dataOff, t.dataSize = pos+8, sz
		}
		pos += 8 + sz
	}
	if t.fmtOff < 0 || t.dataOff < 0 {
		return nil, errors.New("fmt/data fehlt")
	}
	var fmtSize int
	for _, c := range t.chunks {
		if c.id == "fmt " {
			fmtSize = c.size
		}
	}
	if binary.LittleEndian.Uint16(b[t.fmtOff:]) != 0xFFFF || fmtSize != 0x42 {
		return nil, errors.New("Vorlage ist kein Wwise-Vorbis-Format (fmt 0x42)")
	}
	t.channels = int(binary.LittleEndian.Uint16(b[t.fmtOff+2:]))
	t.rate = int(binary.LittleEndian.Uint32(b[t.fmtOff+4:]))
	v := t.fmtOff + 0x18
	t.samples = int(binary.LittleEndian.Uint32(b[v:]))
	ms := binary.LittleEndian.Uint32(b[v+4:])
	t.modPackets = !(ms == 0x4A || ms == 0x4B || ms == 0x69 || ms == 0x70)
	t.setupOff = int(binary.LittleEndian.Uint32(b[v+0x10:]))
	t.firstAudio = int(binary.LittleEndian.Uint32(b[v+0x14:]))
	t.bs0, t.bs1 = int(b[v+0x28]), int(b[v+0x29])
	if t.setupOff <= 0 || t.firstAudio <= t.setupOff || t.firstAudio > t.dataSize {
		return nil, errors.New("ungueltige Offsets")
	}
	sp := t.dataOff + t.setupOff
	ssz := int(binary.LittleEndian.Uint16(b[sp:]))
	if sp+2+ssz != t.dataOff+t.firstAudio {
		return nil, errors.New("Setup-Paket passt nicht zum ersten Audiopaket")
	}
	t.setupPacket = b[sp : sp+2+ssz]
	var err error
	t.setup, err = parseSetup(b[sp+2:sp+2+ssz], t.channels)
	if err != nil {
		return nil, err
	}
	if t.bs1 < 8 || t.bs1 > 13 {
		return nil, errors.New("seltsame Blockgroesse")
	}
	return t, nil
}

// encodeWem erzeugt eine Wwise-Vorbis-WEM aus PCM (ein Slice pro Kanal, auf Vorlagenrate).
func encodeWem(t *wemTemplate, pcm [][]float64, maxBytes int64, log func(string)) ([]byte, string, error) {
	if len(pcm) != t.channels {
		return nil, "", fmt.Errorf("Kanalzahl passt nicht (%d/%d)", len(pcm), t.channels)
	}
	enc, err := newEncoder(t.setup, t.bs1, pcm)
	if err != nil {
		return nil, "", err
	}
	enc.std = !t.modPackets
	hop := enc.m
	frames := enc.frames
	// Kopf-Overhead (RIFF, fmt, evtl. JUNK/akd, data-Header) + Setup
	overhead := 12
	for _, c := range t.chunks {
		if c.id != "data" {
			overhead += 8 + c.size
		} else {
			overhead += 8
		}
	}
	overhead += len(t.setupPacket)
	// Seektabelle: Dichte wie im Original
	origEntries := t.setupOff / 4
	kPk := 32
	if origEntries > 0 && t.samples > 0 {
		kPk = int(math.Round(float64(t.samples) / float64(origEntries) / float64(hop)))
	}
	if kPk < 1 {
		kPk = 1
	}
	if kPk > 60 {
		kPk = 60
	}
	tableBytes := ((enc.nblocks+kPk-1)/kPk + 1) * 4
	budget := float64(maxBytes) - float64(overhead+tableBytes) - 256
	if maxBytes <= 0 {
		budget = math.Inf(1)
	}
	if log != nil {
		log(fmt.Sprintf("Encoder: %d Kanaele, %d Bloecke, Budget %.1f MB", t.channels, enc.nblocks, budget/1e6))
	}
	gammaMin, gammaMax := 0.04, 6.0
	gamma := gammaMin
	est, err := enc.estimateBytes(gamma)
	if err != nil {
		return nil, "", err
	}
	if est > budget {
		lo, hi := math.Log(gammaMin), math.Log(gammaMax)
		estHi, err := enc.estimateBytes(gammaMax)
		if err != nil {
			return nil, "", err
		}
		if estHi > budget {
			return nil, "", fmt.Errorf("too large even at lowest quality (%.1f MB > %.1f MB) – please use a shorter file", estHi/1e6, budget/1e6)
		}
		for it := 0; it < 12; it++ {
			mid := (lo + hi) / 2
			e, err := enc.estimateBytes(math.Exp(mid))
			if err != nil {
				return nil, "", err
			}
			if e > budget*0.985 {
				lo = mid
			} else {
				hi = mid
			}
		}
		gamma = math.Exp(hi)
	}
	var packets [][]byte
	allIdx := make([]int, enc.nblocks)
	for i := range allIdx {
		allIdx[i] = i
	}
	for try := 0; try < 12; try++ {
		if log != nil {
			log(fmt.Sprintf("Encodiere (Qualitaetsstufe %.3f)…", gamma))
		}
		packets, err = enc.encodeBlocks(allIdx, gamma)
		if err != nil {
			return nil, "", err
		}
		tot := 0
		for _, p := range packets {
			tot += len(p) + 2
		}
		if float64(tot) <= budget {
			break
		}
		gamma *= 1.05
		if try == 11 {
			return nil, "", errors.New("Zielgroesse nicht erreichbar")
		}
	}
	lead := 0
	if t.modPackets {
		// Wwise beginnt jeden Stream mit einem kurzen Block (hier: stilles Dummy-Paket),
		// danach folgen die langen Bloecke. Das Audio ist dadurch um (bs0+bs1)/4 Samples verzoegert.
		lead = (1<<uint(t.bs0) + 1<<uint(t.bs1)) / 4
		packets = append([][]byte{t.silentShortPacket()}, packets...)
	}
	out := assembleWem(t, packets, frames, kPk, hop, lead)
	note := fmt.Sprintf("Wwise-Vorbis, %d Kanaele, %d Hz, Qualitaetsstufe %.2f", t.channels, t.rate, gamma)
	return out, note, nil
}

func assembleWem(t *wemTemplate, packets [][]byte, frames, kPk, hop, lead int) []byte {
	// Seektabelle
	var table []byte
	maxPk := 0
	for i := 0; i < len(packets); {
		cnt := kPk
		smp := 0
		if i == 0 && lead > 0 {
			cnt = kPk + 1 // Dummy-Paket + kPk lange
			smp = lead + kPk*hop
		}
		end := i + cnt
		if end > len(packets) {
			end = len(packets)
		}
		if smp == 0 || end-i < cnt {
			smp = (end - i) * hop
			if i == 0 && lead > 0 {
				smp = lead + (end-i-1)*hop
			}
		}
		bytes := 0
		for _, p := range packets[i:end] {
			bytes += len(p) + 2
			if len(p) > maxPk {
				maxPk = len(p)
			}
		}
		if i == 0 {
			bytes += len(t.setupPacket) // wie bei Wwise: erster Eintrag zaehlt das Setup-Paket mit
		}
		if bytes > 65535 {
			bytes = 65535
		}
		if smp > 65535 {
			smp = 65535
		}
		table = binary.LittleEndian.AppendUint16(table, uint16(smp))
		table = binary.LittleEndian.AppendUint16(table, uint16(bytes))
		i = end
	}
	data := append([]byte{}, table...)
	data = append(data, t.setupPacket...)
	for _, p := range packets {
		data = binary.LittleEndian.AppendUint16(data, uint16(len(p)))
		data = append(data, p...)
	}
	setupOff := len(table)
	firstAudio := setupOff + len(t.setupPacket)

	var out []byte
	out = append(out, "RIFF\x00\x00\x00\x00WAVE"...)
	for _, c := range t.chunks {
		out = append(out, c.id...)
		switch c.id {
		case "data":
			out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
			out = append(out, data...)
		case "fmt ":
			f := append([]byte{}, t.raw[c.off:c.off+c.size]...)
			avg := uint32(float64(len(data)) * float64(t.rate) / float64(frames))
			binary.LittleEndian.PutUint32(f[8:], avg)
			v := 0x18
			binary.LittleEndian.PutUint32(f[v:], uint32(frames))
			binary.LittleEndian.PutUint32(f[v+8:], uint32(len(data)-setupOff))
			binary.LittleEndian.PutUint32(f[v+0x10:], uint32(setupOff))
			binary.LittleEndian.PutUint32(f[v+0x14:], uint32(firstAudio))
			w := binary.LittleEndian.Uint32(f[v+0x18:])
			binary.LittleEndian.PutUint32(f[v+0x18:], (w&0xFFFF0000)|uint32(maxPk))
			out = binary.LittleEndian.AppendUint32(out, uint32(len(f)))
			out = append(out, f...)
		default:
			out = binary.LittleEndian.AppendUint32(out, uint32(c.size))
			out = append(out, t.raw[c.off:c.off+c.size]...)
		}
	}
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

// silentShortPacket: Paket im kurzen Modus ohne Inhalt (Boden "ungenutzt" in allen Kanaelen).
func (t *wemTemplate) silentShortPacket() []byte {
	st := t.setup
	mode := 0
	for i, f := range st.modeFlag {
		if !f {
			mode = i
			break
		}
	}
	bw := &bitWriter{}
	bw.write(uint32(mode), st.modeBits)
	for c := 0; c < t.channels; c++ {
		bw.write(0, 1)
	}
	b := bw.bytes()
	for len(b) < 2 {
		b = append(b, 0)
	}
	return b
}
