package h264

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// fakeClock is a manually advanced clock for Stamper tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) set(d time.Duration) { c.t = base.Add(d) }
func newTestStamper() (*Stamper, *fakeClock) {
	c := &fakeClock{t: base}
	s := NewStamper(90000)
	s.now = c.now
	return s, c
}

var base = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

const frameTicks = 3000 // 30 fps at 90 kHz

func frameDur(n int) time.Duration { return time.Duration(n) * time.Second / 30 }

func requireStamp(t *testing.T, s *Stamper, ts uint32, want time.Time, frame uint8) {
	t.Helper()
	got, ok := s.Lookup(ts)
	require.True(t, ok, "ts %d not found", ts)
	require.Equal(t, frame, got.Frame)
	require.InDelta(t, want.UnixMicro(), got.UnixMicros, 1, "ts %d", ts)
}

func TestStamperFragmentsCountOnce(t *testing.T) {
	s, _ := newTestStamper()

	for i := 0; i < 3; i++ {
		s.Observe(1000) // FU-A fragments of one frame
	}
	s.Observe(4000)
	s.Observe(4000)

	requireStamp(t, s, 1000, base, 0)
	requireStamp(t, s, 4000, base.Add(frameDur(1)), 1)
}

func TestStamperFrameCounterRolls(t *testing.T) {
	s, c := newTestStamper()

	for i := 0; i <= 300; i++ {
		c.set(frameDur(i))
		s.Observe(uint32(i * frameTicks))
	}

	requireStamp(t, s, 300*frameTicks, base.Add(frameDur(300)), uint8(300%256))
	requireStamp(t, s, 256*frameTicks, base.Add(frameDur(256)), 0)

	// older than 256 frames: gone
	_, ok := s.Lookup(10 * frameTicks)
	require.False(t, ok)

	// never observed
	_, ok = s.Lookup(12345)
	require.False(t, ok)
}

func TestStamperRTPWraparound(t *testing.T) {
	s, c := newTestStamper()

	start := uint32(0xFFFFFFFF - 2*frameTicks)
	for i := 0; i < 6; i++ {
		c.set(frameDur(i))
		s.Observe(start + uint32(i*frameTicks)) // wraps after frame 2
	}

	requireStamp(t, s, start+5*frameTicks, base.Add(frameDur(5)), 5)
}

func TestStamperIgnoresJitter(t *testing.T) {
	s, c := newTestStamper()

	// frames are captured exactly 1/30 s apart, but arrive with 0..15 ms jitter
	jitter := []time.Duration{0, 15, 3, 9, 1, 12, 0, 7}
	for i := 0; i < 60; i++ {
		c.set(frameDur(i) + jitter[i%len(jitter)]*time.Millisecond)
		s.Observe(uint32(i * frameTicks))

		// stamps follow the RTP clock, not the arrival time
		requireStamp(t, s, uint32(i*frameTicks), base.Add(frameDur(i)), uint8(i))
	}
}

func TestStamperFindsLowestDelay(t *testing.T) {
	s, c := newTestStamper()

	// first frame is late by 40 ms, all later frames by only 2 ms
	c.set(40 * time.Millisecond)
	s.Observe(0)

	n := 0
	for ; frameDur(n) < stamperWindow+time.Second; n++ {
		c.set(frameDur(n+1) + 2*time.Millisecond)
		s.Observe(uint32((n + 1) * frameTicks))
	}

	// after one window the anchor moved to the 2 ms frames
	requireStamp(t, s, uint32(n*frameTicks), base.Add(frameDur(n)+2*time.Millisecond), uint8(n))
}

func TestStamperFollowsDrift(t *testing.T) {
	s, c := newTestStamper()

	// camera clock runs 100 ppm slow: each 3000 ticks really take 1/30 s * 1.0001
	const ppm = 100
	var last time.Time
	for i := 0; i < 30*60; i++ { // one minute
		real := frameDur(i) + frameDur(i)*ppm/1_000_000
		c.set(real)
		s.Observe(uint32(i * frameTicks))
		last = base.Add(real)
	}

	got, ok := s.Lookup(uint32((30*60 - 1) * frameTicks))
	require.True(t, ok)

	// without correction the error would be 6 ms after a minute; stay within 1 ms
	require.InDelta(t, last.UnixMicro(), got.UnixMicros, 1000)
}

func TestStamperResync(t *testing.T) {
	s, c := newTestStamper()

	s.Observe(0)
	c.set(frameDur(1))
	s.Observe(frameTicks)

	// camera restarts: RTP timestamp jumps to a random value
	c.set(10 * time.Second)
	s.Observe(77_000_000)
	requireStamp(t, s, 77_000_000, base.Add(10*time.Second), 2)

	c.set(10*time.Second + frameDur(1))
	s.Observe(77_000_000 + frameTicks)
	requireStamp(t, s, 77_000_000+frameTicks, base.Add(10*time.Second+frameDur(1)), 3)
}

func TestSEIStampFilter(t *testing.T) {
	s, _ := newTestStamper()
	s.Observe(9000)

	var out []*rtp.Packet
	filter := SEIStamp(s, func(p *rtp.Packet) { out = append(out, p) })

	sps := []byte{0x67, 0x64, 0x00, 0x1F}
	pps := []byte{0x68, 0xEE, 0x3C, 0x80}
	key := JoinNALU(sps, pps, []byte{0x65, 0x88, 0x80})
	keyOrig := append([]byte{}, key...)
	pframe := JoinNALU([]byte{0x41, 0x9A, 0x02})

	keyPkt := &rtp.Packet{Header: rtp.Header{Timestamp: 9000}, Payload: key}
	filter(keyPkt)
	filter(&rtp.Packet{Header: rtp.Header{Timestamp: 9000}, Payload: pframe})
	filter(&rtp.Packet{Header: rtp.Header{Timestamp: 1}, Payload: key}) // unknown ts
	filter(&rtp.Packet{Header: rtp.Header{Timestamp: 9000}})            // empty payload

	require.Len(t, out, 4)

	// keyframe: SEI inserted, original packet untouched
	require.Equal(t, keyOrig, keyPkt.Payload)
	require.NotSame(t, keyPkt, out[0])
	require.Equal(t, uint32(9000), out[0].Timestamp)
	require.Equal(t, []byte{7, 8, 6, 5}, naluTypesAVCC(t, out[0].Payload))

	got, err := ParseSEI(SplitNALU(out[0].Payload)[2][4:])
	require.NoError(t, err)
	require.Equal(t, SEITimestamp{UnixMicros: base.UnixMicro(), Frame: 0}, got)

	// P-frame, unknown keyframe, empty: passed through as-is
	require.Equal(t, pframe, out[1].Payload)
	require.Equal(t, key, out[2].Payload)
	require.Empty(t, out[3].Payload)
}

func TestStamperConcurrent(t *testing.T) {
	s := NewStamper(90000)
	key := JoinNALU([]byte{0x65, 0x88, 0x80})

	var wg sync.WaitGroup
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			filter := SEIStamp(s, func(*rtp.Packet) {})
			for i := 0; i < 2000; i++ {
				filter(&rtp.Packet{Header: rtp.Header{Timestamp: uint32(i * frameTicks)}, Payload: key})
			}
		}()
	}
	for i := 0; i < 2000; i++ {
		s.Observe(uint32(i * frameTicks))
	}
	wg.Wait()
}

func TestStamperReset(t *testing.T) {
	s, c := newTestStamper()

	s.Observe(0)
	c.set(frameDur(1))
	s.Observe(frameTicks)

	// reconnect 300 ms later with an unrelated RTP base (would not trigger resync)
	s.Reset()
	c.set(frameDur(1) + 300*time.Millisecond)
	s.Observe(frameTicks + 9000)

	// re-anchored to the new arrival, frame counter continues, old frames still there
	requireStamp(t, s, frameTicks+9000, base.Add(frameDur(1)+300*time.Millisecond), 2)
	requireStamp(t, s, frameTicks, base.Add(frameDur(1)), 1)
}
