# Cyberpunk 2077 – Menu Music Swapper

A small Windows tool that lets you replace the music that plays on the Cyberpunk 2077 main menu with a track of your own choosing — no manual archive editing required.

## Download

Grab the latest build from the **[Releases](../../releases)** page: `CP2077MenuMusicSwapper_v2.exe`.

No installer, no admin rights needed. Just run the `.exe`.

> **Note on antivirus warnings:** Some antivirus engines flag small, unsigned Go executables that patch game files as suspicious (a well-known false-positive pattern for homemade modding tools). The source code in this repository is exactly what was built into the release — you're welcome to read it, or build it yourself with Go (see below), to confirm there's nothing malicious going on.

## What it does

- Opens a simple web UI in your browser (no bundled browser engine, no extra processes).
- Lets you pick an audio file (mp3, wav, etc.) from your computer.
- Converts and injects it into the game's sound banks so it plays as the main menu music.
- Lets you preview the **currently active** menu music before you change anything.
- Can restore the original music at any time.

## How to use

1. Download and run `CP2077MenuMusicSwapper_v2.exe`.
2. A browser tab opens automatically with the tool's interface.
3. Point it at your Cyberpunk 2077 installation folder if it isn't auto-detected.
4. Pick your audio file and apply it.
5. Close the browser tab (or the window) when you're done — the background process shuts down automatically.

## Building from source

Requires [Go](https://go.dev/) 1.24+.

```bash
cd src
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o CP2077MenuMusicSwapper_v2.exe .
```

The audio decode/encode pipeline (Wwise/Vorbis codec handling) is implemented from scratch in pure Go — no external decoder binaries are embedded or launched at runtime.

## License / disclaimer

Unofficial fan tool, not affiliated with CD Projekt RED. Use at your own risk; always keep a backup of your game files before modifying them.
