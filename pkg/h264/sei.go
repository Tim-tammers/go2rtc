package h264

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// SEI timestamp payload carried as user_data_unregistered (ITU-T H.264 D.1.6).
//
// Layout of the payload (before NAL wrapping and emulation prevention):
//
//	offset size field
//	0      16   UUID           identifies this SEI as a go2rtc timestamp
//	16     1    version        payload format version
//	17     8    unix_micros    uint64 big-endian, UTC microseconds since Unix epoch
//	25     1    frame_counter  uint8, rolling 0..255
//
// The full NAL unit is:
//
//	0x06 (NAL header) | 0x05 (payloadType) | 0x1A (payloadSize) | payload | 0x80 (rbsp trailing bits)
//
// with emulation prevention bytes applied to everything after the NAL header.
const (
	SEIPayloadTypeUserDataUnregistered = 5

	SEITimestampVersion = 1

	SEITimestampUUIDSize    = 16
	SEITimestampPayloadSize = SEITimestampUUIDSize + 1 + 8 + 1 // 26

	seiOffsetVersion = SEITimestampUUIDSize
	seiOffsetMicros  = seiOffsetVersion + 1
	seiOffsetFrame   = seiOffsetMicros + 8
)

// SEITimestampUUID - fixed identifier for go2rtc timestamp SEI messages.
// 008A10A2-00FC-4860-B3D3-FFE71CC12842
var SEITimestampUUID = [SEITimestampUUIDSize]byte{
	0x00, 0x8A, 0x10, 0xA2, 0x00, 0xFC, 0x48, 0x60,
	0xB3, 0xD3, 0xFF, 0xE7, 0x1C, 0xC1, 0x28, 0x42,
}

var (
	ErrSEIPayloadSize    = errors.New("h264: wrong SEI timestamp payload size")
	ErrSEIPayloadUUID    = errors.New("h264: SEI payload UUID mismatch")
	ErrSEIPayloadVersion = errors.New("h264: unsupported SEI timestamp version")
	ErrSEINotSEI         = errors.New("h264: NAL unit is not SEI")
	ErrSEIMalformed      = errors.New("h264: malformed SEI message")
	ErrSEINotFound       = errors.New("h264: no timestamp SEI in NAL unit")
)

// SEITimestamp - decoded contents of a go2rtc timestamp SEI payload.
type SEITimestamp struct {
	UnixMicros int64 // UTC wall clock of the frame, microseconds since Unix epoch
	Frame      uint8 // rolling frame counter
}

// MarshalPayload returns the raw 26-byte payload (no NAL header, no escaping).
func (t SEITimestamp) MarshalPayload() []byte {
	b := make([]byte, SEITimestampPayloadSize)
	copy(b, SEITimestampUUID[:])
	b[seiOffsetVersion] = SEITimestampVersion
	binary.BigEndian.PutUint64(b[seiOffsetMicros:], uint64(t.UnixMicros))
	b[seiOffsetFrame] = t.Frame
	return b
}

// UnmarshalPayload parses a raw (already unescaped) 26-byte payload.
func (t *SEITimestamp) UnmarshalPayload(b []byte) error {
	if len(b) != SEITimestampPayloadSize {
		return ErrSEIPayloadSize
	}
	if !bytes.Equal(b[:SEITimestampUUIDSize], SEITimestampUUID[:]) {
		return ErrSEIPayloadUUID
	}
	if b[seiOffsetVersion] != SEITimestampVersion {
		return ErrSEIPayloadVersion
	}
	t.UnixMicros = int64(binary.BigEndian.Uint64(b[seiOffsetMicros:]))
	t.Frame = b[seiOffsetFrame]
	return nil
}

// BuildSEI returns a complete SEI NAL unit (header byte included, no AVCC
// length prefix and no Annex-B start code) carrying the timestamp payload.
func BuildSEI(t SEITimestamp) []byte {
	// RBSP: payloadType | payloadSize | payload | rbsp_trailing_bits
	rbsp := make([]byte, 0, 2+SEITimestampPayloadSize+1)
	rbsp = append(rbsp, SEIPayloadTypeUserDataUnregistered, SEITimestampPayloadSize)
	rbsp = append(rbsp, t.MarshalPayload()...)
	rbsp = append(rbsp, 0x80)

	nal := make([]byte, 1, 1+len(rbsp)+len(rbsp)/2)
	nal[0] = NALUTypeSEI // forbidden_zero_bit=0, nal_ref_idc=0
	return escapeRBSP(nal, rbsp)
}

// ParseSEI looks for a go2rtc timestamp message inside an SEI NAL unit
// (header byte included, no length prefix / start code). Other SEI messages
// in the same NAL unit are skipped.
func ParseSEI(nal []byte) (SEITimestamp, error) {
	var t SEITimestamp

	if len(nal) < 2 || nal[0]&0x1F != NALUTypeSEI {
		return t, ErrSEINotSEI
	}

	b := unescapeRBSP(nal[1:])

	for len(b) > 0 {
		// rbsp_trailing_bits: only the stop bit is left
		if b[0] == 0x80 && len(b) == 1 {
			break
		}

		payloadType, n := readSEIValue(b)
		if n == 0 {
			return t, ErrSEIMalformed
		}
		b = b[n:]

		payloadSize, n := readSEIValue(b)
		if n == 0 || len(b)-n < payloadSize {
			return t, ErrSEIMalformed
		}
		b = b[n:]

		payload := b[:payloadSize]
		b = b[payloadSize:]

		if payloadType == SEIPayloadTypeUserDataUnregistered &&
			payloadSize == SEITimestampPayloadSize &&
			bytes.Equal(payload[:SEITimestampUUIDSize], SEITimestampUUID[:]) {
			err := t.UnmarshalPayload(payload)
			return t, err
		}
	}

	return t, ErrSEINotFound
}

// readSEIValue reads an SEI payloadType / payloadSize value: any number of
// 0xFF bytes (each adding 255) followed by one final byte.
// Returns n == 0 if the data ends early.
func readSEIValue(b []byte) (value, n int) {
	for i, c := range b {
		value += int(c)
		if c != 0xFF {
			return value, i + 1
		}
	}
	return 0, 0
}

// escapeRBSP appends rbsp to dst, inserting an emulation prevention byte (0x03)
// whenever two zero bytes are followed by a byte <= 0x03 (H.264 7.4.1).
func escapeRBSP(dst, rbsp []byte) []byte {
	var zeros int
	for _, c := range rbsp {
		if zeros >= 2 && c <= 0x03 {
			dst = append(dst, 0x03)
			zeros = 0
		}
		dst = append(dst, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

// unescapeRBSP removes emulation prevention bytes (0x03 following two zero bytes).
func unescapeRBSP(b []byte) []byte {
	dst := make([]byte, 0, len(b))
	var zeros int
	for _, c := range b {
		if zeros >= 2 && c == 0x03 {
			zeros = 0
			continue
		}
		dst = append(dst, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

// InsertSEI inserts an SEI NAL unit (from BuildSEI, without length prefix)
// into an AVCC access unit, directly before the first coded slice (NAL types
// 1..5). This keeps AUD / SPS / PPS / existing SEI in front of it, giving e.g.
// [SPS][PPS][SEI][IDR], as required by H.264 7.4.1.2.3.
//
// If the access unit has no slice or its length prefixes are malformed,
// avcc is returned unchanged. The input slice is never modified.
func InsertSEI(avcc, sei []byte) []byte {
	var offset int

	for offset+4 < len(avcc) {
		size := int(binary.BigEndian.Uint32(avcc[offset:]))
		if size == 0 || offset+4+size > len(avcc) {
			return avcc // malformed
		}

		if typ := avcc[offset+4] & 0x1F; typ >= NALUTypePFrame && typ <= NALUTypeIFrame {
			b := make([]byte, 0, len(avcc)+4+len(sei))
			b = append(b, avcc[:offset]...)
			b = binary.BigEndian.AppendUint32(b, uint32(len(sei)))
			b = append(b, sei...)
			return append(b, avcc[offset:]...)
		}

		offset += 4 + size
	}

	return avcc // no slice found
}
