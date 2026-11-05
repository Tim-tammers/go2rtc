package streams

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestCutSEI(t *testing.T) {
	tests := []struct {
		in, out string
		sei     bool
	}{
		{"rtsp://cam/stream", "rtsp://cam/stream", false},
		{"rtsp://cam/stream#sei", "rtsp://cam/stream", true},
		{"onvif://user:pass@<address>:8000#sei", "onvif://user:pass@<address>:8000", true},
		{"ffmpeg:rtsp://cam#video=h264#sei", "ffmpeg:rtsp://cam#video=h264", true},
		{"ffmpeg:rtsp://cam#sei#video=h264#audio=opus", "ffmpeg:rtsp://cam#video=h264#audio=opus", true},
		{"rtsp://cam#seis", "rtsp://cam#seis", false},
		{"rtsp://cam#video=h264", "rtsp://cam#video=h264", false},
	}
	for _, tt := range tests {
		out, sei := cutSEI(tt.in)
		require.Equal(t, tt.out, out, tt.in)
		require.Equal(t, tt.sei, sei, tt.in)
	}
}

func TestProducerSEIFlag(t *testing.T) {
	p := NewProducer("rtsp://cam/stream#sei")
	require.Equal(t, "rtsp://cam/stream", p.url)
	require.True(t, p.sei)

	p = NewProducer("rtsp://cam/stream")
	require.False(t, p.sei)
}

func TestAttachStamper(t *testing.T) {
	h264codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000}
	aac := &core.Codec{Name: core.CodecAAC, ClockRate: 16000}

	p := &Producer{sei: true}

	video := core.NewReceiver(nil, h264codec)
	p.attachStamper(video, nil)
	require.NotNil(t, video.Stamper)

	audio := core.NewReceiver(nil, aac)
	p.attachStamper(audio, nil)
	require.Nil(t, audio.Stamper, "only H264 gets a stamper")

	// reconnect: the new track reuses the same stamper
	video2 := core.NewReceiver(nil, h264codec)
	p.attachStamper(video2, video)
	require.Same(t, video.Stamper, video2.Stamper)

	// flag off: nothing attached
	off := core.NewReceiver(nil, h264codec)
	(&Producer{}).attachStamper(off, nil)
	require.Nil(t, off.Stamper)

	// the receiver feeds the stamper with every packet
	video2.Input(&rtp.Packet{Header: rtp.Header{Timestamp: 1234}})
	_, ok := video2.Stamper.(*h264.Stamper).Lookup(1234)
	require.True(t, ok)
}
