package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// cp_music.bnk (Wwise-Bank Version 150): Das Hauptmenue (Event mus_mainmenu_play) besteht aus
// drei Musik-Segmenten, je mit einer Spur. Wir setzen fuer jede Spur Start/Trim auf 0, die Laenge
// auf die des neuen Songs, Exit-Cue = Ende und stellen von "Prefetch" auf reines Streaming.

// Urspruengliche Streams (werden auch von anderer Musik im Spiel genutzt - deshalb nicht ueberschreiben!)
var menuSources = []uint32{749435057, 117878362, 1040680541}

// Die drei Musik-Spuren, die nur das Hauptmenue nutzt (gleiche Reihenfolge wie menuSources).
var menuTracks = []uint32{0x08308d4b, 0x19ba352c, 0x35f44a71}

// Neue, eigene Stream-ID fuer den Song des Nutzers.
const newSource uint32 = 1938400117

const (
	cueEntryID = 0x0298DF12
	cueExitID  = 0x5BBBD648
)

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func f64At(b []byte, o int) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b[o:]))
}
func putF64(b []byte, o int, v float64) { binary.LittleEndian.PutUint64(b[o:], math.Float64bits(v)) }

type bnkObj struct {
	t     byte
	start int
	n     int
}

func bankObjects(d []byte) ([]bnkObj, error) {
	if len(d) < 60 || string(d[:4]) != "BKHD" {
		return nil, errors.New("not a Wwise bank")
	}
	if ver := le32(d[8:]); ver != 150 {
		return nil, fmt.Errorf("unexpected bank version %d (expected 150) - different game version?", ver)
	}
	q := 0
	hirc, hlen := 0, 0
	for q+8 <= len(d) {
		tag := string(d[q : q+4])
		n := int(le32(d[q+4:]))
		if tag == "HIRC" {
			hirc, hlen = q+8, n
			break
		}
		q += 8 + n
	}
	if hirc == 0 {
		return nil, errors.New("HIRC not found")
	}
	cnt := int(le32(d[hirc:]))
	p := hirc + 4
	var objs []bnkObj
	for i := 0; i < cnt && p+5 <= hirc+hlen; i++ {
		n := int(le32(d[p+1:]))
		objs = append(objs, bnkObj{d[p], p + 5, n})
		p += 5 + n
	}
	return objs, nil
}

type menuTrack struct {
	Source  uint32
	Track   uint32
	TrackAt int // Offset Objektanfang (Spur)
	SegAt   int
	SegLen  int
	Dur     float64 // aktuelle Segmentdauer
}

func analyzeMenu(d []byte) ([]menuTrack, error) {
	objs, err := bankObjects(d)
	if err != nil {
		return nil, err
	}
	var out []menuTrack
	for i, trackID := range menuTracks {
		sid := menuSources[i]
		sb := make([]byte, 4)
		binary.LittleEndian.PutUint32(sb, sid)
		tkb := make([]byte, 4)
		binary.LittleEndian.PutUint32(tkb, trackID)
		var tr *bnkObj
		for j := range objs {
			o := &objs[j]
			if o.t == 11 && o.n > 80 && bytes.Equal(d[o.start:o.start+4], tkb) {
				tr = o
			}
		}
		if tr == nil {
			return nil, fmt.Errorf("menu track %d not found", i+1)
		}
		tb := d[tr.start : tr.start+tr.n]
		cur := tb[14:18]
		if le32(tb[23:]) != 1 || !bytes.Equal(tb[31:35], cur) || !(bytes.Equal(cur, sb) || le32(cur) == newSource) {
			return nil, fmt.Errorf("unexpected track layout for stream %d", sid)
		}
		srcDur := f64At(tb, 63)
		if !(srcDur >= 500 && srcDur < 3.6e7) {
			// bereits angepasst? dann Dauer trotzdem im Rahmen
			return nil, fmt.Errorf("implausible track duration %.1f", srcDur)
		}
		tid := tb[:4]
		var sg *bnkObj
		for i := range objs {
			o := &objs[i]
			if o.t == 10 && o.n > 60 && bytes.Contains(d[o.start+4:o.start+o.n], tid) {
				if sg != nil {
					return nil, errors.New("multiple segments for one menu track")
				}
				sg = o
			}
		}
		if sg == nil {
			return nil, fmt.Errorf("segment for stream %d not found", sid)
		}
		sbytes := d[sg.start : sg.start+sg.n]
		e := len(sbytes) - 38
		if le32(sbytes[e+8:]) != 2 || le32(sbytes[e+12:]) != cueEntryID || le32(sbytes[e+25:]) != cueExitID {
			return nil, fmt.Errorf("unexpected segment markers for stream %d", sid)
		}
		out = append(out, menuTrack{Source: sid, Track: le32(tb), TrackAt: tr.start, SegAt: sg.start, SegLen: sg.n, Dur: f64At(sbytes, e)})
	}
	return out, nil
}

// patchMenu liefert die angepasste Bank (gleiche Groesse) fuer eine Songlaenge in ms.
func patchMenu(orig []byte, durMs float64) ([]byte, []menuTrack, error) {
	mt, err := analyzeMenu(orig)
	if err != nil {
		return nil, nil, err
	}
	out := append([]byte(nil), orig...)
	for _, m := range mt {
		t := out[m.TrackAt:]
		t[13] = 1 // Streaming ohne Prefetch
		binary.LittleEndian.PutUint32(t[14:], newSource)
		binary.LittleEndian.PutUint32(t[31:], newSource)
		binary.LittleEndian.PutUint32(t[18:], 0) // Prefetch-Groesse
		putF64(t, 39, 0)                         // PlayAt
		putF64(t, 47, 0)                         // BeginTrim
		putF64(t, 55, 0)                         // EndTrim
		putF64(t, 63, durMs)                     // Quelldauer
		s := out[m.SegAt : m.SegAt+m.SegLen]
		e := len(s) - 38
		putF64(s, e, durMs)    // Segmentdauer
		putF64(s, e+16, 0)     // Entry-Cue
		putF64(s, e+29, durMs) // Exit-Cue
	}
	// Zweiter Musik-Container (gleiches Hauptthema, gleiche Schalter): ALLE Schalter-Werte zeigen
	// auf unsere Playlist, damit es keinen Original-Schnipsel und keinen Neustart beim Wechsel gibt.
	if objs, e := bankObjects(out); e == nil {
		for _, o := range objs {
			if o.t == 12 && le32(out[o.start:]) == 0x1d7d7ed1 {
				blob := out[o.start : o.start+o.n]
				tail := blob[len(blob)-120:]
				keys := []uint32{0x2f056c5d, 0x5bd1d8be, 0x778c9dfc, 0xe5ac7864}
				for _, k := range keys {
					kb := make([]byte, 4)
					binary.LittleEndian.PutUint32(kb, k)
					if i := bytes.Index(tail, kb); i >= 0 && bytes.Count(tail, kb) == 1 && i+8 <= len(tail) {
						binary.LittleEndian.PutUint32(tail[i+4:], 0x09a3a330)
					}
				}
			}
		}
	}
	// Der Original-Jingle (Zustand e5ac7864, Spur 0x234d34df, 2 s) wird auf 1 ms verkuerzt.
	if objs, e := bankObjects(out); e == nil {
		tid := []byte{0xdf, 0x34, 0x4d, 0x23}
		for _, o := range objs {
			if o.t == 11 && le32(out[o.start:]) == 0x234d34df && o.n > 71 {
				putF64(out[o.start:], 63, 1)
			}
			if o.t == 10 && o.n > 60 && bytes.Contains(out[o.start+4:o.start+o.n], tid) {
				sg := out[o.start : o.start+o.n]
				e := len(sg) - 38
				if le32(sg[e+8:]) == 2 && le32(sg[e+12:]) == cueEntryID && le32(sg[e+25:]) == cueExitID {
					putF64(sg, e, 1)
					putF64(sg, e+16, 0)
					putF64(sg, e+29, 1)
				}
			}
		}
	}
	// Der erste Container (Event mus_mainmenu_play) spielt sonst vorab einen kurzen Schnipsel.
	// Seine Play-Aktion wird ins Leere geleitet - das eigentliche Menue-Thema laeuft ueber den zweiten Container.
	if objs, e := bankObjects(out); e == nil {
		for _, o := range objs {
			if o.t == 3 && o.n == 22 && le32(out[o.start:]) == 0x3e4d4cea && le32(out[o.start+6:]) == 0x2136824c {
				binary.LittleEndian.PutUint32(out[o.start+6:], 0x0badc0de)
			}
		}
	}
	chk, err := analyzeMenu(out)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range chk {
		if math.Abs(m.Dur-durMs) > 0.01 {
			return nil, nil, errors.New("bank verification failed")
		}
	}
	return out, mt, nil
}
