package h264

import (
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

const (
	// stamperWindow - how often the RTP->wall clock offset is re-evaluated.
	// Short enough to follow camera clock drift (100 ppm = 0.5 ms per 5 s),
	// long enough to find at least one low-latency packet.
	stamperWindow = 5 * time.Second

	// stamperResync - if a frame's arrival differs from its computed time by
	// more than this, treat it as a discontinuity (camera restart, RTP
	// timestamp jump, long stall) and re-anchor immediately.
	stamperResync = time.Second

	stamperHistory = 256 // one entry per frame counter value
)

type stamperEntry struct {
	ts    uint32
	stamp SEITimestamp
	sei   []byte // BuildSEI(stamp), shared read-only between consumers
	valid bool
}

// Stamper assigns a wall clock time and a rolling frame counter to every
// frame of one producer. Observe is called by core.Receiver for every packet
// (producer side, one goroutine, frames in order). Consumers call Lookup with the RTP timestamp
// of the frame they are about to send, so every viewer gets the same values
// no matter how far behind its own queue is.
//
// Time is derived from the RTP clock, anchored to the host UTC clock:
//
//	micros = offset + rtpElapsed * 1e6 / clockRate
//
// offset is the minimum (arrival - rtpElapsed) seen over a window, i.e. the
// arrival time of the frame with the least network/queue delay, so arrival
// jitter does not leak into the timestamps while slow camera clock drift is
// still followed.
type Stamper struct {
	clockRate int64
	now       func() time.Time // for tests

	mu sync.RWMutex

	started bool
	lastTS  uint32
	ext     int64 // RTP timestamp extended to 64 bits, relative to the first frame
	frame   uint8

	offset   int64 // micros
	winMin   int64 // micros, minimum offset candidate in the current window
	winStart int64 // micros, wall clock start of the current window

	history [stamperHistory]stamperEntry
}

func NewStamper(clockRate uint32) *Stamper {
	if clockRate == 0 {
		clockRate = 90000
	}
	return &Stamper{clockRate: int64(clockRate), now: time.Now}
}

// Observe registers a packet timestamp from the producer. Packets of the same
// frame (same RTP timestamp, e.g. FU-A fragments) are counted once.
func (s *Stamper) Observe(ts uint32) {
	now := s.now().UnixMicro()

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		if s.history[s.frame].valid {
			s.frame++ // after Reset: keep counting, don't overwrite the last frame
		}
		s.started = true
		s.lastTS = ts
		s.ext = 0
		s.offset, s.winMin, s.winStart = now, now, now
		s.store(ts)
		return
	}

	if ts == s.lastTS {
		return // same frame
	}

	// signed delta handles 32-bit wraparound and small reordering (B-frames)
	s.ext += int64(int32(ts - s.lastTS))
	s.lastTS = ts
	s.frame++

	rtpMicros := s.ext * 1_000_000 / s.clockRate
	candidate := now - rtpMicros

	if d := candidate - s.offset; d > stamperResync.Microseconds() || d < -stamperResync.Microseconds() {
		// discontinuity: re-anchor to this frame
		s.offset, s.winMin, s.winStart = candidate, candidate, now
	} else {
		if candidate < s.winMin {
			s.winMin = candidate
		}
		if now-s.winStart >= stamperWindow.Microseconds() {
			s.offset = s.winMin
			s.winMin, s.winStart = candidate, now
		}
	}

	s.store(ts)
}

// Reset re-anchors the stamper on the next observed frame. Call it when the
// producer reconnects: the new RTP timestamps are unrelated to the old ones.
// The frame counter keeps counting and recent history stays available for
// consumers still draining their queues.
func (s *Stamper) Reset() {
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()
}

// store saves the stamp for the current frame. Must be called with mu locked.
func (s *Stamper) store(ts uint32) {
	stamp := SEITimestamp{
		UnixMicros: s.offset + s.ext*1_000_000/s.clockRate,
		Frame:      s.frame,
	}
	s.history[s.frame] = stamperEntry{ts: ts, stamp: stamp, valid: true}
}

// Lookup returns the stamp of a recently observed frame by its RTP timestamp.
// ok is false if the frame was never observed or is older than the history
// (256 frames).
func (s *Stamper) Lookup(ts uint32) (stamp SEITimestamp, ok bool) {
	stamp, _, ok = s.lookup(ts)
	return
}

// LookupSEI returns the encoded SEI NAL unit (BuildSEI) for a recently
// observed frame. It is built once per frame and shared between consumers,
// so callers must not modify it. Implements core.SEIStamper.
func (s *Stamper) LookupSEI(ts uint32) ([]byte, bool) {
	stamp, sei, ok := s.lookup(ts)
	if !ok || sei != nil {
		return sei, ok
	}

	sei = BuildSEI(stamp)

	s.mu.Lock()
	if e := &s.history[stamp.Frame]; e.valid && e.ts == ts {
		e.sei = sei
	}
	s.mu.Unlock()

	return sei, true
}

func (s *Stamper) lookup(ts uint32) (SEITimestamp, []byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// search newest first: start at the current frame and walk backwards
	for i := 0; i < stamperHistory; i++ {
		e := &s.history[uint8(int(s.frame)-i)]
		if !e.valid {
			break
		}
		if e.ts == ts {
			return e.stamp, e.sei, true
		}
	}
	return SEITimestamp{}, nil, false
}

// SEIStamp is the consumer-side filter. It must receive complete AVCC access
// units, so place it after RTPDepay / RepairAVCC (RepairAVCC strips a leading
// SEI). Keyframes get the timestamp SEI inserted before the first slice; all
// other frames, and keyframes the stamper has no record of, pass unchanged.
// The incoming packet is never modified.
func SEIStamp(stamper core.SEIStamper, handler core.HandlerFunc) core.HandlerFunc {
	return func(packet *rtp.Packet) {
		if len(packet.Payload) > 4 && IsKeyframe(packet.Payload) {
			if sei, ok := stamper.LookupSEI(packet.Timestamp); ok {
				clone := *packet
				clone.Payload = InsertSEI(packet.Payload, sei)
				handler(&clone)
				return
			}
		}
		handler(packet)
	}
}
