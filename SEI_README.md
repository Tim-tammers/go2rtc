# SEI Frame Timestamps

go2rtc can embed a **UTC timestamp and a frame counter** in H.264 video as an SEI message. Every viewer of a stream receives the same values for the same frame, so the stream itself becomes the source of truth for "when was this frame captured".

- Turned on **per stream source** with a `#sei` flag. Streams without it are unchanged.
- Written on **keyframes only**.
- Supported outputs: **RTSP** and **WebRTC**. MSE, HLS, MP4 and MJPEG outputs do not carry it yet.
- Codec: **H.264 only**. H.265 tracks are left untouched.

---

## 1. Requirements

| Requirement | Why |
|---|---|
| Camera/source sends **H.264** | The stamper only attaches to H.264 tracks. Many cameras default to H.265: switch the profile in the camera settings, or use a substream that is already H.264. |
| Host clock synced with **NTP/chrony** | The timestamp is the go2rtc host's UTC clock. A Docker container uses the host's clock and cannot correct it. |
| Linux host for production | Docker Desktop on macOS/Windows runs in a VM whose clock can drift (especially after sleep). Fine for development, not for accuracy testing. |

Check the host clock on Linux:

```sh
timedatectl status        # "System clock synchronized: yes"
chronyc tracking          # if chrony is used: "System time" offset should be well under 1 ms
```

---

## 2. Enable SEI on a stream

Add `#sei` to the end of the source URL. It can be combined with any other `#param`. go2rtc removes it before connecting, so the camera never sees it.

```yaml
streams:
  # RTSP camera
  front_door: rtsp://user:pass@<address>:554/stream1#sei

  # ONVIF camera: the flag survives the ONVIF -> RTSP lookup
  yard: onvif://user:pass@<address>:8000?subtype=mainStream_Profile_Token#sei

  # combined with other params
  garage: ffmpeg:rtsp://<address>/live#video=h264#sei

  # low-resolution preview that is never saved: leave the flag off
  yard_preview: onvif://user:pass@<address>:8000?subtype=subStream_Profile_Token
```

Restart go2rtc after editing `go2rtc.yaml`.

**Temporary stream from the web UI or a URL:** the `#` must be written as `%23`, otherwise the browser treats it as a page anchor and never sends it:

```
http://localhost:1984/stream.html?src=rtsp://user:pass@<address>:554/stream1%23sei
```

**Through the API** (also saved to `go2rtc.yaml`):

```sh
curl -X PUT "http://localhost:1984/api/streams?name=front_door&src=rtsp://user:pass@<address>:554/stream1%23sei"
```

---

## 3. Run

### Locally (development)

```sh
go run .            # reads ./go2rtc.yaml
```

### Docker (production)

Build an image from this branch, since the official image does not include this feature:

```sh
docker build -f docker/Dockerfile -t go2rtc-sei .
```

Run it with the config folder mounted. Use host networking so WebRTC works and cameras on the LAN are reachable:

```sh
docker run -d --name go2rtc \
  --network host \
  --restart unless-stopped \
  -v ~/go2rtc:/config \
  go2rtc-sei
```

or with Compose:

```yaml
services:
  go2rtc:
    image: go2rtc-sei
    container_name: go2rtc
    network_mode: host
    restart: unless-stopped
    volumes:
      - ~/go2rtc:/config      # contains go2rtc.yaml
```

Ports: `1984` web UI/API, `8554` RTSP, `8555` WebRTC (TCP/UDP).

---

## 4. Verify

### Is the source H.264?

```sh
curl -s "http://localhost:1984/api/streams?src=front_door"
```

Under `producers[].receivers[]`, the video `codec_name` must be `h264`. It only shows up while someone is watching the stream.

### Are the timestamps in the stream?

This pulls the RTSP output and prints one line per keyframe until Ctrl+C. It works on macOS and Linux. Inside the Docker image, prefix it with `docker exec -it go2rtc sh -c '...'`, since the image includes ffmpeg.

```sh
ffmpeg -hide_banner -rtsp_transport tcp -i rtsp://localhost:8554/front_door \
  -map 0:v -c copy -bsf:v trace_headers -f null - 2>&1 | awk '
/User Data Unregistered/ { uuid=""; ours=0 }
/uuid_iso_iec_11578\[/   { uuid = uuid sprintf("%02X", $NF); if (uuid == "008A10A200FC4860B3D3FFE71CC12842") ours=1 }
ours && /user_data_payload_byte\[/ {
  match($0, /\[[0-9]+\]/); i = substr($0, RSTART+1, RLENGTH-2) + 0
  if (i == 0) us = 0; else if (i <= 8) us = us*256 + $NF
  else { printf "frame=%3d  unix_us=%d  utc=%s.%06dZ\n", $NF, us, utc(int(us/1e6)), us%1e6; fflush(); ours=0 }
}
function utc(s,   cmd, out) {
  cmd = "date -u -r " s " +%Y-%m-%dT%H:%M:%S 2>/dev/null || date -u -d @" s " +%Y-%m-%dT%H:%M:%S"
  cmd | getline out; close(cmd); return out
}'
```

Expected output, with a keyframe every 2 s at 30 fps:

```
frame=130  unix_us=1793919167155325  utc=2026-11-05T22:52:47.155325Z
frame=190  unix_us=1793919169154198  utc=2026-11-05T22:52:49.154198Z
frame=250  unix_us=1793919171154687  utc=2026-11-05T22:52:51.154687Z
frame= 54  unix_us=1793919173155765  utc=2026-11-05T22:52:53.155765Z
```

Check that:

- the `frame` step matches the camera's keyframe interval (GOP) and wraps 255 → 0;
- `utc` is close to the current UTC time;
- two copies of the command running at the same time print **identical** lines.

### Does it still decode cleanly?

```sh
ffmpeg -v error -rtsp_transport tcp -i rtsp://localhost:8554/front_door -t 10 -f null -
```

It should print nothing. VLC (`rtsp://<host>:8554/front_door`) should play normally. VLC ignores the SEI, so it shows no timestamp.

---

## 5. SEI format (for client/app developers)

The timestamp is an H.264 `user_data_unregistered` SEI message (payloadType 5, ITU-T H.264 §D.1.6). It is placed in each keyframe access unit just before the first slice:

```
[AUD] [SPS] [PPS] [other SEI] [timestamp SEI] [IDR slice] ...
```

### NAL unit

```
0x06                NAL header (nal_ref_idc=0, type 6 = SEI)
0x05                payloadType  = user_data_unregistered
0x1A                payloadSize  = 26
<26-byte payload>
0x80                rbsp_trailing_bits
```

Emulation prevention bytes (`00 00 03`) are applied to everything after the NAL header. **Remove them before reading the payload**: wherever `00 00 03` appears, drop the `03`.

### Payload (26 bytes)

| Offset | Size | Field | Notes |
|---|---|---|---|
| 0 | 16 | UUID | `008A10A2-00FC-4860-B3D3-FFE71CC12842`. Ignore SEI messages with any other UUID: cameras and encoders write their own. |
| 16 | 1 | version | `0x01` |
| 17 | 8 | unix_micros | uint64 big-endian, **UTC microseconds** since the Unix epoch |
| 25 | 1 | frame_counter | uint8, counts **every** frame, wraps 255 → 0 |

### Parsing steps

1. Split the access unit into NAL units: Annex-B start codes, or 4-byte AVCC length prefixes depending on your container.
2. For each NAL with `(byte0 & 0x1F) == 6`, remove the emulation prevention bytes from the rest of the NAL.
3. Walk the SEI messages. payloadType and payloadSize are each read as a run of `0xFF` bytes (+255 each) plus one final byte. Stop at the `0x80` trailing byte.
4. Take the message with type 5, size 26 and the UUID above. Check that version == 1, then read `unix_micros` and `frame_counter`.

The reference implementation is `ParseSEI` in [`pkg/h264/sei.go`](pkg/h264/sei.go).

### Timing frames between keyframes

Only keyframes carry the SEI. For the frames after a keyframe, use the RTP timestamp, which runs on a 90 kHz clock for H.264:

```
frame_time_us = keyframe_unix_micros + (frame_rtp_ts - keyframe_rtp_ts) * 1_000_000 / 90_000
frame_number  = (keyframe_frame_counter + frames_since_keyframe) mod 256
```

Use a signed 32-bit difference for `frame_rtp_ts - keyframe_rtp_ts` so the RTP timestamp wraparound is handled. Clients should compute times this way from the stream instead of using their own local clocks.

---

## 6. How the timestamp is produced

- **Capture time = arrival at go2rtc**, measured with the host's UTC clock. go2rtc cannot see the camera's sensor time, so network delay from the camera to go2rtc (a few ms on a LAN) is included.
- **Anchored to the RTP clock:** time = `offset + RTP elapsed`. Network jitter therefore does not show up in the timestamps. Frame-to-frame spacing follows the camera's own clock.
- **Drift correction:** every 5 s, `offset` is updated to the lowest-delay arrival seen in that window. This follows camera clock drift, which would otherwise be up to ~360 ms/hour at 100 ppm.
- **Discontinuities:** if a frame's arrival differs from its computed time by more than 1 s (camera restart, RTP jump, long stall), the stamper re-anchors immediately.
- **Assigned once per stream:** the stamp is computed on the producer side. Every consumer looks it up by RTP timestamp, so all viewers get identical values even if one is buffering several frames behind.
- **Reconnects:** the same stamper is reused and re-anchored on the first new frame. The frame counter keeps counting.

---

## 7. Behavior and limitations

- **RTSP passthrough changes for `#sei` streams.** RTSP output is normally passed straight through. With `#sei`, go2rtc depacketizes, stamps and repacketizes it, so sequence numbers and packet sizes differ from the camera's. The cost is a little CPU per viewer. Streams without `#sei` are not affected.
- **Viewers more than 256 frames behind** (about 8 s at 30 fps) receive keyframes without the SEI rather than with a wrong value.
- **Known accuracy issue:** each 5 s anchor update can move the timestamps by up to ~1.5 ms in one step. A fix that applies the correction gradually is planned.
- **Not yet supported:** MSE (browser player in the go2rtc web UI), HLS, MP4/recording outputs, and H.265.

---

## 8. Code map

| File | Role |
|---|---|
| [`pkg/h264/sei.go`](pkg/h264/sei.go) | Payload layout, `BuildSEI` / `ParseSEI`, emulation prevention, `InsertSEI` |
| [`pkg/h264/stamper.go`](pkg/h264/stamper.go) | `Stamper` (time/frame assignment, drift handling) and the `SEIStamp` consumer filter |
| [`pkg/core/track.go`](pkg/core/track.go) | `SEIStamper` interface. `Receiver` passes every packet to its stamper |
| [`internal/streams/producer.go`](internal/streams/producer.go) | Removes the `#sei` flag, attaches the stamper to H.264 tracks, keeps it across reconnects |
| [`pkg/webrtc/consumer.go`](pkg/webrtc/consumer.go) | Adds `SEIStamp` to the WebRTC output |
| [`pkg/rtsp/consumer.go`](pkg/rtsp/consumer.go) | Adds `SEIStamp` to the RTSP output (depay → stamp → repay) |

Tests: `go test ./pkg/h264/ ./internal/streams/ -run 'SEI|Stamper'`
