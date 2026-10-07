// Package qrcode generates event-link QR images without an external service.
// The matrix and Reed-Solomon algorithms are adapted from Project Nayuki's
// MIT-licensed QR Code generator; see LICENSE and README.md in this directory.
package qrcode

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

const (
	version       = 10
	modules       = 57
	quietZone     = 4
	dataCodewords = 216
	eccCodewords  = 26
	// MaxBytes is the byte-mode capacity of a version 10, medium-ECC symbol.
	MaxBytes = 213
)

// EncodePNG encodes text in a version 10 QR Code with medium error correction.
// It includes the required four-module quiet zone and uses whole-pixel modules.
// Text may contain up to MaxBytes UTF-8 bytes; size must be between 130 and 4096.
func EncodePNG(text string, size int) ([]byte, error) {
	if len(text) > MaxBytes {
		return nil, fmt.Errorf("QR text exceeds %d bytes", MaxBytes)
	}
	if size < 2*(modules+2*quietZone) || size > 4096 {
		return nil, fmt.Errorf("QR image size must be between 130 and 4096 pixels")
	}

	code := encode(text)
	img := image.NewGray(image.Rect(0, 0, size, size))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	scale := size / (modules + 2*quietZone)
	offset := (size - modules*scale) / 2
	for y := 0; y < modules; y++ {
		for x := 0; x < modules; x++ {
			if code.cells[y][x] {
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.SetGray(offset+x*scale+dx, offset+y*scale+dy, color.Gray{})
					}
				}
			}
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type symbol struct {
	cells     [modules][modules]bool
	functions [modules][modules]bool
}

func encode(text string) symbol {
	var data [dataCodewords]byte
	bitCount := 0
	appendBits := func(value, count int) {
		for i := count - 1; i >= 0; i-- {
			data[bitCount/8] |= byte((value>>i)&1) << (7 - bitCount%8)
			bitCount++
		}
	}
	appendBits(4, 4) // Byte mode.
	appendBits(len(text), 16)
	for _, b := range []byte(text) {
		appendBits(int(b), 8)
	}
	appendBits(0, min(4, dataCodewords*8-bitCount))
	appendBits(0, (8-bitCount%8)%8)
	for pad := byte(0xEC); bitCount < dataCodewords*8; pad ^= 0xEC ^ 0x11 {
		appendBits(int(pad), 8)
	}

	var code symbol
	code.drawFunctions()
	code.drawData(interleave(data[:]))
	best, bestScore := code, int(^uint(0)>>1)
	for mask := 0; mask < 8; mask++ {
		candidate := code
		candidate.applyMask(mask)
		candidate.drawFormat(mask)
		if score := candidate.penalty(); score < bestScore {
			best, bestScore = candidate, score
		}
	}
	return best
}

func (s *symbol) setFunction(x, y int, dark bool) {
	s.cells[y][x], s.functions[y][x] = dark, true
}

func (s *symbol) drawFunctions() {
	for i := 0; i < modules; i++ {
		s.setFunction(6, i, i%2 == 0)
		s.setFunction(i, 6, i%2 == 0)
	}
	for _, center := range [][2]int{{3, 3}, {modules - 4, 3}, {3, modules - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := center[0]+dx, center[1]+dy
				if x >= 0 && y >= 0 && x < modules && y < modules {
					distance := max(abs(dx), abs(dy))
					s.setFunction(x, y, distance != 2 && distance != 4)
				}
			}
		}
	}
	positions := [...]int{6, 28, 50}
	for i, x := range positions {
		for j, y := range positions {
			if (i == 0 && j == 0) || (i == 0 && j == 2) || (i == 2 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					s.setFunction(x+dx, y+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	s.drawFormat(0)
	remainder := version
	for i := 0; i < 12; i++ {
		remainder = (remainder << 1) ^ ((remainder >> 11) * 0x1F25)
	}
	bits := version<<12 | remainder
	for i := 0; i < 18; i++ {
		x, y := modules-11+i%3, i/3
		s.setFunction(x, y, bit(bits, i))
		s.setFunction(y, x, bit(bits, i))
	}
}

func (s *symbol) drawFormat(mask int) {
	// Medium ECC has format value 00; the remaining three bits select the mask.
	remainder := mask
	for i := 0; i < 10; i++ {
		remainder = (remainder << 1) ^ ((remainder >> 9) * 0x537)
	}
	bits := (mask<<10 | remainder) ^ 0x5412
	for i := 0; i < 6; i++ {
		s.setFunction(8, i, bit(bits, i))
	}
	s.setFunction(8, 7, bit(bits, 6))
	s.setFunction(8, 8, bit(bits, 7))
	s.setFunction(7, 8, bit(bits, 8))
	for i := 9; i < 15; i++ {
		s.setFunction(14-i, 8, bit(bits, i))
	}
	for i := 0; i < 8; i++ {
		s.setFunction(modules-1-i, 8, bit(bits, i))
	}
	for i := 8; i < 15; i++ {
		s.setFunction(8, modules-15+i, bit(bits, i))
	}
	s.setFunction(8, modules-8, true)
}

func (s *symbol) drawData(data []byte) {
	i := 0
	for right := modules - 1; right >= 1; right -= 2 {
		if right == 6 {
			right--
		}
		for vertical := 0; vertical < modules; vertical++ {
			y := vertical
			if (right+1)&2 == 0 {
				y = modules - 1 - vertical
			}
			for j := 0; j < 2; j++ {
				x := right - j
				if !s.functions[y][x] && i < len(data)*8 {
					s.cells[y][x] = bit(int(data[i/8]), 7-i%8)
					i++
				}
			}
		}
	}
}

func (s *symbol) applyMask(mask int) {
	for y := 0; y < modules; y++ {
		for x := 0; x < modules; x++ {
			invert := false
			switch mask {
			case 0:
				invert = (x+y)%2 == 0
			case 1:
				invert = y%2 == 0
			case 2:
				invert = x%3 == 0
			case 3:
				invert = (x+y)%3 == 0
			case 4:
				invert = (x/3+y/2)%2 == 0
			case 5:
				invert = x*y%2+x*y%3 == 0
			case 6:
				invert = (x*y%2+x*y%3)%2 == 0
			case 7:
				invert = ((x+y)%2+x*y%3)%2 == 0
			}
			if invert && !s.functions[y][x] {
				s.cells[y][x] = !s.cells[y][x]
			}
		}
	}
}

func (s *symbol) penalty() int {
	score, dark := 0, 0
	for y := 0; y < modules; y++ {
		for x := 0; x < modules; x++ {
			if s.cells[y][x] {
				dark++
			}
			if x > 0 && y > 0 && s.cells[y][x] == s.cells[y-1][x] &&
				s.cells[y][x] == s.cells[y][x-1] && s.cells[y][x] == s.cells[y-1][x-1] {
				score += 3
			}
		}
		score += linePenalty(s.cells[y][:])
		var column [modules]bool
		for x := 0; x < modules; x++ {
			column[x] = s.cells[x][y]
		}
		score += linePenalty(column[:])
	}
	score += abs(dark*20-modules*modules*10) / (modules * modules) * 10
	return score
}

func linePenalty(line []bool) int {
	score, run := 0, 1
	for i := 1; i < len(line); i++ {
		if line[i] == line[i-1] {
			run++
			if run == 5 {
				score += 3
			} else if run > 5 {
				score++
			}
		} else {
			run = 1
		}
	}
	// Finder-like patterns require four light modules before or after 1011101.
	for i := 0; i+7 <= len(line); i++ {
		if !line[i] || line[i+1] || !line[i+2] || !line[i+3] || !line[i+4] || line[i+5] || !line[i+6] {
			continue
		}
		before, after := true, true
		for j := 1; j <= 4; j++ {
			before = before && (i-j < 0 || !line[i-j])
			after = after && (i+6+j >= len(line) || !line[i+6+j])
		}
		if before {
			score += 40
		}
		if after {
			score += 40
		}
	}
	return score
}

func interleave(data []byte) []byte {
	// Version 10/M uses four 43-byte blocks and one 44-byte block, each with
	// 26 parity codewords. Data codewords precede interleaved parity codewords.
	var blocks [5][]byte
	var ecc [5][]byte
	divisor := reedSolomonDivisor(eccCodewords)
	offset := 0
	for i := range blocks {
		length := 43
		if i == 4 {
			length++
		}
		blocks[i] = data[offset : offset+length]
		ecc[i] = reedSolomonRemainder(blocks[i], divisor)
		offset += length
	}
	result := make([]byte, 0, 346)
	for i := 0; i < 44; i++ {
		for _, block := range blocks {
			if i < len(block) {
				result = append(result, block[i])
			}
		}
	}
	for i := 0; i < eccCodewords; i++ {
		for _, block := range ecc {
			result = append(result, block[i])
		}
	}
	return result
}

func reedSolomonDivisor(degree int) []byte {
	result := make([]byte, degree)
	result[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range result {
			result[j] = multiply(result[j], root)
			if j+1 < degree {
				result[j] ^= result[j+1]
			}
		}
		root = multiply(root, 2)
	}
	return result
}

func reedSolomonRemainder(data, divisor []byte) []byte {
	result := make([]byte, len(divisor))
	for _, b := range data {
		factor := b ^ result[0]
		copy(result, result[1:])
		result[len(result)-1] = 0
		for i, coefficient := range divisor {
			result[i] ^= multiply(coefficient, factor)
		}
	}
	return result
}

func multiply(x, y byte) byte {
	var result uint16
	for i := 7; i >= 0; i-- {
		result = (result << 1) ^ ((result >> 7) * 0x11D)
		result ^= uint16((y>>i)&1) * uint16(x)
	}
	return byte(result)
}

func bit(value, position int) bool { return (value>>position)&1 != 0 }
func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
