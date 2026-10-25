package main

// Wwise-Vorbis-Encoder: nutzt das Setup (Codebuecher, Floor, Residue) der Original-WEM,
// damit der Spiel-Decoder die erzeugte Datei sicher versteht.

import (
	"errors"
	"fmt"
	"math"
	"math/cmplx"
	"runtime"
	"sort"
	"sync"
)

// ---------- FFT / MDCT ----------

type fftPlan struct {
	n   int
	rev []int
	tw  []complex128
}

func newFFT(n int) *fftPlan {
	p := &fftPlan{n: n, rev: make([]int, n), tw: make([]complex128, n/2)}
	bits := 0
	for 1<<uint(bits) < n {
		bits++
	}
	for i := 0; i < n; i++ {
		r := 0
		for b := 0; b < bits; b++ {
			if i&(1<<uint(b)) != 0 {
				r |= 1 << uint(bits-1-b)
			}
		}
		p.rev[i] = r
	}
	for i := range p.tw {
		p.tw[i] = cmplx.Exp(complex(0, -2*math.Pi*float64(i)/float64(n)))
	}
	return p
}

func (p *fftPlan) transform(a []complex128) {
	n := p.n
	for i := 0; i < n; i++ {
		if j := p.rev[i]; j > i {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		step := n / size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				w := p.tw[k*step]
				u := a[start+k]
				v := a[start+k+half] * w
				a[start+k] = u + v
				a[start+k+half] = u - v
			}
		}
	}
}

type mdct struct {
	n, m int
	win  []float64
	fft  *fftPlan
	pre  []complex128
	post []complex128
	buf  []complex128
	u    []float64
}

func newMDCT(n int) *mdct {
	m := n / 2
	t := &mdct{n: n, m: m, win: make([]float64, n), fft: newFFT(m / 2), pre: make([]complex128, m/2), post: make([]complex128, m/2), buf: make([]complex128, m/2), u: make([]float64, m)}
	for i := 0; i < n; i++ {
		s := math.Sin(math.Pi * (float64(i) + 0.5) / float64(n))
		t.win[i] = math.Sin(math.Pi / 2 * s * s)
	}
	for i := range t.pre {
		t.pre[i] = cmplx.Exp(complex(0, -math.Pi*float64(i)/float64(m)))
		t.post[i] = cmplx.Exp(complex(0, -math.Pi*(float64(i)+0.25)/float64(m)))
	}
	return t
}

// dct4 berechnet X[k] = sum u[n] cos(pi/M (n+1/2)(k+1/2)) in out.
func (t *mdct) dct4(u []float64, out []float64) {
	m := t.m
	for i := 0; i < m/2; i++ {
		t.buf[i] = complex(u[2*i], u[m-1-2*i]) * t.pre[i]
	}
	t.fft.transform(t.buf)
	for k := 0; k < m/2; k++ {
		y := t.buf[k] * t.post[k]
		out[2*k] = real(y)
		out[m-1-2*k] = -imag(y)
	}
}

// forward: x (Laenge n, ungefenstert) -> out (Laenge m), mit Skalierung 4/n wie libvorbis.
func (t *mdct) forward(x []float64, out []float64) {
	m := t.m
	h := m / 2
	w := t.win
	for i := 0; i < h; i++ {
		// u[i] = -c_r[i] - d[i]
		t.u[i] = -x[3*h-1-i]*w[3*h-1-i] - x[3*h+i]*w[3*h+i]
		// u[h+i] = a[i] - b_r[i]
		t.u[h+i] = x[i]*w[i] - x[m-1-i]*w[m-1-i]
	}
	t.dct4(t.u, out)
	sc := 4.0 / float64(t.n)
	for i := range out[:m] {
		out[i] *= sc
	}
}

// ---------- Floor1 ----------

var dbTable [256]float64
var dbLn0, dbStep float64

func init() {
	dbLn0 = math.Log(1.0649863e-07)
	dbStep = -dbLn0 / 255
	for i := range dbTable {
		dbTable[i] = math.Exp(dbLn0 + dbStep*float64(i))
	}
}

func renderPoint(x0, y0, x1, y1, x int) int {
	dy := y1 - y0
	adx := x1 - x0
	ady := dy
	if ady < 0 {
		ady = -ady
	}
	off := ady * (x - x0) / adx
	if dy < 0 {
		return y0 - off
	}
	return y0 + off
}

func renderLine(x0, y0, x1, y1 int, out []float64) {
	dy := y1 - y0
	adx := x1 - x0
	if adx <= 0 {
		return
	}
	ady := dy
	if ady < 0 {
		ady = -ady
	}
	base := dy / adx
	x := x0
	y := y0
	err := 0
	sy := base + 1
	if dy < 0 {
		sy = base - 1
	}
	ab := base
	if ab < 0 {
		ab = -ab
	}
	ady -= ab * adx
	if x < len(out) {
		out[x] = dbTable[clampInt(y, 0, 255)]
	}
	for x++; x < x1; x++ {
		err += ady
		if err >= adx {
			err -= adx
			y += sy
		} else {
			y += base
		}
		if x < len(out) {
			out[x] = dbTable[clampInt(y, 0, 255)]
		}
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (f *floor1) curve(finalY []int, step2 []bool, out []float64) {
	lx := 0
	ly := finalY[f.order[0]] * f.mult
	hx, hy := 0, 0
	for j := 1; j < len(f.order); j++ {
		i := f.order[j]
		if step2[i] {
			hy = finalY[i] * f.mult
			hx = f.x[i]
			renderLine(lx, ly, hx, hy, out)
			lx, ly = hx, hy
		}
	}
	if hx < len(out) {
		renderLine(hx, hy, len(out), hy, out)
	}
}

// decodeVal: aus dem gelesenen Wert den endgueltigen Y-Wert berechnen (Spec 7.2.4).
func decodeFloorVal(val, pred, rng int) int {
	highroom := rng - pred
	lowroom := pred
	var room int
	if highroom < lowroom {
		room = highroom * 2
	} else {
		room = lowroom * 2
	}
	if val >= room {
		if highroom > lowroom {
			return val - lowroom + pred
		}
		return pred - val + highroom - 1
	}
	if val&1 == 1 {
		return pred - (val+1)/2
	}
	return pred + val/2
}

// encodeFloorVal liefert den Wert, der final ergibt (ok=false, wenn nicht darstellbar).
func encodeFloorVal(final, pred, rng int) (int, bool) {
	if final == pred {
		return 0, true
	}
	highroom := rng - pred
	lowroom := pred
	var room int
	if highroom < lowroom {
		room = highroom * 2
	} else {
		room = lowroom * 2
	}
	d := final - pred
	if d > 0 {
		if 2*d < room {
			return 2 * d, true
		}
		if highroom > lowroom {
			v := d + lowroom
			return v, v >= room
		}
		return 0, false
	}
	m := -d
	if 2*m-1 < room {
		return 2*m - 1, true
	}
	if highroom <= lowroom {
		v := m + highroom - 1
		return v, v >= room
	}
	return 0, false
}

func (f *floor1) predict(i int, finalY []int) int {
	lo, hi := f.low[i], f.high[i]
	return renderPoint(f.x[lo], finalY[lo], f.x[hi], finalY[hi], f.x[i])
}

func codeLen(b *codebook, e int) int {
	if e < 0 || e >= len(b.lengths) {
		return 0
	}
	return b.lengths[e]
}

// encode schreibt den Floor eines Kanals. target = gewuenschte Y-Werte pro Stuetzpunkt.
func (f *floor1) encode(bw *bitWriter, books []*codebook, target []int) ([]int, []bool, error) {
	n := len(f.x)
	finalY := make([]int, n)
	step2 := make([]bool, n)
	bw.write(1, 1)
	rb := ilog(uint32(f.rng - 1))
	finalY[0] = clampInt(target[0], 0, f.rng-1)
	finalY[1] = clampInt(target[1], 0, f.rng-1)
	step2[0], step2[1] = true, true
	bw.write(uint32(finalY[0]), rb)
	bw.write(uint32(finalY[1]), rb)
	offset := 2
	const W = 2.0
	for p := 0; p < f.partitions; p++ {
		cl := f.classes[f.pcl[p]]
		cdim, cbits := cl.dim, cl.sub
		csub := (1 << uint(cbits)) - 1
		subs := make([]int, cdim)
		vals := make([]int, cdim)
		tmpY := make([]int, n)
		copy(tmpY, finalY)
		for j := 0; j < cdim; j++ {
			i := offset + j
			pred := f.predict(i, tmpY)
			bestS, bestV, bestFinal := 0, 0, pred
			bestCost := math.Inf(1)
			for s := 0; s <= csub; s++ {
				bk := cl.subbooks[s]
				var v, fin int
				var cost float64
				if bk < 0 {
					v, fin = 0, pred
					d := float64(fin - target[i])
					cost = W * d * d
				} else {
					b := books[bk]
					v, fin = bestRepresentable(f, b, target[i], pred)
					if v < 0 {
						continue
					}
					d := float64(fin - target[i])
					cost = W*d*d + float64(codeLen(b, v))
				}
				if cost < bestCost {
					bestCost, bestS, bestV, bestFinal = cost, s, v, fin
				}
			}
			subs[j], vals[j] = bestS, bestV
			tmpY[i] = bestFinal
		}
		cval := 0
		for j := 0; j < cdim; j++ {
			cval |= subs[j] << uint(cbits*j)
		}
		if cbits > 0 {
			mb := books[cl.master]
			if cval >= mb.entries || mb.lengths[cval] == 0 {
				// Ersatz: naechste gueltige Kombination suchen
				found := false
				bestCost := math.Inf(1)
				for cv := 0; cv < mb.entries; cv++ {
					if mb.lengths[cv] == 0 {
						continue
					}
					sv, vv, ty, cost, ok := f.simulate(books, cl, cv, offset, finalY, target, W)
					if ok && cost < bestCost {
						bestCost, found = cost, true
						subs, vals, tmpY, cval = sv, vv, ty, cv
					}
				}
				if !found {
					return nil, nil, errors.New("Floor: keine gueltige Codebuch-Kombination")
				}
			}
			bw.writeCode(mb.codes[cval], mb.lengths[cval])
		}
		for j := 0; j < cdim; j++ {
			bk := cl.subbooks[subs[j]]
			if bk >= 0 {
				b := books[bk]
				bw.writeCode(b.codes[vals[j]], b.lengths[vals[j]])
			}
		}
		copy(finalY, tmpY)
		// step2: nur Punkte mit val != 0 sind Stuetzpunkte
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
	return finalY, step2, nil
}

func (f *floor1) simulate(books []*codebook, cl floorClass, cval, offset int, finalY, target []int, W float64) ([]int, []int, []int, float64, bool) {
	cdim, cbits := cl.dim, cl.sub
	csub := (1 << uint(cbits)) - 1
	tmp := make([]int, len(finalY))
	copy(tmp, finalY)
	subs := make([]int, cdim)
	vals := make([]int, cdim)
	cost := 0.0
	for j := 0; j < cdim; j++ {
		i := offset + j
		pred := f.predict(i, tmp)
		s := (cval >> uint(cbits*j)) & csub
		subs[j] = s
		bk := cl.subbooks[s]
		if bk < 0 {
			tmp[i] = pred
			d := float64(pred - target[i])
			cost += W * d * d
			continue
		}
		v, fin := bestRepresentable(f, books[bk], target[i], pred)
		if v < 0 {
			return nil, nil, nil, 0, false
		}
		vals[j] = v
		tmp[i] = fin
		d := float64(fin - target[i])
		cost += W*d*d + float64(codeLen(books[bk], v))
	}
	return subs, vals, tmp, cost, true
}

// bestRepresentable: bester darstellbarer Wert fuer das Ziel; v<0 wenn keiner.
func bestRepresentable(f *floor1, b *codebook, target, pred int) (int, int) {
	target = clampInt(target, 0, f.rng-1)
	if v, ok := encodeFloorVal(target, pred, f.rng); ok && v < b.entries && b.lengths[v] > 0 {
		return v, target
	}
	bestV, bestF := -1, 0
	bestD := 1 << 30
	for _, e := range b.used {
		fin := decodeFloorVal(e, pred, f.rng)
		if fin < 0 || fin >= f.rng {
			continue
		}
		d := fin - target
		if d < 0 {
			d = -d
		}
		if d < bestD || (d == bestD && b.lengths[e] < b.lengths[bestV]) {
			bestD, bestV, bestF = d, e, fin
		}
	}
	return bestV, bestF
}

// ---------- Residue ----------

type partPlan struct {
	class   int
	entries [8][]int
}

// quantStage: bester Eintrag pro Teilvektor; liefert Eintraege, Bits und aktualisiert cur.
func quantStage(b *codebook, cur []float32) (entries []int, bits int) {
	d := b.dim
	cnt := len(cur) / d
	entries = make([]int, cnt)
	for c := 0; c < cnt; c++ {
		v := cur[c*d : c*d+d]
		bestE, bestD := -1, float32(math.MaxFloat32)
		for _, e := range b.used {
			t := b.vec[e]
			var s float32
			for k := 0; k < d; k++ {
				x := v[k] - t[k]
				s += x * x
			}
			if s < bestD || (s == bestD && b.lengths[e] < b.lengths[bestE]) {
				bestD, bestE = s, e
			}
		}
		entries[c] = bestE
		bits += b.lengths[bestE]
		t := b.vec[bestE]
		for k := 0; k < d; k++ {
			v[k] -= t[k]
		}
	}
	return
}

func sumSq(v []float32) float32 {
	var s float32
	for _, x := range v {
		s += x * x
	}
	return s
}

// planPartition wählt die Klasse für einen Partitionsvektor.
func (r *residue) planPartition(books []*codebook, part []float32, order []int) partPlan {
	ps := len(part)
	okErr := float32(ps) * 0.12
	var best partPlan
	bestJ := float32(math.MaxFloat32)
	cur := make([]float32, ps)
	for _, c := range order {
		copy(cur, part)
		var plan partPlan
		plan.class = c
		bits := 0
		for s := 0; s < 8; s++ {
			bk := r.books[c][s]
			if bk < 0 {
				continue
			}
			e, bt := quantStage(books[bk], cur)
			plan.entries[s] = e
			bits += bt
		}
		err := sumSq(cur)
		j := err + 0.02*float32(bits)
		if j < bestJ {
			bestJ, best = j, plan
		}
		if err <= okErr {
			break
		}
	}
	return best
}

func (r *residue) classOrder(books []*codebook) ([]int, []float32) {
	maxR := make([]float32, r.nclass)
	for c := 0; c < r.nclass; c++ {
		var tot float32
		for s := 0; s < 8; s++ {
			bk := r.books[c][s]
			if bk < 0 {
				continue
			}
			var mx float32
			for _, e := range books[bk].used {
				for _, x := range books[bk].vec[e] {
					if x < 0 {
						x = -x
					}
					if x > mx {
						mx = x
					}
				}
			}
			tot += mx
		}
		maxR[c] = tot
	}
	order := make([]int, r.nclass)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return maxR[order[a]] < maxR[order[b]] })
	return order, maxR
}

// encode: vecs = ein Vektor pro Kanal (Typ 1) oder ein verschachtelter Vektor (Typ 2).
func (r *residue) encode(bw *bitWriter, books []*codebook, vecs [][]float32, order []int, maxR []float32) error {
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
	plans := make([][]partPlan, len(vecs))
	for j, v := range vecs {
		plans[j] = make([]partPlan, partvals)
		for i := 0; i < partvals; i++ {
			off := r.begin + i*r.ps
			plans[j][i] = r.planPartition(books, v[off:off+r.ps], order)
		}
	}
	for s := 0; s < r.stages; s++ {
		for i := 0; i < partvals; i += cdim {
			if s == 0 {
				for j := range vecs {
					e := 0
					for k := 0; k < cdim; k++ {
						c := 0
						if i+k < partvals {
							c = plans[j][i+k].class
						}
						e = e*r.nclass + c
					}
					if e >= cbk.entries || cbk.lengths[e] == 0 {
						return fmt.Errorf("Klassenwort %d nicht im Codebuch", e)
					}
					bw.writeCode(cbk.codes[e], cbk.lengths[e])
				}
			}
			for k := 0; k < cdim && i+k < partvals; k++ {
				for j := range vecs {
					pl := &plans[j][i+k]
					bk := r.books[pl.class][s]
					if bk < 0 {
						continue
					}
					b := books[bk]
					for _, e := range pl.entries[s] {
						bw.writeCode(b.codes[e], b.lengths[e])
					}
				}
			}
		}
	}
	return nil
}

// ---------- Encoder ----------

type vencoder struct {
	s        *vsetup
	mode     int // Index des langen Modus
	mp       *mapping
	n, m     int // Blockgroesse / Koeffizienten
	pcm      [][]float64
	frames   int
	nblocks  int
	orders   map[int][]int
	maxRs    map[int][]float32
	floorFor []int // pro Kanal: Floor-Index
	submapOf []int
	std      bool // Standard-Vorbis-Pakete statt Wwise-Modified
	mu       float64
}

func newEncoder(s *vsetup, bs1 int, pcm [][]float64) (*vencoder, error) {
	e := &vencoder{s: s, pcm: pcm, mu: encMu}
	e.mode = -1
	for i, f := range s.modeFlag {
		if f {
			e.mode = i
			break
		}
	}
	if e.mode < 0 {
		return nil, errors.New("Setup enthaelt keinen langen Modus")
	}
	e.mp = s.mappings[s.modeMap[e.mode]]
	e.n = 1 << uint(bs1)
	e.m = e.n / 2
	e.frames = len(pcm[0])
	e.nblocks = (e.frames+e.m-1)/e.m + 1
	e.orders = map[int][]int{}
	e.maxRs = map[int][]float32{}
	for _, rn := range e.mp.resNum {
		o, mr := s.residues[rn].classOrder(s.books)
		e.orders[rn], e.maxRs[rn] = o, mr
	}
	e.floorFor = make([]int, s.channels)
	e.submapOf = make([]int, s.channels)
	for c := 0; c < s.channels; c++ {
		sm := e.mp.mux[c]
		e.submapOf[c] = sm
		e.floorFor[c] = e.mp.floorNum[sm]
	}
	return e, nil
}

func lnClamp(v float64) float64 {
	if v < 1e-30 {
		v = 1e-30
	}
	return math.Log(v)
}

// floorTargets: Y-Zielwerte aus dem Spektrum (gamma = Rauschpegel relativ zur Huellkurve).
func (f *floor1) targets(spec []float64, gamma, mu float64) []int {
	n := len(f.x)
	t := make([]int, n)
	pos := make([]int, n) // Position in order
	for k, i := range f.order {
		pos[i] = k
	}
	rmsAll := make([]float64, n)
	peak := 0.0
	for i := 0; i < n; i++ {
		k := pos[i]
		x := f.x[i]
		lo := 0
		hi := len(spec)
		if k > 0 {
			lo = (f.x[f.order[k-1]] + x) / 2
		}
		if k+1 < n {
			hi = (x + f.x[f.order[k+1]]) / 2
		}
		if hi > len(spec) {
			hi = len(spec)
		}
		if hi <= lo {
			if lo >= len(spec) {
				lo = len(spec) - 1
			}
			hi = lo + 1
		}
		var s float64
		for _, v := range spec[lo:hi] {
			s += v * v
		}
		rms := math.Sqrt(s / float64(hi-lo))
		rmsAll[i] = rms
		if rms > peak {
			peak = rms
		}
		continue
	}
	for i := 0; i < n; i++ {
		rms := rmsAll[i]
		if rms < mu*peak {
			rms = mu * peak
		}
		F := rms * gamma
		var idx float64
		if F <= 0 {
			idx = 0
		} else {
			idx = (lnClamp(F) - dbLn0) / dbStep
		}
		y := int(math.Round(idx / float64(f.mult)))
		t[i] = clampInt(y, 0, f.rng-1)
	}
	return t
}

func (e *vencoder) encodeBlock(j int, gamma float64, tm *mdct, scratch *blockScratch) ([]byte, error) {
	s := e.s
	C := s.channels
	m, n := e.m, e.n
	start := (j - 1) * m
	bw := &bitWriter{}
	if e.std {
		bw.write(0, 1)
	}
	bw.write(uint32(e.mode), s.modeBits)
	if e.std && s.modeFlag[e.mode] {
		bw.write(1, 1)
		bw.write(1, 1)
	}
	spec := scratch.spec
	for c := 0; c < C; c++ {
		x := scratch.x
		for i := 0; i < n; i++ {
			p := start + i
			if p >= 0 && p < e.frames {
				x[i] = e.pcm[c][p]
			} else {
				x[i] = 0
			}
		}
		tm.forward(x, spec[c])
	}
	// Floors
	curves := scratch.curves
	for c := 0; c < C; c++ {
		f := s.floors[e.floorFor[c]]
		tg := f.targets(spec[c], gamma, e.mu)
		fy, st, err := f.encode(bw, s.books, tg)
		if err != nil {
			return nil, err
		}
		f.curve(fy, st, curves[c])
	}
	// Normieren
	res := scratch.res
	for c := 0; c < C; c++ {
		for k := 0; k < m; k++ {
			res[c][k] = float32(spec[c][k] / curves[c][k])
		}
	}
	// Kopplung
	for _, st := range e.mp.coupling {
		mg, an := res[st[0]], res[st[1]]
		for k := 0; k < m; k++ {
			a, b := mg[k], an[k]
			var mag, ang float32
			aa, ab := a, b
			if aa < 0 {
				aa = -aa
			}
			if ab < 0 {
				ab = -ab
			}
			if aa > ab {
				mag = a
				if a > 0 {
					ang = a - b
				} else {
					ang = b - a
				}
			} else {
				mag = b
				if b > 0 {
					ang = a - b
				} else {
					ang = b - a
				}
			}
			mg[k], an[k] = mag, ang
		}
	}
	// Residue je Submap
	for sm := 0; sm < e.mp.submaps; sm++ {
		rn := e.mp.resNum[sm]
		r := s.residues[rn]
		var chans []int
		for c := 0; c < C; c++ {
			if e.submapOf[c] == sm {
				chans = append(chans, c)
			}
		}
		var vecs [][]float32
		if r.typ == 2 {
			iv := make([]float32, m*len(chans))
			for k := 0; k < m; k++ {
				for ci, c := range chans {
					iv[k*len(chans)+ci] = res[c][k]
				}
			}
			vecs = [][]float32{iv}
		} else {
			for _, c := range chans {
				vecs = append(vecs, res[c])
			}
		}
		if err := r.encode(bw, s.books, vecs, e.orders[rn], e.maxRs[rn]); err != nil {
			return nil, err
		}
	}
	out := bw.bytes()
	if len(out) > 65000 {
		return nil, errors.New("Paket zu gross")
	}
	return out, nil
}

type blockScratch struct {
	x      []float64
	spec   [][]float64
	curves [][]float64
	res    [][]float32
}

func (e *vencoder) newScratch() *blockScratch {
	C := e.s.channels
	sc := &blockScratch{x: make([]float64, e.n)}
	for c := 0; c < C; c++ {
		sc.spec = append(sc.spec, make([]float64, e.m))
		sc.curves = append(sc.curves, make([]float64, e.m))
		sc.res = append(sc.res, make([]float32, e.m))
	}
	return sc
}

// encodeBlocks codiert die angegebenen Bloecke parallel.
func (e *vencoder) encodeBlocks(idx []int, gamma float64) ([][]byte, error) {
	out := make([][]byte, len(idx))
	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	next := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm := newMDCT(e.n)
			sc := e.newScratch()
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= len(idx) {
					return
				}
				b, err := e.encodeBlock(idx[i], gamma, tm, sc)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				out[i] = b
			}
		}()
	}
	wg.Wait()
	return out, firstErr
}

// estimateBytes schaetzt die Gesamtgroesse der Pakete fuer gamma anhand einer Stichprobe.
func (e *vencoder) estimateBytes(gamma float64) (float64, error) {
	k := 48
	if e.nblocks < k {
		k = e.nblocks
	}
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i * (e.nblocks - 1) / maxInt(k-1, 1)
	}
	pk, err := e.encodeBlocks(idx, gamma)
	if err != nil {
		return 0, err
	}
	tot := 0
	for _, p := range pk {
		tot += len(p) + 2
	}
	return float64(tot) / float64(k) * float64(e.nblocks), nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var encMu = 0.06
