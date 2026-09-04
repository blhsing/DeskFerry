package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"testing"
)

func TestStripEmbeddedZip(t *testing.T) {
	prefix := []byte("executable-prefix")
	var payload bytes.Buffer
	writer := zip.NewWriter(&payload)
	entry, err := writer.Create("payload.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	adjusted, err := adjustOffsets(payload.Bytes(), uint32(len(prefix)))
	if err != nil {
		t.Fatal(err)
	}
	combined := append(append([]byte(nil), prefix...), adjusted...)
	if got := stripEmbeddedZip(combined); !bytes.Equal(got, prefix) {
		t.Fatalf("stripped prefix = %q, want %q", got, prefix)
	}
	if got := stripEmbeddedZip(prefix); !bytes.Equal(got, prefix) {
		t.Fatalf("plain executable changed: %q", got)
	}
}

func TestStripEmbeddedZipRejectsUnrelatedEndSignature(t *testing.T) {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[10:14], endSignature)
	if got := stripEmbeddedZip(data); !bytes.Equal(got, data) {
		t.Fatal("invalid trailing record was stripped")
	}
}
