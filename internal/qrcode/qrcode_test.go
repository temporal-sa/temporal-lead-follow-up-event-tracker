package qrcode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPNGDimensionsAndQuietZone(t *testing.T) {
	for _, size := range []int{130, 195, 512, 768} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data, err := EncodePNG("https://leads.example.com/events/01234567-89ab-cdef-0123-456789abcdef", size)
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
				t.Fatalf("image dimensions: %v", img.Bounds())
			}
			border := 4 * (size / 65)
			darkPixels := 0
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					r, g, b, a := img.At(x, y).RGBA()
					white := r == 65535 && g == 65535 && b == 65535 && a == 65535
					if (x < border || y < border || x >= size-border || y >= size-border) && !white {
						t.Fatalf("quiet zone is not white at %d,%d", x, y)
					}
					if !white {
						darkPixels++
					}
				}
			}
			if darkPixels == 0 {
				t.Fatal("image contains no dark modules")
			}
		})
	}
}

func TestCapacityAndSizeErrors(t *testing.T) {
	if _, err := EncodePNG(strings.Repeat("x", MaxBytes), 512); err != nil {
		t.Fatalf("maximum capacity rejected: %v", err)
	}
	if _, err := EncodePNG(strings.Repeat("x", MaxBytes+1), 512); err == nil {
		t.Fatal("over-capacity input accepted")
	}
	// Capacity counts bytes, rather than Unicode code points.
	if _, err := EncodePNG(strings.Repeat("é", 107), 512); err == nil {
		t.Fatal("over-capacity UTF-8 input accepted")
	}
	for _, size := range []int{-1, 0, 129, 4097} {
		if _, err := EncodePNG("https://example.com", size); err == nil {
			t.Fatalf("invalid size %d accepted", size)
		}
	}
}

// Apple's reader is independent, so this verifies the encoded payload,
// parity, matrix layout, masking, and rendering together rather than duplicating
// the encoder in a test. The production package has no platform dependencies.
func TestIndependentAppleDecoder(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("independent Apple decoder is available on macOS")
	}
	compiler, err := exec.LookPath("swiftc")
	if err != nil {
		t.Skip("Swift command-line tools are not installed")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "decode-qr")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, compiler, "-module-cache-path", filepath.Join(dir, "swift-cache"), "testdata/decode.swift", "-o", binary)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile independent decoder: %v\n%s", err, output)
	}
	for _, test := range []struct {
		name, text string
		size       int
	}{
		{"minimum image size", "https://leads.example.com/events/3d78cabe-7cba-4ab8-91fe-4b34133c4cfe", 130},
		{"mobile image", "https://leads.tmprl-demo.cloud/events/3d78cabe-7cba-4ab8-91fe-4b34133c4cfe", 512},
		{"desktop image", "https://leads.tmprl-demo.cloud/events/3d78cabe-7cba-4ab8-91fe-4b34133c4cfe", 768},
		{"maximum capacity", "https://example.com/events/" + strings.Repeat("a", MaxBytes-len("https://example.com/events/")), 512},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := EncodePNG(test.text, test.size)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "qr.png")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(ctx, binary, path)
			var stderr bytes.Buffer
			command.Stderr = &stderr
			output, err := command.Output()
			if err != nil {
				t.Fatalf("decode PNG: %v\n%s", err, stderr.Bytes())
			}
			var decoded struct {
				Available bool     `json:"available"`
				Payloads  []string `json:"payloads"`
			}
			if err := json.Unmarshal(output, &decoded); err != nil {
				t.Fatalf("decoder output: %v\n%s", err, output)
			}
			if !decoded.Available {
				t.Skipf("Apple decoder cannot decode its own reference image in this environment: %s", stderr.Bytes())
			}
			if len(decoded.Payloads) != 1 || decoded.Payloads[0] != test.text {
				t.Fatalf("decoded %q; expected %q", decoded.Payloads, test.text)
			}
		})
	}
}

func TestIndependentEncoderGoldenVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Text   string   `json:"text"`
		Hashes []string `json:"hashes"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		code := encode(vector.Text)
		matrix := make([]byte, 0, modules*modules)
		for _, row := range code.cells {
			for _, dark := range row {
				value := byte('0')
				if dark {
					value = '1'
				}
				matrix = append(matrix, value)
			}
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(matrix))
		matched := false
		for _, expected := range vector.Hashes {
			matched = matched || hash == expected
		}
		if !matched {
			t.Errorf("%d-byte payload matrix %s differs from independent encoder", len(vector.Text), hash)
		}
	}
}
