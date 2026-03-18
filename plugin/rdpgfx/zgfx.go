package rdpgfx

// ZGFX (RDP8 Bulk Compression) decompressor.
// Implements the decompression algorithm described in MS-RDPEGFX 2.2.4 / 3.3.8.

const zgfxHistorySize = 2500000

type zgfxContext struct {
	history    []byte
	historyIdx int
}

func newZgfxContext() *zgfxContext {
	return &zgfxContext{
		history: make([]byte, zgfxHistorySize),
	}
}

// Huffman token types
const (
	tokenLiteral = 0
	tokenMatch   = 1
)

type zgfxToken struct {
	prefixLen  uint8
	prefixCode uint16
	valueBits  uint8
	tokenType  uint8
	valueBase  uint32
}

// Fixed Huffman table (MS-RDPEGFX 3.3.8.2, derived from FreeRDP zgfx.c)
var zgfxTokenTable = []zgfxToken{
	{1, 0, 8, tokenLiteral, 0},
	{5, 17, 5, tokenMatch, 0},
	{5, 18, 7, tokenMatch, 32},
	{5, 19, 9, tokenMatch, 160},
	{5, 20, 10, tokenMatch, 672},
	{5, 21, 12, tokenMatch, 1696},
	{5, 24, 0, tokenLiteral, 0x00},
	{5, 25, 0, tokenLiteral, 0x01},
	{7, 100, 14, tokenMatch, 5792},
	{7, 101, 15, tokenMatch, 22176},
	{7, 102, 18, tokenMatch, 54944},
	{7, 103, 20, tokenMatch, 317088},
	{8, 208, 20, tokenMatch, 1365184},
	{8, 209, 21, tokenMatch, 2413760},
	{9, 420, 22, tokenMatch, 4510912},
	{9, 421, 23, tokenMatch, 8705216},
	{9, 422, 24, tokenMatch, 17093824},
}

// bitReader reads bits MSB-first from a byte slice.
type bitReader struct {
	data    []byte
	bytePos int
	bitPos  uint8 // bits remaining in current byte (8..1)
}

func newBitReader(data []byte) *bitReader {
	br := &bitReader{data: data}
	if len(data) > 0 {
		br.bitPos = 8
	}
	return br
}

func (br *bitReader) hasMore() bool {
	return br.bytePos < len(br.data)
}

func (br *bitReader) getBit() uint32 {
	if br.bytePos >= len(br.data) {
		return 0
	}
	br.bitPos--
	bit := uint32((br.data[br.bytePos] >> br.bitPos) & 1)
	if br.bitPos == 0 {
		br.bytePos++
		br.bitPos = 8
	}
	return bit
}

func (br *bitReader) getBits(n uint8) uint32 {
	var result uint32
	for i := uint8(0); i < n; i++ {
		result = (result << 1) | br.getBit()
	}
	return result
}

func (z *zgfxContext) outputByte(b byte, out *[]byte) {
	z.history[z.historyIdx] = b
	z.historyIdx = (z.historyIdx + 1) % zgfxHistorySize
	*out = append(*out, b)
}

func (z *zgfxContext) outputMatch(distance, length int, out *[]byte) {
	srcIdx := z.historyIdx - distance
	if srcIdx < 0 {
		srcIdx += zgfxHistorySize
	}

	// Grow output slice once instead of per-byte append
	o := *out
	need := len(o) + length
	if need > cap(o) {
		newBuf := make([]byte, len(o), need+(need>>1))
		copy(newBuf, o)
		o = newBuf
	}
	o = o[:need]
	dst := need - length

	// Batch copy from history, handling wrap-around with larger chunks
	for length > 0 {
		end := srcIdx + length
		if end > zgfxHistorySize {
			end = zgfxHistorySize
		}
		n := end - srcIdx
		// Copy chunk to output
		copy(o[dst:dst+n], z.history[srcIdx:end])
		// Update history ring buffer
		histEnd := z.historyIdx + n
		if histEnd <= zgfxHistorySize {
			copy(z.history[z.historyIdx:histEnd], z.history[srcIdx:end])
			z.historyIdx = histEnd % zgfxHistorySize
		} else {
			// History write wraps around
			first := zgfxHistorySize - z.historyIdx
			copy(z.history[z.historyIdx:zgfxHistorySize], z.history[srcIdx:srcIdx+first])
			copy(z.history[0:n-first], z.history[srcIdx+first:end])
			z.historyIdx = n - first
		}
		dst += n
		srcIdx = end % zgfxHistorySize
		length -= n
	}
	*out = o
}

// Decompress decompresses a ZGFX segment (after the 1-byte flag).
// The flag byte has already been checked (0x04 = RDP8 compressed).
func (z *zgfxContext) Decompress(data []byte) []byte {
	br := newBitReader(data)
	// Pre-allocate: compressed data typically expands 2-4x
	out := make([]byte, 0, len(data)*3)

	for br.hasMore() {
		// Decode Huffman token
		token, ok := z.decodeToken(br)
		if !ok {
			break
		}

		if token.tokenType == tokenLiteral {
			value := token.valueBase + br.getBits(token.valueBits)
			z.outputByte(byte(value), &out)
		} else {
			// Match
			distance := int(token.valueBase + br.getBits(token.valueBits))
			if distance == 0 {
				// Distance 0 is not valid for copy
				continue
			}
			length := z.decodeMatchLength(br)
			z.outputMatch(distance, length, &out)
		}
	}

	return out
}

func (z *zgfxContext) decodeToken(br *bitReader) (zgfxToken, bool) {
	// Try to match prefix codes from the Huffman table.
	// Read bits one at a time and match against known prefixes.
	var code uint16
	var bits uint8

	for bits < 9 {
		if !br.hasMore() && bits == 0 {
			return zgfxToken{}, false
		}
		code = (code << 1) | uint16(br.getBit())
		bits++

		for _, t := range zgfxTokenTable {
			if t.prefixLen == bits && t.prefixCode == code {
				return t, true
			}
		}
	}

	return zgfxToken{}, false
}

func (z *zgfxContext) decodeMatchLength(br *bitReader) int {
	// Match length encoding (MS-RDPEGFX 3.3.8.2):
	// 0        -> 3
	// 10       -> 4
	// 110      -> 5
	// 1110     -> 6
	// 11110    -> 7
	// 111110   -> 8 + readBits(2)   [8-11]
	// 1111110  -> 12 + readBits(4)  [12-27]
	// 11111110 -> 28 + readBits(8)  [28-283]
	// 111111110 -> 284 + readBits(16) [284-65819]

	ones := 0
	for ones < 9 {
		if br.getBit() == 0 {
			break
		}
		ones++
	}

	switch ones {
	case 0:
		return 3
	case 1:
		return 4
	case 2:
		return 5
	case 3:
		return 6
	case 4:
		return 7
	case 5:
		return 8 + int(br.getBits(2))
	case 6:
		return 12 + int(br.getBits(4))
	case 7:
		return 28 + int(br.getBits(8))
	case 8:
		return 284 + int(br.getBits(16))
	default:
		return 3
	}
}
