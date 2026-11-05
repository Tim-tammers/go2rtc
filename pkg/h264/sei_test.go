package h264

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// hasStartCodeEmulation reports whether b contains 00 00 00, 00 00 01 or
// 00 00 02, which must never appear inside an escaped NAL unit
// (00 00 03 is the emulation prevention sequence itself).
func hasStartCodeEmulation(b []byte) bool {
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] <= 2 {
			return true
		}
	}
	return false
}

func TestSEIRoundTrip(t *testing.T) {
	cases := []SEITimestamp{
		{UnixMicros: 1_793_000_000_123_456, Frame: 42}, // realistic 2026 timestamp
		{UnixMicros: 0, Frame: 0},                      // all zeros: worst case for escaping
		{UnixMicros: 0x0000000100000002, Frame: 3},     // 00 00 00 01 / 00 00 00 02 / 03
		{UnixMicros: 0x0000030000000300, Frame: 255},   // payload already contains 00 00 03
		{UnixMicros: -1, Frame: 1},                     // all 0xFF
	}

	for _, tc := range cases {
		nal := BuildSEI(tc)

		require.Equal(t, byte(NALUTypeSEI), nal[0])
		require.False(t, hasStartCodeEmulation(nal), "unescaped start code in % X", nal)
		require.Equal(t, byte(0x80), nal[len(nal)-1])

		got, err := ParseSEI(nal)
		require.NoError(t, err)
		require.Equal(t, tc, got)
	}
}

func TestSEIUnescapedLength(t *testing.T) {
	// 1 header + 1 type + 1 size + 26 payload + 1 trailing = 30 bytes before escaping
	nal := BuildSEI(SEITimestamp{UnixMicros: 1_793_000_000_123_456, Frame: 7})
	require.Len(t, unescapeRBSP(nal[1:]), 29)
}

func TestSEIEscapeRBSP(t *testing.T) {
	tests := []struct{ in, out []byte }{
		{[]byte{0, 0, 0}, []byte{0, 0, 3, 0}},
		{[]byte{0, 0, 1}, []byte{0, 0, 3, 1}},
		{[]byte{0, 0, 3}, []byte{0, 0, 3, 3}},
		{[]byte{0, 0, 4}, []byte{0, 0, 4}},
		{[]byte{0, 0, 0, 0, 0}, []byte{0, 0, 3, 0, 0, 3, 0}},
	}
	for _, tt := range tests {
		require.Equal(t, tt.out, escapeRBSP(nil, tt.in))
		require.Equal(t, tt.in, unescapeRBSP(tt.out))
	}
}

func TestSEISkipsForeignMessages(t *testing.T) {
	ours := BuildSEI(SEITimestamp{UnixMicros: 123, Frame: 9})

	// header | foreign user_data_unregistered (type 5, size 17) | our message | trailing
	foreign := append([]byte{5, 17}, make([]byte, 17)...)
	for i := range foreign[2:] {
		foreign[2+i] = 0xAA // a different UUID
	}

	rbsp := append([]byte{}, foreign...)
	rbsp = append(rbsp, unescapeRBSP(ours[1:])...) // already ends with 0x80
	nal := escapeRBSP([]byte{NALUTypeSEI}, rbsp)

	got, err := ParseSEI(nal)
	require.NoError(t, err)
	require.Equal(t, SEITimestamp{UnixMicros: 123, Frame: 9}, got)
}

func TestSEIErrors(t *testing.T) {
	_, err := ParseSEI([]byte{NALUTypeIFrame, 0x88})
	require.ErrorIs(t, err, ErrSEINotSEI)

	// type 5, size 26, but only 3 payload bytes
	_, err = ParseSEI([]byte{NALUTypeSEI, 5, 26, 1, 2, 3})
	require.ErrorIs(t, err, ErrSEIMalformed)

	// a valid SEI with only a foreign message
	_, err = ParseSEI([]byte{NALUTypeSEI, 1, 1, 0x10, 0x80})
	require.ErrorIs(t, err, ErrSEINotFound)
}

// naluTypesAVCC returns the NAL types of an AVCC access unit.
func naluTypesAVCC(t *testing.T, avcc []byte) (types []byte) {
	for _, nal := range SplitNALU(avcc) {
		types = append(types, nal[4]&0x1F)
	}
	return
}

func TestInsertSEI(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x1F}
	pps := []byte{0x68, 0xEE, 0x3C, 0x80}
	aud := []byte{0x09, 0xF0}
	idr := []byte{0x65, 0x88, 0x80, 0x10, 0x20}
	foreignSEI := []byte{0x06, 0x01, 0x01, 0x10, 0x80}

	stamp := SEITimestamp{UnixMicros: 1_793_000_000_123_456, Frame: 200}
	sei := BuildSEI(stamp)

	tests := []struct {
		name string
		in   [][]byte
		want []byte
	}{
		{"idr only", [][]byte{idr}, []byte{6, 5}},
		{"sps pps idr", [][]byte{sps, pps, idr}, []byte{7, 8, 6, 5}},
		{"aud sps pps idr", [][]byte{aud, sps, pps, idr}, []byte{9, 7, 8, 6, 5}},
		{"existing sei", [][]byte{sps, pps, foreignSEI, idr}, []byte{7, 8, 6, 6, 5}},
		{"multi slice", [][]byte{sps, pps, idr, idr}, []byte{7, 8, 6, 5, 5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := JoinNALU(tt.in...)
			orig := append([]byte{}, in...)

			out := InsertSEI(in, sei)

			require.Equal(t, orig, in, "input was modified")
			require.Len(t, out, len(in)+4+len(sei))
			require.Equal(t, tt.want, naluTypesAVCC(t, out))
			require.True(t, IsKeyframe(out))

			// the last SEI before the first slice is ours and parses back
			nals := SplitNALU(out)
			var got SEITimestamp
			var err error
			for _, nal := range nals {
				if nal[4]&0x1F == NALUTypeSEI {
					got, err = ParseSEI(nal[4:])
				}
			}
			require.NoError(t, err)
			require.Equal(t, stamp, got)
		})
	}
}

func TestInsertSEIUnchanged(t *testing.T) {
	sei := BuildSEI(SEITimestamp{UnixMicros: 1, Frame: 1})

	// no slice in the access unit
	ps := JoinNALU([]byte{0x67, 0x64}, []byte{0x68, 0xEE})
	require.Equal(t, ps, InsertSEI(ps, sei))

	// length prefix larger than the data
	bad := []byte{0, 0, 0, 50, 0x65, 0x88}
	require.Equal(t, bad, InsertSEI(bad, sei))

	// empty
	require.Empty(t, InsertSEI(nil, sei))
}
