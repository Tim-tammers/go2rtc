package streams

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
)

type state byte

const (
	stateNone state = iota
	stateMedias
	stateTracks
	stateStart
	stateExternal
	stateInternal
)

type Producer struct {
	core.Listener

	url      string
	template string

	// sei - source has the #sei flag: attach h264.Stamper to H264 tracks
	sei bool

	conn      core.Producer
	receivers []*core.Receiver
	senders   []*core.Receiver

	state    state
	mu       sync.Mutex
	workerID int
}

const SourceTemplate = "{input}"

func NewProducer(source string) *Producer {
	source, sei := cutSEI(source)

	if strings.Contains(source, SourceTemplate) {
		return &Producer{template: source, sei: sei}
	}

	return &Producer{url: source, sei: sei}
}

// cutSEI removes the "#sei" flag from a source URL. The flag is handled here
// and never reaches the source handlers (so it also survives redirects, e.g.
// onvif:// -> rtsp://). Other #params are kept in their original order.
func cutSEI(source string) (string, bool) {
	i := strings.IndexByte(source, '#')
	if i < 0 {
		return source, false
	}

	var found bool
	params := strings.Split(source[i+1:], "#")
	kept := params[:0]
	for _, param := range params {
		if param == "sei" {
			found = true
			continue
		}
		kept = append(kept, param)
	}

	if !found {
		return source, false
	}
	if len(kept) == 0 {
		return source[:i], true
	}
	return source[:i] + "#" + strings.Join(kept, "#"), true
}

// attachStamper enables SEI timestamps on a new H264 track. On reconnect the
// stamper of the previous track is reused (reset), so consumers keep working.
func (p *Producer) attachStamper(track, prev *core.Receiver) {
	if !p.sei || track.Codec.Name != core.CodecH264 {
		return
	}

	if prev != nil {
		if stamper, ok := prev.Stamper.(*h264.Stamper); ok {
			stamper.Reset()
			track.Stamper = stamper
			return
		}
	}

	track.Stamper = h264.NewStamper(track.Codec.ClockRate)
	log.Debug().Msgf("[streams] sei stamper on url=%s", p.url)
}

func (p *Producer) SetSource(s string) {
	if p.template == "" {
		p.url, p.sei = cutSEI(s)
	} else {
		p.url = strings.Replace(p.template, SourceTemplate, s, 1)
	}
}

func (p *Producer) Dial() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		conn, err := GetProducer(p.url)
		if err != nil {
			return err
		}

		p.conn = conn
		p.state = stateMedias
	}

	return nil
}

func (p *Producer) GetMedias() []*core.Media {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil {
		return nil
	}

	return p.conn.GetMedias()
}

func (p *Producer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		return nil, errors.New("get track from none state")
	}

	for _, track := range p.receivers {
		if track.Codec == codec {
			return track, nil
		}
	}

	track, err := p.conn.GetTrack(media, codec)
	if err != nil {
		return nil, err
	}

	p.attachStamper(track, nil)

	p.receivers = append(p.receivers, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	return track, nil
}

func (p *Producer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == stateNone {
		return errors.New("add track from none state")
	}

	if err := p.conn.(core.Consumer).AddTrack(media, codec, track); err != nil {
		return err
	}

	p.senders = append(p.senders, track)

	if p.state == stateMedias {
		p.state = stateTracks
	}

	return nil
}

func (p *Producer) MarshalJSON() ([]byte, error) {
	if conn := p.conn; conn != nil {
		return json.Marshal(conn)
	}
	info := map[string]string{"url": p.url}
	return json.Marshal(info)
}

// internals

func (p *Producer) start() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != stateTracks {
		return
	}

	log.Debug().Msgf("[streams] start producer url=%s", p.url)

	p.state = stateStart
	p.workerID++

	go p.worker(p.conn, p.workerID)
}

func (p *Producer) worker(conn core.Producer, workerID int) {
	if err := conn.Start(); err != nil {
		p.mu.Lock()
		closed := p.workerID != workerID
		p.mu.Unlock()

		if closed {
			return
		}

		log.Warn().Err(err).Str("url", p.url).Caller().Send()
	}

	p.reconnect(workerID, 0)
}

func (p *Producer) reconnect(workerID, retry int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.workerID != workerID {
		log.Trace().Msgf("[streams] stop reconnect url=%s", p.url)
		return
	}

	log.Debug().Msgf("[streams] retry=%d to url=%s", retry, p.url)

	conn, err := GetProducer(p.url)
	if err != nil {
		log.Debug().Msgf("[streams] producer=%s", err)

		timeout := time.Minute
		if retry < 5 {
			timeout = time.Second
		} else if retry < 10 {
			timeout = time.Second * 5
		} else if retry < 20 {
			timeout = time.Second * 10
		}

		time.AfterFunc(timeout, func() {
			p.reconnect(workerID, retry+1)
		})
		return
	}

	for _, media := range conn.GetMedias() {
		switch media.Direction {
		case core.DirectionRecvonly:
			for i, receiver := range p.receivers {
				codec := media.MatchCodec(receiver.Codec)
				if codec == nil {
					continue
				}

				track, err := conn.GetTrack(media, codec)
				if err != nil {
					continue
				}

				p.attachStamper(track, receiver)

				receiver.Replace(track)
				p.receivers[i] = track
				break
			}

		case core.DirectionSendonly:
			for _, sender := range p.senders {
				codec := media.MatchCodec(sender.Codec)
				if codec == nil {
					continue
				}

				_ = conn.(core.Consumer).AddTrack(media, codec, sender)
			}
		}
	}

	// stop previous connection after moving tracks (fix ghost exec/ffmpeg)
	_ = p.conn.Stop()
	// swap connections
	p.conn = conn

	go p.worker(conn, workerID)
}

func (p *Producer) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch p.state {
	case stateExternal:
		log.Trace().Msgf("[streams] skip stop external producer")
		return
	case stateNone:
		log.Trace().Msgf("[streams] skip stop none producer")
		return
	case stateStart:
		p.workerID++
	}

	log.Debug().Msgf("[streams] stop producer url=%s", p.url)

	if p.conn != nil {
		_ = p.conn.Stop()
		p.conn = nil
	}

	p.state = stateNone
	p.receivers = nil
	p.senders = nil
}
