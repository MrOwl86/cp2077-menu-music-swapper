package main

import (
	_ "embed"
)

//go:embed banner.jpg
var bannerJPG []byte

// wemToWAV dekodiert ein Wwise-Vorbis-WEM direkt in Go (kein Hilfsprogramm, kein
// Prozessstart - siehe vdecode.go) zu WAV, damit die Vorhoer-Funktion ohne das
// Antivirus-Risiko von frueheren Builds funktioniert.
func wemToWAV(raw []byte, tmp string) ([]byte, error) {
	pcm, err := decodeWemToPCM(raw)
	if err != nil {
		return nil, err
	}
	return buildPCMWem(pcm, 0), nil
}
