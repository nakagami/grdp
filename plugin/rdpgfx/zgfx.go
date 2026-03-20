package rdpgfx

// ZGFX (RDP8 Bulk Compression) decompressor.
// Implements the decompression algorithm described in MS-RDPEGFX 2.2.4 / 3.3.8.
// Based on FreeRDP's reference implementation (libfreerdp/codec/zgfx.c).

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

// Fixed Huffman table from FreeRDP zgfx.c (Apache-2.0 licensed).
var zgfxTokenTable = []zgfxToken{
	{1, 0, 8, tokenLiteral, 0},           // 0
	{5, 17, 5, tokenMatch, 0},            // 10001
	{5, 18, 7, tokenMatch, 32},           // 10010
	{5, 19, 9, tokenMatch, 160},          // 10011
	{5, 20, 10, tokenMatch, 672},         // 10100
	{5, 21, 12, tokenMatch, 1696},        // 10101
	{5, 24, 0, tokenLiteral, 0x00},       // 11000
	{5, 25, 0, tokenLiteral, 0x01},       // 11001
	{6, 44, 14, tokenMatch, 5792},        // 101100
	{6, 45, 15, tokenMatch, 22176},       // 101101
	{6, 52, 0, tokenLiteral, 0x02},       // 110100
	{6, 53, 0, tokenLiteral, 0x03},       // 110101
	{6, 54, 0, tokenLiteral, 0xFF},       // 110110
	{7, 92, 18, tokenMatch, 54944},       // 1011100
	{7, 93, 20, tokenMatch, 317088},      // 1011101
	{7, 110, 0, tokenLiteral, 0x04},      // 1101110
	{7, 111, 0, tokenLiteral, 0x05},      // 1101111
	{7, 112, 0, tokenLiteral, 0x06},      // 1110000
	{7, 113, 0, tokenLiteral, 0x07},      // 1110001
	{7, 114, 0, tokenLiteral, 0x08},      // 1110010
	{7, 115, 0, tokenLiteral, 0x09},      // 1110011
	{7, 116, 0, tokenLiteral, 0x0A},      // 1110100
	{7, 117, 0, tokenLiteral, 0x0B},      // 1110101
	{7, 118, 0, tokenLiteral, 0x3A},      // 1110110
	{7, 119, 0, tokenLiteral, 0x3B},      // 1110111
	{7, 120, 0, tokenLiteral, 0x3C},      // 1111000
	{7, 121, 0, tokenLiteral, 0x3D},      // 1111001
	{7, 122, 0, tokenLiteral, 0x3E},      // 1111010
	{7, 123, 0, tokenLiteral, 0x3F},      // 1111011
	{7, 124, 0, tokenLiteral, 0x40},      // 1111100
	{7, 125, 0, tokenLiteral, 0x80},      // 1111101
	{8, 188, 20, tokenMatch, 1365664},    // 10111100
	{8, 189, 21, tokenMatch, 2414240},    // 10111101
	{8, 252, 0, tokenLiteral, 0x0C},      // 11111100
	{8, 253, 0, tokenLiteral, 0x38},      // 11111101
	{8, 254, 0, tokenLiteral, 0x39},      // 11111110
	{8, 255, 0, tokenLiteral, 0x66},      // 11111111
	{9, 380, 22, tokenMatch, 4511392},    // 101111100
	{9, 381, 23, tokenMatch, 8705696},    // 101111101
	{9, 382, 24, tokenMatch, 17094304},   // 101111110
}

// bitReader reads bits MSB-first from a byte slice.
type bitReader struct {
	data          []byte
	bytePos       int
	bitPos        uint8  // bits remaining in current byte (8..1)
	bitsRemaining uint32 // total decodable bits remaining
}

func newBitReader(data []byte) *bitReader {
	br := &bitReader{data: data}
	if len(data) > 0 {
		br.bitPos = 8
	}
	return br
}

// newBitReaderWithCount creates a reader that tracks total decodable bits.
// The last byte of RDP8 compressed data encodes the number of padding bits
// to subtract: bitsAvailable = (len-1)*8 - lastByte.
func newBitReaderWithCount(data []byte) *bitReader {
	br := &bitReader{data: data}
	if len(data) < 2 {
		return br
	}
	br.bitPos = 8
	paddingBits := uint32(data[len(data)-1])
	totalBits := uint32(len(data)-1) * 8
	if paddingBits > totalBits {
		br.bitsRemaining = 0
	} else {
		br.bitsRemaining = totalBits - paddingBits
	}
	// Exclude the last byte from readable data
	br.data = data[:len(data)-1]
	return br
}

func (br *bitReader) hasBitsRemaining() bool {
	return br.bitsRemaining > 0
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

// readBytes reads count raw bytes (byte-aligned). Used for unencoded sections.
func (br *bitReader) readBytes(count int) []byte {
	// Discard any partial-byte bits to align
	if br.bitPos != 8 && br.bitPos != 0 {
		br.bytePos++
		br.bitPos = 8
	}
	if br.bytePos+count > len(br.data) {
		count = len(br.data) - br.bytePos
	}
	if count <= 0 {
		return nil
	}
	out := make([]byte, count)
	copy(out, br.data[br.bytePos:br.bytePos+count])
	br.bytePos += count
	return out
}

func (z *zgfxContext) historyWrite(data []byte) {
	for _, b := range data {
		z.history[z.historyIdx] = b
		z.historyIdx = (z.historyIdx + 1) % zgfxHistorySize
	}
}

func (z *zgfxContext) outputLiteral(b byte, out *[]byte) {
	z.history[z.historyIdx] = b
	z.historyIdx = (z.historyIdx + 1) % zgfxHistorySize
	*out = append(*out, b)
}

func (z *zgfxContext) outputMatch(distance, count int, out *[]byte) {
	srcIdx := (z.historyIdx + zgfxHistorySize - distance) % zgfxHistorySize

	// Read from history ring buffer into temporary buffer
	tmp := make([]byte, 0, count)

	// First copy: up to 'distance' bytes from history
	toCopy := count
	if toCopy > distance {
		toCopy = distance
	}
	for i := 0; i < toCopy; i++ {
		tmp = append(tmp, z.history[(srcIdx+i)%zgfxHistorySize])
	}

	// If count > distance, repeat the pattern
	for len(tmp) < count {
		remaining := count - len(tmp)
		if remaining > len(tmp) {
			remaining = len(tmp)
		}
		tmp = append(tmp, tmp[:remaining]...)
	}

	// Write to history and output
	z.historyWrite(tmp)
	*out = append(*out, tmp...)
}

// Decompress decompresses a ZGFX compressed segment payload.
// The payload must NOT include the 1-byte segment header (flags byte).
// In RDP8 ZGFX, the last byte of the payload encodes the number of
// padding bits to subtract from the total bit count.
func (z *zgfxContext) Decompress(data []byte) []byte {
	if len(data) < 2 {
		// Need at least 1 byte of compressed data + 1 byte of padding count
		return nil
	}

	br := newBitReaderWithCount(data)
	out := make([]byte, 0, len(data)*3)

	tokenCount := 0
	for br.hasBitsRemaining() {
		token, ok := z.decodeToken(br)
		if !ok {
			break
		}
		tokenCount++
		br.bitsRemaining -= uint32(token.prefixLen)

		if token.tokenType == tokenLiteral {
			if br.bitsRemaining < uint32(token.valueBits) {
				break
			}
			value := token.valueBase + br.getBits(token.valueBits)
			br.bitsRemaining -= uint32(token.valueBits)
			z.outputLiteral(byte(value), &out)
		} else {
			// Match token
			if br.bitsRemaining < uint32(token.valueBits) {
				break
			}
			distance := int(token.valueBase + br.getBits(token.valueBits))
			br.bitsRemaining -= uint32(token.valueBits)

			if distance != 0 {
				// Match: copy from history
				count := z.decodeMatchCount(br)
				z.outputMatch(distance, count, &out)
			} else {
				// Unencoded: read raw bytes
				if br.bitsRemaining < 15 {
					break
				}
				rawCount := int(br.getBits(15))
				br.bitsRemaining -= 15
				// Discard remaining bits in current byte to align to byte boundary
				// (equivalent to FreeRDP's cBitsCurrent = 0; BitsCurrent = 0;)
				if br.bitPos < 8 {
					br.bitsRemaining -= uint32(br.bitPos)
					br.bytePos++
					br.bitPos = 8
				}
				if br.bytePos+rawCount > len(br.data) || uint32(rawCount)*8 > br.bitsRemaining {
					break
				}
				rawBytes := br.data[br.bytePos : br.bytePos+rawCount]
				br.bytePos += rawCount
				br.bitsRemaining -= uint32(rawCount) * 8
				z.historyWrite(rawBytes)
				out = append(out, rawBytes...)
			}
		}
	}

	return out
}

func (z *zgfxContext) decodeToken(br *bitReader) (zgfxToken, bool) {
	var code uint16
	var bits uint8

	for bits < 9 {
		if br.bitsRemaining <= uint32(bits) && bits > 0 {
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

// decodeMatchCount decodes the match length using FreeRDP's algorithm:
//
//	0           → 3
//	10 + 2 bits → 4 + value   (4..7)
//	110 + 3 bits → 8 + value  (8..15)
//	1110 + 4 bits → 16 + value (16..31)
//	... and so on (each additional leading 1 doubles the base and adds 1 extra bit)
func (z *zgfxContext) decodeMatchCount(br *bitReader) int {
	bit := br.getBit()
	br.bitsRemaining--
	if bit == 0 {
		return 3
	}

	count := 4
	extra := uint8(2)

	bit = br.getBit()
	br.bitsRemaining--
	for bit == 1 {
		count <<= 1
		extra++
		bit = br.getBit()
		br.bitsRemaining--
	}

	if br.bitsRemaining < uint32(extra) {
		return count
	}
	count += int(br.getBits(extra))
	br.bitsRemaining -= uint32(extra)
	return count
}
