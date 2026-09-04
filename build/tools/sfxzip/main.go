package main

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
)

const (
	localSignature   = 0x04034b50
	centralSignature = 0x02014b50
	endSignature     = 0x06054b50
)

func main() {
	stubPath := flag.String("stub", "", "Windows executable stub")
	zipPath := flag.String("zip", "", "ZIP payload")
	outputPath := flag.String("output", "", "self-extracting output")
	flag.Parse()
	if *stubPath == "" || *zipPath == "" || *outputPath == "" {
		fatal(errors.New("-stub, -zip, and -output are required"))
	}
	stub, err := os.ReadFile(*stubPath)
	if err != nil {
		fatal(err)
	}
	stub = stripEmbeddedZip(stub)
	payload, err := os.ReadFile(*zipPath)
	if err != nil {
		fatal(err)
	}
	adjusted, err := adjustOffsets(payload, uint32(len(stub)))
	if err != nil {
		fatal(err)
	}
	combined := make([]byte, 0, len(stub)+len(adjusted))
	combined = append(combined, stub...)
	combined = append(combined, adjusted...)
	if err := os.WriteFile(*outputPath, combined, 0755); err != nil {
		fatal(err)
	}
	archive, err := zip.OpenReader(*outputPath)
	if err != nil {
		fatal(fmt.Errorf("verify self-extracting ZIP: %w", err))
	}
	entries := len(archive.File)
	archive.Close()
	if entries == 0 {
		fatal(errors.New("verify self-extracting ZIP: payload has no entries"))
	}
	fmt.Printf("embedded %d ZIP entries in %s\n", entries, *outputPath)
}

// stripEmbeddedZip makes packaging idempotent when the output from a previous
// run is reused as the input stub. A valid trailing ZIP records absolute local
// offsets, so the first one is also the exact end of the executable prefix.
func stripEmbeddedZip(data []byte) []byte {
	end := findEndRecord(data)
	if end < 0 || end+22 > len(data) {
		return data
	}
	commentLength := int(binary.LittleEndian.Uint16(data[end+20 : end+22]))
	if end+22+commentLength != len(data) {
		return data
	}
	entryCount := int(binary.LittleEndian.Uint16(data[end+10 : end+12]))
	position := int(binary.LittleEndian.Uint32(data[end+16 : end+20]))
	if entryCount == 0 || position < 0 || position >= end {
		return data
	}
	firstLocal := len(data)
	for entry := 0; entry < entryCount; entry++ {
		if position+46 > end || binary.LittleEndian.Uint32(data[position:position+4]) != centralSignature {
			return data
		}
		localOffset := int(binary.LittleEndian.Uint32(data[position+42 : position+46]))
		if localOffset < firstLocal {
			firstLocal = localOffset
		}
		nameLength := int(binary.LittleEndian.Uint16(data[position+28 : position+30]))
		extraLength := int(binary.LittleEndian.Uint16(data[position+30 : position+32]))
		commentLength := int(binary.LittleEndian.Uint16(data[position+32 : position+34]))
		position += 46 + nameLength + extraLength + commentLength
	}
	if firstLocal < 0 || firstLocal+4 > len(data) || binary.LittleEndian.Uint32(data[firstLocal:firstLocal+4]) != localSignature {
		return data
	}
	return data[:firstLocal]
}

func adjustOffsets(payload []byte, prefix uint32) ([]byte, error) {
	if uint64(len(payload))+uint64(prefix) > uint64(^uint32(0)) {
		return nil, errors.New("ZIP64 payloads are not supported")
	}
	result := append([]byte(nil), payload...)
	end := findEndRecord(result)
	if end < 0 || end+22 > len(result) {
		return nil, errors.New("ZIP end-of-central-directory record not found")
	}
	entryCount := int(binary.LittleEndian.Uint16(result[end+10 : end+12]))
	centralOffset := binary.LittleEndian.Uint32(result[end+16 : end+20])
	position := int(centralOffset)
	for entry := 0; entry < entryCount; entry++ {
		if position+46 > len(result) || binary.LittleEndian.Uint32(result[position:position+4]) != centralSignature {
			return nil, fmt.Errorf("invalid central-directory entry %d", entry)
		}
		localOffset := binary.LittleEndian.Uint32(result[position+42 : position+46])
		binary.LittleEndian.PutUint32(result[position+42:position+46], localOffset+prefix)
		nameLength := int(binary.LittleEndian.Uint16(result[position+28 : position+30]))
		extraLength := int(binary.LittleEndian.Uint16(result[position+30 : position+32]))
		commentLength := int(binary.LittleEndian.Uint16(result[position+32 : position+34]))
		position += 46 + nameLength + extraLength + commentLength
	}
	binary.LittleEndian.PutUint32(result[end+16:end+20], centralOffset+prefix)
	return result, nil
}

func findEndRecord(data []byte) int {
	start := len(data) - 22
	minimum := len(data) - (65535 + 22)
	if minimum < 0 {
		minimum = 0
	}
	for position := start; position >= minimum; position-- {
		if binary.LittleEndian.Uint32(data[position:position+4]) == endSignature {
			return position
		}
	}
	return -1
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
