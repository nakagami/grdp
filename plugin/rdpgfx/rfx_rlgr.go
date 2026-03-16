package rdpgfx

// RLGR1 (Run-Length Golomb-Rice Level 1) decoder for RFX codec.
// Reference: MS-RDPRFX 3.1.8.1.7.3 RLGR1/RLGR3 Pseudocode
// Matches FreeRDP's rfx_rlgr.c implementation.

const (
rlgrLSGR  = 3  // shift count to convert kp to k
rlgrKPMax = 80 // max value for kp or krp
rlgrUPGR  = 4  // increase in kp after a zero run in RL mode
rlgrDNGR  = 6  // decrease in kp after a nonzero symbol in RL mode
rlgrUQGR  = 3  // increase in kp after zero symbol in GR mode
rlgrDQGR  = 3  // decrease in kp after nonzero symbol in GR mode
)

// rlgr1Decode decodes RLGR1-encoded data into signed 16-bit DWT coefficients.
func rlgr1Decode(data []byte, outputSize int) []int16 {
output := make([]int16, outputSize)
br := &rlgrBitReader{data: data, total: len(data) * 8}
cnt := 0

k := uint32(1)
kp := uint32(1 << rlgrLSGR) // 8
kr := uint32(1)
krp := uint32(1 << rlgrLSGR) // 8

for br.remaining() > 0 && cnt < outputSize {
if k > 0 {
// RL (Run-Length) Mode

// Count leading 0-bits → number of full run groups
vk := br.countLeadingBits(0)
if br.remaining() < 0 {
break
}

// Each leading 0 adds (1 << k) to run, with k adapting upward
run := uint32(0)
for i := uint32(0); i < vk; i++ {
run += 1 << k
kp += rlgrUPGR
if kp > rlgrKPMax {
kp = rlgrKPMax
}
k = kp >> rlgrLSGR
}

// Read k bits for run remainder
if br.remaining() < int(k) {
break
}
if k > 0 {
run += br.readBits(int(k))
}

// Read sign bit for the non-zero value
if br.remaining() < 1 {
break
}
sign := br.readBits(1)

// Decode non-zero magnitude using GR code with leading 1-bits
vk2 := br.countLeadingBits(1)
if br.remaining() < 0 {
break
}

// Read kr bits for code remainder
if br.remaining() < int(kr) {
break
}
code := uint32(0)
if kr > 0 {
code = br.readBits(int(kr))
}
code |= vk2 << kr

// Update kr/krp
if vk2 == 0 {
if krp > 2 {
krp -= 2
} else {
krp = 0
}
kr = krp >> rlgrLSGR
} else if vk2 != 1 {
krp += vk2
if krp > rlgrKPMax {
krp = rlgrKPMax
}
kr = krp >> rlgrLSGR
}

// Update k/kp (decrease after non-zero)
if kp > rlgrDNGR {
kp -= rlgrDNGR
} else {
kp = 0
}
k = kp >> rlgrLSGR

// Compute magnitude (code + 1, guaranteed non-zero)
mag := int16(code + 1)
if sign != 0 {
mag = -mag
}

// Output: run zeros, then the non-zero value
for i := uint32(0); i < run && cnt < outputSize; i++ {
output[cnt] = 0
cnt++
}
if cnt < outputSize {
output[cnt] = mag
cnt++
}

} else {
// GR (Golomb-Rice) Mode

// Count leading 1-bits
vk := br.countLeadingBits(1)
if br.remaining() < 0 {
break
}

// Read kr bits for code remainder
if br.remaining() < int(kr) {
break
}
code := uint32(0)
if kr > 0 {
code = br.readBits(int(kr))
}
code |= vk << kr

// Update kr/krp
if vk == 0 {
if krp > 2 {
krp -= 2
} else {
krp = 0
}
kr = krp >> rlgrLSGR
} else if vk != 1 {
krp += vk
if krp > rlgrKPMax {
krp = rlgrKPMax
}
kr = krp >> rlgrLSGR
}

// RLGR1: sign embedded in code as code = 2*magnitude - sign
if code == 0 {
kp += rlgrUQGR
if kp > rlgrKPMax {
kp = rlgrKPMax
}
k = kp >> rlgrLSGR

if cnt < outputSize {
output[cnt] = 0
cnt++
}
} else {
if kp > rlgrDQGR {
kp -= rlgrDQGR
} else {
kp = 0
}
k = kp >> rlgrLSGR

var mag int16
if code&1 != 0 {
// odd code → negative
mag = -int16((code + 1) >> 1)
} else {
// even code → positive
mag = int16(code >> 1)
}
if cnt < outputSize {
output[cnt] = mag
cnt++
}
}
}
}

return output
}

// rlgrBitReader reads bits MSB-first from a byte slice.
type rlgrBitReader struct {
data    []byte
bytePos int
bitPos  int // 0=MSB, 7=LSB
total   int
read    int
}

func (br *rlgrBitReader) remaining() int {
return br.total - br.read
}

func (br *rlgrBitReader) readBits(n int) uint32 {
val := uint32(0)
for i := 0; i < n; i++ {
if br.read >= br.total {
return val
}
bit := (br.data[br.bytePos] >> uint(7-br.bitPos)) & 1
val = (val << 1) | uint32(bit)
br.bitPos++
br.read++
if br.bitPos >= 8 {
br.bitPos = 0
br.bytePos++
}
}
return val
}

// countLeadingBits counts consecutive bits matching 'target' (0 or 1),
// then skips the terminator bit (the opposite). Returns the count.
func (br *rlgrBitReader) countLeadingBits(target uint32) uint32 {
count := uint32(0)
for br.remaining() > 0 {
bit := br.readBits(1)
if bit == target {
count++
} else {
// This is the terminator bit
return count
}
}
return count
}
