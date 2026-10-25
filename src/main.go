package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed index.html
var indexHTML []byte

const (
	bankPath = `base\sound\soundbanks\cp_music.bnk`
	modName  = "zzzz_MenuMusic.archive"
)

func mediaPath(id uint32) string { return fmt.Sprintf(`base\sound\soundbanks\media\%d.wem`, id) }

type MenuState struct {
	Src    string  `json:"src"`
	DurSec float64 `json:"durSec"`
	Time   string  `json:"time"`
}

type App struct {
	mu       sync.Mutex
	game     string
	busy     bool
	progress string
	tmp      string
}

var app = &App{}

func validGame(p string) string {
	p = strings.TrimSpace(strings.Trim(p, `"`))
	if p == "" {
		return ""
	}
	for _, c := range []string{p, filepath.Dir(p), filepath.Join(p, "Cyberpunk 2077")} {
		if _, err := os.Stat(filepath.Join(c, "archive", "pc", "content", "audio_2_soundbanks.archive")); err == nil {
			return c
		}
	}
	return ""
}

func findGame() string {
	exe, _ := os.Executable()
	if c := validGame(filepath.Dir(exe)); c != "" {
		return c
	}
	var roots []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if b := os.Getenv(env); b != "" {
			roots = append(roots, filepath.Join(b, "Steam"), filepath.Join(b, "GOG Galaxy", "Games"), filepath.Join(b, "Epic Games"))
		}
	}
	for _, r := range roots {
		if b, err := os.ReadFile(filepath.Join(r, "steamapps", "libraryfolders.vdf")); err == nil {
			for _, ln := range strings.Split(string(b), "\n") {
				ln = strings.TrimSpace(ln)
				if strings.HasPrefix(ln, `"path"`) {
					if parts := strings.Split(ln, `"`); len(parts) >= 4 {
						roots = append(roots, filepath.Join(strings.ReplaceAll(parts[3], `\\`, `\`), "steamapps", "common"))
					}
				}
			}
		}
		roots = append(roots, filepath.Join(r, "steamapps", "common"))
	}
	for d := 'C'; d <= 'Z'; d++ {
		for _, sub := range []string{`SteamLibrary\steamapps\common`, `Steam\steamapps\common`, `Games`, `GOG Games`, `Epic Games`} {
			roots = append(roots, fmt.Sprintf(`%c:\%s`, d, sub))
		}
	}
	for _, r := range roots {
		for _, n := range []string{"Cyberpunk 2077", "Cyberpunk2077"} {
			if c := validGame(filepath.Join(r, n)); c != "" {
				return c
			}
		}
	}
	return ""
}

func (a *App) modFile() string { return filepath.Join(a.game, "archive", "pc", "mod", modName) }
func (a *App) statePath() string {
	return filepath.Join(a.game, "archive", "pc", "mod", modName+".info.json")
}

func (a *App) loadState() (MenuState, bool) {
	var s MenuState
	if _, err := os.Stat(a.modFile()); err != nil {
		return s, false
	}
	if b, err := os.ReadFile(a.statePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s, true
}

func (a *App) setProgress(s string) {
	a.mu.Lock()
	a.progress = s
	a.mu.Unlock()
	logLine("%s", s)
}

func (a *App) baseArchive() (*arch, error) {
	return loadArchive(filepath.Join(a.game, "archive", "pc", "content", "audio_2_soundbanks.archive"))
}

func readFrom(ar *arch, path string) ([]byte, error) {
	i, ok := ar.index[fnv64(path)]
	if !ok {
		return nil, fmt.Errorf("%s not found in archive", path)
	}
	return ar.read(ar.ents[i])
}

// conflicts: andere Mod-Archive, die dieselben Dateien ersetzen.
func (a *App) conflicts() []string {
	var out []string
	want := []uint64{fnv64(bankPath)}
	want = append(want, fnv64(mediaPath(newSource)))
	m, _ := filepath.Glob(filepath.Join(a.game, "archive", "pc", "mod", "*.archive"))
	for _, p := range m {
		if strings.EqualFold(filepath.Base(p), modName) {
			continue
		}
		ar, err := loadArchive(p)
		if err != nil {
			continue
		}
		for _, h := range want {
			if _, ok := ar.index[h]; ok {
				out = append(out, filepath.Base(p))
				break
			}
		}
		ar.Close()
	}
	return out
}

func (a *App) apply(filename string, src []byte) (MenuState, error) {
	if a.game == "" {
		return MenuState{}, errors.New("Game folder not set")
	}
	if err := initOodle(a.game); err != nil {
		return MenuState{}, fmt.Errorf("Oodle library could not be loaded: %v", err)
	}
	a.setProgress("Reading the original sound bank …")
	ar, err := a.baseArchive()
	if err != nil {
		return MenuState{}, err
	}
	defer ar.Close()
	bank, err := readFrom(ar, bankPath)
	if err != nil {
		return MenuState{}, err
	}
	if _, err := analyzeMenu(bank); err != nil {
		return MenuState{}, fmt.Errorf("Unexpected game data: %v", err)
	}
	tpl, err := readFrom(ar, mediaPath(1040680541))
	if err != nil {
		return MenuState{}, err
	}
	t, err := parseWemTemplate(tpl)
	if err != nil {
		return MenuState{}, fmt.Errorf("Original audio stream unreadable: %w", err)
	}
	a.setProgress("Reading music …")
	p, err := decodeAudio(filename, src)
	if err != nil {
		return MenuState{}, err
	}
	edgeFade(p, 0.02)
	p = remapChannels(p, t.channels)
	p = resample(p, t.rate)
	fr := p.Frames()
	if fr < 2048 {
		return MenuState{}, errors.New("The music file is too short")
	}
	pcm := make([][]float64, t.channels)
	for c := range pcm {
		pcm[c] = make([]float64, fr)
		for i := 0; i < fr; i++ {
			pcm[c][i] = float64(p.Data[i*t.channels+c])
		}
	}
	secs := float64(fr) / float64(t.rate)
	a.setProgress(fmt.Sprintf("Converting %d:%02d of music to the game format – this can take 1–3 minutes …", int(secs)/60, int(secs)%60))
	budget := int64(secs * 36000) // ca. 288 kbit/s
	if budget < 1<<20 {
		budget = 1 << 20
	}
	wem, note, err := encodeWem(t, pcm, budget, func(s string) { logLine("%s", s) })
	if err != nil {
		return MenuState{}, err
	}
	durMs := secs*1000 - 20
	a.setProgress("Adjusting length and loop …")
	nb, _, err := patchMenu(bank, durMs)
	if err != nil {
		return MenuState{}, err
	}
	files := []modFile{{fnv64(bankPath), nb}}
	files = append(files, modFile{fnv64(mediaPath(newSource)), wem})
	a.setProgress("Writing the mod file …")
	if err := os.MkdirAll(filepath.Dir(a.modFile()), 0o755); err != nil {
		return MenuState{}, err
	}
	if err := writeArchive(a.modFile(), files); err != nil {
		return MenuState{}, fmt.Errorf("Could not write the mod file: %v", err)
	}
	ms := MenuState{Src: filepath.Base(filename), DurSec: durMs / 1000, Time: nowStr()}
	b, _ := json.MarshalIndent(ms, "", " ")
	_ = os.WriteFile(a.statePath(), b, 0o644)
	logLine("fertig: %s", note)
	a.setProgress("Done.")
	return ms, nil
}

func (a *App) restore() error {
	for _, f := range []string{a.modFile(), a.statePath()} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func edgeFade(p *PCM, sec float64) {
	n := int(sec * float64(p.Rate))
	fr := p.Frames()
	if n*2 > fr {
		return
	}
	for i := 0; i < n; i++ {
		g := float32(i) / float32(n)
		for c := 0; c < p.Ch; c++ {
			p.Data[i*p.Ch+c] *= g
			p.Data[(fr-1-i)*p.Ch+c] *= g
		}
	}
}

// Originale Startversatz (BeginTrim) und Laenge der drei Menue-Titel in Sekunden.
var origTracks = []struct {
	Label string
	Start float64
	Dur   float64
}{{"Track 1", 0.667, 154.1}, {"Track 2", 0, 359.0}, {"Track 3", 0, 104.5}}

func (a *App) currentWAV(v int) ([]byte, float64, error) {
	if v < 0 || v >= len(menuSources) {
		return nil, 0, errors.New("unknown track")
	}
	path := mediaPath(menuSources[v])
	start := origTracks[v].Start
	var raw []byte
	if _, ok := a.loadState(); ok {
		ar, err := loadArchive(a.modFile())
		if err != nil {
			return nil, 0, err
		}
		defer ar.Close()
		raw, err = readFrom(ar, mediaPath(newSource))
		if err != nil {
			return nil, 0, err
		}
		start = 0
	} else {
		ar, err := a.baseArchive()
		if err != nil {
			return nil, 0, err
		}
		defer ar.Close()
		raw, err = readFrom(ar, path)
		if err != nil {
			return nil, 0, err
		}
	}
	w, err := wemToWAV(raw, app.tmp)
	return w, start, err
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func main() {
	app.tmp, _ = os.MkdirTemp("", "cpmenu")
	defer os.RemoveAll(app.tmp)
	if len(os.Args) > 1 {
		app.game = validGame(os.Args[1])
	}
	if app.game == "" {
		app.game = findGame()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		out := map[string]any{"cooked": app.game, "ready": app.game != "", "busy": app.busy, "progress": app.progress, "tracks": origTracks}
		g := app.game
		app.mu.Unlock()
		if g != "" {
			ms, ok := app.loadState()
			out["custom"] = ok
			out["src"], out["durSec"] = ms.Src, ms.DurSec
		}
		jsonOut(w, out)
	})
	mux.HandleFunc("/api/conflicts", func(w http.ResponseWriter, r *http.Request) {
		if app.game == "" {
			jsonOut(w, []string{})
			return
		}
		c := app.conflicts()
		if c == nil {
			c = []string{}
		}
		jsonOut(w, c)
	})
	mux.HandleFunc("/api/setdir", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Path string }
		json.NewDecoder(r.Body).Decode(&in)
		c := validGame(in.Path)
		if c == "" {
			fail(w, 400, "Cyberpunk 2077 was not found there (folder with “archive” and “bin”).")
			return
		}
		app.mu.Lock()
		app.game = c
		app.mu.Unlock()
		jsonOut(w, map[string]string{"ok": "1"})
	})
	mux.HandleFunc("/api/apply", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		if app.busy {
			app.mu.Unlock()
			fail(w, 409, "An operation is already running.")
			return
		}
		app.busy = true
		app.mu.Unlock()
		defer func() { app.mu.Lock(); app.busy = false; app.mu.Unlock() }()
		r.ParseMultipartForm(1 << 30)
		file, hdr, err := r.FormFile("file")
		if err != nil {
			fail(w, 400, "No file received")
			return
		}
		defer file.Close()
		src, _ := io.ReadAll(file)
		ms, err := app.apply(hdr.Filename, src)
		if err != nil {
			logLine("FEHLER: %v", err)
			fail(w, 400, err.Error())
			return
		}
		jsonOut(w, ms)
	})
	mux.HandleFunc("/api/current", func(w http.ResponseWriter, r *http.Request) {
		v := 0
		fmt.Sscan(r.URL.Query().Get("v"), &v)
		b, start, err := app.currentWAV(v)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Access-Control-Expose-Headers", "X-Start")
		w.Header().Set("X-Start", fmt.Sprintf("%.3f", start))
		w.Write(b)
	})
	mux.HandleFunc("/api/banner.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		if exe, err := os.Executable(); err == nil {
			if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "banner.jpg")); err == nil {
				w.Write(b)
				return
			}
		}
		w.Write(bannerJPG)
	})
	mux.HandleFunc("/api/restore", func(w http.ResponseWriter, r *http.Request) {
		if err := app.restore(); err != nil {
			fail(w, 500, err.Error())
			return
		}
		jsonOut(w, map[string]string{"ok": "1"})
	})
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		lastPing.Store(time.Now().UnixNano())
		jsonOut(w, map[string]string{"ok": "1"})
	})
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]string{"ok": "1"})
		go os.Exit(0)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("Port error:", err)
		os.Exit(1)
	}
	url := "http://" + ln.Addr().String() + "/"
	logLine("Server: %s, Spiel: %q", url, app.game)
	fmt.Println("Cyberpunk 2077 Menu Music running:", url)
	if runtime.GOOS == "windows" {
		go http.Serve(ln, mux)
		runtime.LockOSThread()
		if runNativeWindow(url) {
			os.Exit(0)
		}
		openAppWindow(url)
	}
	http.Serve(ln, mux)
}

var lastPing atomic.Int64

func openAppWindow(url string) {
	openURL(url)
	// Erste Pings koennen ein paar Sekunden brauchen (Browserstart) - erst danach ueberwachen.
	lastPing.Store(time.Now().UnixNano())
	go func() {
		for {
			time.Sleep(1 * time.Second)
			if time.Since(time.Unix(0, lastPing.Load())) > 8*time.Second {
				os.Exit(0)
			}
		}
	}()
}

var logf *os.File
var logMu sync.Mutex

func logLine(format string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if logf == nil {
		exe, _ := os.Executable()
		logf, _ = os.Create(filepath.Join(filepath.Dir(exe), "cp_menumusic_log.txt"))
	}
	if logf != nil {
		fmt.Fprintf(logf, time.Now().Format("15:04:05 ")+format+"\n", a...)
		logf.Sync()
	}
}

func webviewDataPath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "ArkhamMenuMusic", "webview")
}
