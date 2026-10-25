package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	mp3 "github.com/hajimehoshi/go-mp3"
)

func nowStr() string { return time.Now().Format("2006-01-02 15:04:05") }

// PCM: interleaved float32 [-1,1]
type PCM struct {
	Rate, Ch int
	Data     []float32
}

func (p *PCM) Frames() int { return len(p.Data) / p.Ch }

func decodeAudio(name string, data []byte) (*PCM, error) {
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, ".mp3"):
		return decodeMP3(data)
	case strings.HasSuffix(l, ".wav"), strings.HasSuffix(l, ".wem"):
		return decodeWAV(data)
	}
	if len(data) > 12 && string(data[:4]) == "RIFF" {
		return decodeWAV(data)
	}
	if len(data) > 3 && (string(data[:3]) == "ID3" || (data[0] == 0xFF && data[1]&0xE0 == 0xE0)) {
		return decodeMP3(data)
	}
	return nil, errors.New("unknown format – please use MP3 or WAV")
}

func decodeMP3(data []byte) (*PCM, error) {
	d, err := mp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("MP3 unreadable: %w", err)
	}
	raw, err := io.ReadAll(d)
	if err != nil {
		return nil, err
	}
	n := len(raw) / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = float32(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
	}
	return &PCM{Rate: d.SampleRate(), Ch: 2, Data: out}, nil
}

func decodeWAV(b []byte) (*PCM, error) {
	if len(b) < 44 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("not a valid WAV file")
	}
	var tag, ch, bits uint16
	var rate uint32
	var pcm []byte
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		sz := int(binary.LittleEndian.Uint32(b[pos+4:]))
		body := pos + 8
		end := body + sz
		if end > len(b) || sz < 0 {
			end = len(b)
		}
		switch id {
		case "fmt ":
			tag = binary.LittleEndian.Uint16(b[body:])
			ch = binary.LittleEndian.Uint16(b[body+2:])
			rate = binary.LittleEndian.Uint32(b[body+4:])
			bits = binary.LittleEndian.Uint16(b[body+14:])
			if tag == 0xFFFE && sz >= 26 {
				tag = binary.LittleEndian.Uint16(b[body+24:])
			}
		case "data":
			pcm = b[body:end]
		}
		pos = end + (end & 1)
	}
	if ch == 0 || pcm == nil {
		return nil, errors.New("WAV ohne Audiodaten")
	}
	bytesPer := int(bits) / 8
	n := len(pcm) / bytesPer
	out := make([]float32, n)
	switch {
	case tag == 1 && bits == 16:
		for i := range out {
			out[i] = float32(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) / 32768
		}
	case tag == 1 && bits == 24:
		for i := range out {
			v := int32(pcm[i*3]) | int32(pcm[i*3+1])<<8 | int32(int8(pcm[i*3+2]))<<16
			out[i] = float32(v) / 8388608
		}
	case tag == 1 && bits == 32:
		for i := range out {
			out[i] = float32(int32(binary.LittleEndian.Uint32(pcm[i*4:]))) / 2147483648
		}
	case tag == 1 && bits == 8:
		for i := range out {
			out[i] = (float32(pcm[i]) - 128) / 128
		}
	case tag == 3 && bits == 32:
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(pcm[i*4:]))
		}
	default:
		return nil, fmt.Errorf("WAV-Format nicht unterstuetzt (Tag %d, %d Bit) - bitte als 16-Bit-WAV speichern", tag, bits)
	}
	n -= n % int(ch)
	return &PCM{Rate: int(rate), Ch: int(ch), Data: out[:n]}, nil
}

// Kanalanpassung an das Original-Layout.
func remapChannels(p *PCM, target int) *PCM {
	if p.Ch == target {
		return p
	}
	fr := p.Frames()
	out := make([]float32, fr*target)
	for i := 0; i < fr; i++ {
		var l, r float32
		if p.Ch == 1 {
			l = p.Data[i]
			r = l
		} else {
			l = p.Data[i*p.Ch]
			r = p.Data[i*p.Ch+1]
		}
		o := out[i*target:]
		switch target {
		case 1:
			o[0] = (l + r) / 2
		case 2:
			o[0], o[1] = l, r
		case 4: // FL FR BL BR
			o[0], o[1], o[2], o[3] = l, r, l*0.5, r*0.5
		case 6: // FL FR FC LFE BL BR
			o[0], o[1], o[2], o[3], o[4], o[5] = l, r, (l+r)*0.35, 0, l*0.5, r*0.5
		default:
			o[0] = l
			if target > 1 {
				o[1] = r
			}
		}
	}
	return &PCM{Rate: p.Rate, Ch: target, Data: out}
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// Lanczos-Resampler (a=3), mit Tiefpass beim Verkleinern der Rate.
func resample(p *PCM, newRate int) *PCM {
	if p.Rate == newRate {
		return p
	}
	ratio := float64(p.Rate) / float64(newRate)
	cut := 1.0
	if ratio > 1 {
		cut = 1 / ratio
	}
	const a = 3.0
	fr := p.Frames()
	nOut := int(float64(fr) / ratio)
	out := make([]float32, nOut*p.Ch)
	support := a / cut
	for i := 0; i < nOut; i++ {
		c := float64(i) * ratio
		lo := int(math.Ceil(c - support))
		hi := int(math.Floor(c + support))
		for ch := 0; ch < p.Ch; ch++ {
			var acc, wsum float64
			for j := lo; j <= hi; j++ {
				if j < 0 || j >= fr {
					continue
				}
				x := (float64(j) - c) * cut
				w := sinc(x) * sinc(x/a)
				acc += w * float64(p.Data[j*p.Ch+ch])
				wsum += w
			}
			if wsum != 0 {
				acc /= wsum
			}
			out[i*p.Ch+ch] = float32(acc)
		}
	}
	return &PCM{Rate: newRate, Ch: p.Ch, Data: out}
}

func chanMask(ch int, orig uint32) uint32 {
	cnt := 0
	for m := orig; m != 0; m &= m - 1 {
		cnt++
	}
	if cnt == ch && orig != 0 {
		return orig
	}
	switch ch {
	case 1:
		return 4
	case 2:
		return 3
	case 4:
		return 0x33
	case 6:
		return 0x3F
	}
	return 0
}

// buildPCMWem erzeugt eine Wwise-kompatible PCM-WEM (16 Bit).
func buildPCMWem(p *PCM, mask uint32) []byte {
	n := len(p.Data)
	data := make([]byte, n*2)
	for i, v := range p.Data {
		x := float64(v) * 32767
		if x > 32767 {
			x = 32767
		} else if x < -32768 {
			x = -32768
		}
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(math.Round(x))))
	}
	var fmtc bytes.Buffer
	ba := uint16(p.Ch * 2)
	if p.Ch <= 2 {
		binary.Write(&fmtc, binary.LittleEndian, uint16(1))
	} else {
		binary.Write(&fmtc, binary.LittleEndian, uint16(0xFFFE))
	}
	binary.Write(&fmtc, binary.LittleEndian, uint16(p.Ch))
	binary.Write(&fmtc, binary.LittleEndian, uint32(p.Rate))
	binary.Write(&fmtc, binary.LittleEndian, uint32(p.Rate)*uint32(ba))
	binary.Write(&fmtc, binary.LittleEndian, ba)
	binary.Write(&fmtc, binary.LittleEndian, uint16(16))
	if p.Ch > 2 {
		binary.Write(&fmtc, binary.LittleEndian, uint16(22))
		binary.Write(&fmtc, binary.LittleEndian, uint16(16))
		binary.Write(&fmtc, binary.LittleEndian, mask)
		// KSDATAFORMAT_SUBTYPE_PCM
		fmtc.Write([]byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71})
	}
	var out bytes.Buffer
	out.WriteString("RIFF")
	binary.Write(&out, binary.LittleEndian, uint32(4+8+fmtc.Len()+8+len(data)))
	out.WriteString("WAVEfmt ")
	binary.Write(&out, binary.LittleEndian, uint32(fmtc.Len()))
	out.Write(fmtc.Bytes())
	out.WriteString("data")
	binary.Write(&out, binary.LittleEndian, uint32(len(data)))
	out.Write(data)
	return out.Bytes()
}

// convertForSlot bereitet eine Datei fuer ein Ziel-Wem auf.
// maxBytes>0: Rate wird bei Bedarf gesenkt, bis die Datei passt.
func convertForSlot(name string, src []byte, ch, rate int, mask uint32, maxBytes int64, fitDur float64) ([]byte, string, error) {
	p, err := decodeAudio(name, src)
	if err != nil {
		return nil, "", err
	}
	if rate <= 0 {
		rate = 48000
	}
	if fitDur > 0 {
		p = fitDuration(p, fitDur)
	}
	p = remapChannels(p, ch)
	note := fmt.Sprintf("PCM 16 Bit, %d Kanaele, %d Hz", ch, rate)
	useRate := rate
	if maxBytes > 0 {
		dur := float64(p.Frames()) / float64(p.Rate)
		need := func(r int) int64 { return int64(dur*float64(r))*int64(ch)*2 + 128 }
		for need(useRate) > maxBytes && useRate > 8000 {
			useRate = useRate * 3 / 4
		}
		if need(useRate) > maxBytes {
			return nil, "", fmt.Errorf("auch bei 8000 Hz zu gross fuer den vorhandenen Platz")
		}
		if useRate != rate {
			note += fmt.Sprintf(" (Rate auf %d Hz gesenkt, damit es in den Platz passt)", useRate)
		}
	}
	p = resample(p, useRate)
	return buildPCMWem(p, chanMask(ch, mask)), note, nil
}

// fitDuration schneidet auf die Originallaenge (kurze Ausblendung am Ende) oder
// fuellt mit Stille auf. Die Spiel-Schleife wird von der Bank bestimmt und kann nicht geaendert werden.
func fitDuration(p *PCM, dur float64) *PCM {
	want := int(dur * float64(p.Rate))
	have := p.Frames()
	out := make([]float32, want*p.Ch)
	n := have
	if want < n {
		n = want
	}
	copy(out, p.Data[:n*p.Ch])
	if want < have {
		fade := p.Rate / 20
		if fade > want {
			fade = want
		}
		for i := 0; i < fade; i++ {
			g := float32(fade-i) / float32(fade)
			for c := 0; c < p.Ch; c++ {
				out[(want-fade+i)*p.Ch+c] *= g
			}
		}
	}
	return &PCM{Rate: p.Rate, Ch: p.Ch, Data: out}
}
