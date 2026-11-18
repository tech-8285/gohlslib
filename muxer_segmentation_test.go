package gohlslib_test

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcmpegts "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	mccodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

// Synthetic content used by the segmentation tests below.
//
// The three tracks deliberately do NOT share a common origin or cadence:
//
//   - video: 250ms frames (90kHz), IDR every 2s, first frame at t=0
//   - AAC:   ~23.22ms access units (1024/44100), first AU at t=0
//   - Opus:  20ms packets (960/48000), first packet at t=0.3s
//
// Segment boundaries are therefore set by video random access points only,
// while both audio tracks cut on their own, unrelated sample grids.
// Every audio AU carries a 3-byte synthetic payload encoding its source
// index, so the actual media output can be checked for lost, duplicated or
// corrupted data across segment/part cuts.
const (
	segTestVideoFrames    = 25 // t = 0..6s, 250ms cadence
	segTestIDRPeriod      = 8  // one IDR every 8 frames (= every 2s)
	segTestAUAACCount     = 300
	segTestAUOpusCount    = 340
	segTestOpusStartTS    = 14400 // 0.3s in 48kHz units
	segTestSegmentMinDur  = 2 * time.Second
	segTestPartMinDur     = 250 * time.Millisecond
	segTestFMP4SegCount   = 3
	segTestLLSegCount     = 7
	segTestMPEGTSSegCount = 3
)

func segTestH264Track() *gohlslib.Track {
	return &gohlslib.Track{
		Codec:     &codecs.H264{SPS: testH264SPS, PPS: []byte{0x08}},
		ClockRate: 90000,
	}
}

func segTestAACTrack(name, language string) *gohlslib.Track {
	return &gohlslib.Track{
		Codec:     &codecs.MPEG4Audio{Config: testAACConfig},
		ClockRate: 44100,
		Name:      name,
		Language:  language,
	}
}

func segTestOpusTrack() *gohlslib.Track {
	return &gohlslib.Track{
		Codec:     &codecs.Opus{ChannelCount: 2},
		ClockRate: 48000,
	}
}

// segAUIndex decodes the synthetic payload back to its source AU index.
func segAUIndex(payload []byte) int {
	return int(payload[1]) | int(payload[2])<<8
}

func segAACPayload(index int) []byte {
	return []byte{0xAA, byte(index), byte(index >> 8)}
}

func segOpusPayload(index int) []byte {
	return []byte{0x78, byte(index), byte(index >> 8)}
}

// segTickDuration converts a timestamp in clock-rate units to a duration.
func segTickDuration(ticks int64, clockRate int) time.Duration {
	return time.Duration(ticks) * time.Second / time.Duration(clockRate)
}

type segTestEvent struct {
	track int // 0=video, 1=AAC, 2=Opus
	ntp   time.Time
	pts   int64
	index int
}

// segEventTime returns the media time of an event in seconds.
func segEventTime(e segTestEvent) float64 {
	switch e.track {
	case 0:
		return float64(e.pts) / 90000
	case 1:
		return float64(e.pts) / 44100
	default:
		return float64(e.pts) / 48000
	}
}

// segBuildTimeline builds a single, deterministically ordered interleaved
// timeline for the three tracks. Events at the same instant keep a stable
// video -> AAC -> Opus order.
func segBuildTimeline(t *testing.T) []segTestEvent {
	t.Helper()

	events := make([]segTestEvent, 0, segTestVideoFrames+segTestAUAACCount+segTestAUOpusCount)

	for f := range segTestVideoFrames {
		events = append(events, segTestEvent{
			track: 0,
			ntp:   testTime.Add(time.Duration(f) * 250 * time.Millisecond),
			pts:   int64(f) * 22500, // 90kHz / 4
			index: f,
		})
	}

	for i := range segTestAUAACCount {
		pts := int64(i) * mpeg4audio.SamplesPerAccessUnit
		events = append(events, segTestEvent{
			track: 1,
			ntp:   testTime.Add(segTickDuration(pts, 44100)),
			pts:   pts,
			index: i,
		})
	}

	for i := range segTestAUOpusCount {
		pts := int64(segTestOpusStartTS + i*960)
		events = append(events, segTestEvent{
			track: 2,
			ntp:   testTime.Add(segTickDuration(pts, 48000)),
			pts:   pts,
			index: i,
		})
	}

	slices.SortFunc(events, func(a, b segTestEvent) int {
		x, y := segEventTime(a), segEventTime(b)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		case a.track < b.track:
			return -1
		case a.track > b.track:
			return 1
		default:
			return 0
		}
	})

	return events
}

func segWriteTimeline(
	t *testing.T,
	m *gohlslib.Muxer,
	video, aac, opus *gohlslib.Track,
	events []segTestEvent,
) {
	t.Helper()

	for _, e := range events {
		var err error

		switch e.track {
		case 0:
			var au [][]byte
			if e.index%segTestIDRPeriod == 0 {
				au = [][]byte{testH264SPS, {0x08}, {0x05}}
			} else {
				au = [][]byte{{0x01}}
			}
			err = m.WriteH264(video, e.ntp, e.pts, au)

		case 1:
			err = m.WriteMPEG4Audio(aac, e.ntp, e.pts, [][]byte{segAACPayload(e.index)})

		case 2:
			err = m.WriteOpus(opus, e.ntp, e.pts, [][]byte{segOpusPayload(e.index)})
		}

		require.NoError(t, err)
	}
}

// segPlaylistURIs returns the segment URIs advertised by a media playlist.
func segPlaylistURIs(t *testing.T, m *gohlslib.Muxer, playlist string) []string {
	t.Helper()

	byts, _, err := doRequest(m, playlist)
	require.NoError(t, err)

	re := regexp.MustCompile(`[0-9a-f]+_[A-Za-z0-9]+_seg[0-9]+\.(?:mp4|ts)`)
	matches := re.FindAllString(string(byts), -1)
	require.NotEmpty(t, matches)
	return matches
}

// segPartURIs returns the part URIs advertised by EXT-X-PART tags,
// excluding the EXT-X-PRELOAD-HINT entry which is not finalized yet.
func segPartURIs(t *testing.T, m *gohlslib.Muxer, playlist string) []string {
	t.Helper()

	byts, _, err := doRequest(m, playlist)
	require.NoError(t, err)

	re := regexp.MustCompile(`#EXT-X-PART:[^\n]*URI="([0-9a-f]+_[A-Za-z0-9]+_part[0-9]+\.mp4)"`)
	groups := re.FindAllSubmatch(byts, -1)
	require.NotEmpty(t, groups)

	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = string(g[1])
	}
	return out
}

// fmp4TrackSpan describes the actual playback interval covered by a track
// across the parts of an fMP4 segment, in the track's own clock-rate units.
type fmp4TrackSpan struct {
	baseTime       uint64
	endTime        uint64
	sampleCount    int
	firstNonSync   bool
	payloadIndices []int
}

// segFMP4TrackSpans parses an fMP4 part/segment file and returns, per track ID,
// the covered interval, the sample count and every source-AU index.
func segFMP4TrackSpans(t *testing.T, byts []byte) map[int]*fmp4TrackSpan {
	t.Helper()

	var parts fmp4.Parts
	err := parts.Unmarshal(byts)
	require.NoError(t, err)

	out := make(map[int]*fmp4TrackSpan)

	for _, part := range parts {
		for _, tr := range part.Tracks {
			span, ok := out[tr.ID]
			if !ok {
				span = &fmp4TrackSpan{baseTime: tr.BaseTime}
				out[tr.ID] = span
			}

			for i, sample := range tr.Samples {
				if len(span.payloadIndices) == 0 && i == 0 {
					span.firstNonSync = sample.IsNonSyncSample
				}
				span.sampleCount++
				span.payloadIndices = append(span.payloadIndices, segAUIndex(sample.Payload))
			}

			// end time of this part = BaseTime + sum of sample durations;
			// a rendition contains a single track, so take the furthest end
			var partDuration uint64
			for _, sample := range tr.Samples {
				partDuration += uint64(sample.Duration)
			}
			if tr.BaseTime+partDuration > span.endTime {
				span.endTime = tr.BaseTime + partDuration
			}
		}
	}

	return out
}

// segAssertContiguousIndices fails if the indices are not a contiguous run.
func segAssertContiguousIndices(t *testing.T, indices []int, what string) {
	t.Helper()
	require.NotEmpty(t, indices)
	for i := 1; i < len(indices); i++ {
		require.Equal(t, indices[i-1]+1, indices[i],
			"%s: non-contiguous AU indices at position %d", what, i)
	}
}

// TestMuxerSegmentationFMP4MultiTrack verifies segmentation driven by video
// random access points when the video, AAC and Opus tracks have different
// origins and sample cadences (fMP4 variant).
func TestMuxerSegmentationFMP4MultiTrack(t *testing.T) {
	video := segTestH264Track()
	aac := segTestAACTrack("", "")
	opus := segTestOpusTrack()

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantFMP4,
		SegmentCount:       segTestFMP4SegCount,
		SegmentMinDuration: segTestSegmentMinDur,
		Tracks:             []*gohlslib.Track{video, aac, opus},
	}
	require.NoError(t, m.Start())
	defer m.Close()

	segWriteTimeline(t, m, video, aac, opus, segBuildTimeline(t))

	// one playlist per track, all advertising the same number of segments
	videoURIs := segPlaylistURIs(t, m, "video1_stream.m3u8")
	aacURIs := segPlaylistURIs(t, m, "audio2_stream.m3u8")
	opusURIs := segPlaylistURIs(t, m, "audio3_stream.m3u8")
	require.Len(t, videoURIs, segTestFMP4SegCount)
	require.Len(t, aacURIs, segTestFMP4SegCount)
	require.Len(t, opusURIs, segTestFMP4SegCount)

	// ---- video track -----------------------------------------------------
	var previousVideoEnd uint64
	allVideoAU := make([][][]byte, 0, 24)

	for segIdx, uri := range videoURIs {
		byts, _, err := doRequest(m, uri)
		require.NoError(t, err)

		spans := segFMP4TrackSpans(t, byts)
		require.Contains(t, spans, 1)
		span := spans[1]

		// each video segment spans exactly two seconds of the 90kHz clock
		require.Equal(t, uint64((10+2*segIdx)*90000), span.baseTime,
			"segment %d video BaseTime", segIdx)
		require.Equal(t, uint64((12+2*segIdx)*90000), span.endTime,
			"segment %d video end time", segIdx)
		require.Equal(t, 8, span.sampleCount)

		// first sample of every segment is a sync sample (IDR), so segments
		// are independently decodable
		require.False(t, span.firstNonSync, "segment %d must start on a sync sample", segIdx)

		if segIdx > 0 {
			require.Equal(t, previousVideoEnd, span.baseTime, "video segments must be contiguous")
		}
		previousVideoEnd = span.endTime

		// encoded data must be untouched: every sample is still valid AVCC
		var parts fmp4.Parts
		require.NoError(t, parts.Unmarshal(byts))
		for _, part := range parts {
			for _, tr := range part.Tracks {
				for _, sample := range tr.Samples {
					var avcc h264.AVCC
					require.NoError(t, avcc.Unmarshal(sample.Payload))
					require.NotEmpty(t, avcc)
					allVideoAU = append(allVideoAU, [][]byte(avcc))
				}
			}
		}
	}

	// exactly one IDR per segment, never elsewhere
	idrCount := 0
	for _, au := range allVideoAU {
		for _, nalu := range au {
			if h264.NALUType(nalu[0]) == h264.NALUTypeIDR {
				idrCount++
			}
		}
	}
	require.Equal(t, segTestFMP4SegCount, idrCount)
	require.Len(t, allVideoAU, 24)

	// ---- audio tracks ----------------------------------------------------
	for _, tc := range []struct {
		name      string
		playlist  string
		uris      []string
		clockRate int64
		// duration of one AU in the track's clock-rate units
		auStep int64
		// expected source-AU index range of each finalized segment
		expectedRanges [3][2]int
		payloadFor     func(int) []byte
	}{
		{
			name:           "aac",
			playlist:       "audio2_stream.m3u8",
			uris:           aacURIs,
			clockRate:      44100,
			auStep:         mpeg4audio.SamplesPerAccessUnit,
			expectedRanges: [3][2]int{{10, 85}, {86, 171}, {172, 257}},
			payloadFor:     segAACPayload,
		},
		{
			name:           "opus",
			playlist:       "audio3_stream.m3u8",
			uris:           opusURIs,
			clockRate:      48000,
			auStep:         960,
			expectedRanges: [3][2]int{{0, 83}, {84, 183}, {184, 283}},
			payloadFor:     segOpusPayload,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var previousEnd uint64
			seenIndices := make(map[int]struct{})

			for segIdx, uri := range tc.uris {
				byts, _, err := doRequest(m, uri)
				require.NoError(t, err)

				spans := segFMP4TrackSpans(t, byts)
				require.Contains(t, spans, 1, "rendition has a single track with ID 1")
				span := spans[1]

				// every audio sample is independently decodable
				require.False(t, span.firstNonSync)

				// segment duration is two seconds on the track clock.
				// For segments created on a video cut, BaseTime may lead the
				// nominal cut by at most one AU: the AU in flight when the cut
				// happens is flushed into the new segment together with its
				// BaseTime. The very first segment instead starts at the first
				// sample that arrives after the leading track creates it, so
				// its BaseTime may lag the nominal origin (audio samples
				// written before the first segment exists are discarded).
				nominalBase := uint64(int64(10+2*segIdx) * tc.clockRate)
				nominalEnd := uint64(int64(12+2*segIdx) * tc.clockRate)
				if segIdx == 0 {
					// the first segment starts when the leading (video) track
					// emits its first random access point; audio samples written
					// before that moment are discarded, so its BaseTime may lag
					// the nominal origin by up to one segment's worth of AUs.
					require.GreaterOrEqual(t, span.baseTime, nominalBase,
						"first segment BaseTime cannot precede the segment origin")
					require.LessOrEqual(t, span.baseTime,
						nominalBase+uint64(segTestSegmentMinDur/time.Millisecond)*uint64(tc.clockRate)/1000)
				} else {
					require.GreaterOrEqual(t, span.baseTime, nominalBase-uint64(tc.auStep))
					require.LessOrEqual(t, span.baseTime, nominalBase+1)
				}
				require.InDelta(t, nominalEnd, span.endTime, float64(tc.auStep))

				// segments are contiguous on the track timeline:
				// the next one starts exactly where the previous one ended
				if segIdx > 0 {
					require.Equal(t, previousEnd, span.baseTime,
						"audio segment %d must start exactly where the previous one ended", segIdx)
				}
				previousEnd = span.endTime

				// samples inside one segment are a contiguous run of source AUs
				lo, hi := tc.expectedRanges[segIdx][0], tc.expectedRanges[segIdx][1]
				segAssertContiguousIndices(t, span.payloadIndices,
					tc.name+" segment "+strconv.Itoa(segIdx))
				require.Equal(t, lo, span.payloadIndices[0])
				require.Equal(t, hi, span.payloadIndices[len(span.payloadIndices)-1])
				require.Len(t, span.payloadIndices, hi-lo+1)

				var parts fmp4.Parts
				require.NoError(t, parts.Unmarshal(byts))
				samplePos := 0
				for _, part := range parts {
					for _, tr := range part.Tracks {
						for j, sample := range tr.Samples {
							idx := span.payloadIndices[samplePos]
							samplePos++

							// no AU is ever emitted twice across segments
							_, duplicated := seenIndices[idx]
							require.False(t, duplicated, "AU %d duplicated", idx)
							seenIndices[idx] = struct{}{}

							// the encoded data survives byte-for-byte (never split)
							require.Equal(t, tc.payloadFor(idx), sample.Payload,
								"part %d track %d sample %d", part.SequenceNumber, tr.ID, j)
						}
					}
				}
			}

			// all expected AUs are present: nothing valid was dropped at the cuts
			for _, rng := range tc.expectedRanges {
				for auIdx := rng[0]; auIdx <= rng[1]; auIdx++ {
					_, ok := seenIndices[auIdx]
					require.True(t, ok, "AU %d missing from output", auIdx)
				}
			}
			require.Len(t, seenIndices,
				tc.expectedRanges[len(tc.expectedRanges)-1][1]-
					tc.expectedRanges[0][0]+1)
		})
	}

	// advertised segment durations must be consistent with the actual media:
	// all tracks show 2s segments regardless of their sample cadence
	for _, playlist := range []string{
		"video1_stream.m3u8",
		"audio2_stream.m3u8",
		"audio3_stream.m3u8",
	} {
		byts, _, err := doRequest(m, playlist)
		require.NoError(t, err)
		require.Equal(t, segTestFMP4SegCount,
			len(regexp.MustCompile(`#EXTINF:2\.00000,`).FindAll(byts, -1)),
			"playlist %s", playlist)
		require.Contains(t, string(byts), "#EXT-X-TARGETDURATION:2\n")
	}

	// multivariant playlist references all three renditions and signals
	// independent segments
	index, _, err := doRequest(m, "index.m3u8")
	require.NoError(t, err)
	require.Contains(t, string(index), "video1_stream.m3u8")
	require.Contains(t, string(index), "audio2_stream.m3u8")
	require.Contains(t, string(index), "audio3_stream.m3u8")
	require.Contains(t, string(index), "#EXT-X-INDEPENDENT-SEGMENTS")
}

// TestMuxerSegmentationLowLatencyParts verifies that low-latency parts rotate
// on the video grid while audio renditions keep their own, unaligned cadence,
// and that part boundaries never duplicate or lose audio access units.
func TestMuxerSegmentationLowLatencyParts(t *testing.T) {
	video := segTestH264Track()
	aac := segTestAACTrack("", "")
	opus := segTestOpusTrack()

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantLowLatency,
		SegmentCount:       segTestLLSegCount,
		SegmentMinDuration: segTestSegmentMinDur,
		PartMinDuration:    segTestPartMinDur,
		Tracks:             []*gohlslib.Track{video, aac, opus},
	}
	require.NoError(t, m.Start())
	defer m.Close()

	segWriteTimeline(t, m, video, aac, opus, segBuildTimeline(t))

	// playlist-level declarations are shared by every rendition
	for _, playlist := range []string{
		"video1_stream.m3u8",
		"audio2_stream.m3u8",
		"audio3_stream.m3u8",
	} {
		byts, _, err := doRequest(m, playlist)
		require.NoError(t, err)
		text := string(byts)
		require.Contains(t, text, "#EXT-X-PART-INF:PART-TARGET=0.25000\n")
		require.Contains(t, text, "#EXT-X-TARGETDURATION:2\n")
		require.Contains(t, text, "#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES")

		partLines := regexp.MustCompile(`#EXT-X-PART:[^\n]*`).FindAllString(text, -1)
		require.NotEmpty(t, partLines)

		// Every audio sample is a sync sample, so every part of an audio
		// rendition is independently decodable. The video rendition only marks
		// parts starting on an IDR (the first part of each parent segment).
		if playlist != "video1_stream.m3u8" {
			for _, line := range partLines {
				require.Contains(t, line, "INDEPENDENT=YES", playlist)
			}
		}
	}

	// in the video rendition, independent parts are exactly the first part of
	// each parent segment (one every 8 parts, aligned with the IDR frames)
	videoPlaylist, _, err := doRequest(m, "video1_stream.m3u8")
	require.NoError(t, err)
	videoPartLines := regexp.MustCompile(`#EXT-X-PART:[^\n]*`).FindAllString(string(videoPlaylist), -1)
	for i, line := range videoPartLines {
		if i%8 == 0 {
			require.Contains(t, line, "INDEPENDENT=YES", "video part %d must be independent", i)
		} else {
			require.NotContains(t, line, "INDEPENDENT=YES", "video part %d must not be independent", i)
		}
	}

	// inspect the part files of every rendition
	for _, tc := range []struct {
		name     string
		playlist string
		isVideo  bool
	}{
		{"video", "video1_stream.m3u8", true},
		{"aac", "audio2_stream.m3u8", false},
		{"opus", "audio3_stream.m3u8", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partURIs := segPartURIs(t, m, tc.playlist)

			var previousEnd uint64
			firstPart := true
			seenBaseTimes := make(map[uint64]struct{})
			seenIndices := make(map[int]struct{})
			totalSamples := 0

			for _, uri := range partURIs {
				partByts, _, reqErr := doRequest(m, uri)
				require.NoError(t, reqErr)

				spans := segFMP4TrackSpans(t, partByts)
				require.Contains(t, spans, 1)
				span := spans[1]

				// a part always contains at least one sample (no empty parts
				// are exposed through EXT-X-PART)
				require.Greater(t, span.sampleCount, 0, "part %s is empty", uri)

				// consecutive parts tile the track timeline without gaps or overlaps
				if !firstPart {
					require.Equal(t, previousEnd, span.baseTime,
						"part %s must continue exactly where the previous one ended", uri)
				}
				firstPart = false
				previousEnd = span.endTime

				// parts never share a BaseTime (no part is emitted twice)
				_, reused := seenBaseTimes[span.baseTime]
				require.False(t, reused, "part with BaseTime %d emitted twice", span.baseTime)
				seenBaseTimes[span.baseTime] = struct{}{}

				var parts fmp4.Parts
				require.NoError(t, parts.Unmarshal(partByts))

				if tc.isVideo {
					// every video part is exactly one 250ms frame
					require.Equal(t, 1, span.sampleCount)
					require.Equal(t, uint64(22500), span.endTime-span.baseTime)
				}

				samplePos := 0
				for _, part := range parts {
					for _, tr := range part.Tracks {
						for _, sample := range tr.Samples {
							idx := span.payloadIndices[samplePos]
							samplePos++
							totalSamples++

							if tc.isVideo {
								// video samples are distinguished by their
								// position on the timeline, already checked above
								continue
							}

							// audio: no AU is ever emitted twice across parts
							_, duplicated := seenIndices[idx]
							require.False(t, duplicated, "AU %d duplicated in part %s", idx, uri)
							seenIndices[idx] = struct{}{}

							// encoded data is never split or corrupted
							var expected []byte
							if tc.name == "aac" {
								expected = segAACPayload(idx)
							} else {
								expected = segOpusPayload(idx)
							}
							require.Equal(t, expected, sample.Payload)
						}
					}
				}
			}

			if tc.isVideo {
				// one frame per part, all distinct on the 250ms grid
				require.Len(t, seenBaseTimes, totalSamples)
			} else {
				// the part files of each audio rendition form one contiguous AU run
				indices := make([]int, 0, len(seenIndices))
				for idx := range seenIndices {
					indices = append(indices, idx)
				}
				slices.Sort(indices)
				segAssertContiguousIndices(t, indices, tc.name+" parts")
				require.Len(t, indices, totalSamples)
			}
		})
	}

	// the parent video segments still cover the exact 2s video intervals and
	// start on an IDR
	videoURIs := segPlaylistURIs(t, m, "video1_stream.m3u8")
	require.NotEmpty(t, videoURIs)
	for segIdx, uri := range videoURIs {
		segByts, _, reqErr := doRequest(m, uri)
		require.NoError(t, reqErr)

		spans := segFMP4TrackSpans(t, segByts)
		span := spans[1]
		require.Equal(t, uint64((10+2*segIdx)*90000), span.baseTime)
		require.Equal(t, uint64((12+2*segIdx)*90000), span.endTime)
		require.False(t, span.firstNonSync)
		require.Equal(t, 8, span.sampleCount)
	}
}

// TestMuxerSegmentationMPEGTSAudioAcrossCuts verifies MPEG-TS segmentation
// driven by H264 random access points: AAC access units straddling the video
// cuts must remain continuous, with no repeated or missing AU, intact encoded
// frames and PTS values that follow the audio cadence across segments.
func TestMuxerSegmentationMPEGTSAudioAcrossCuts(t *testing.T) {
	video := segTestH264Track()
	aac := segTestAACTrack("", "")

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantMPEGTS,
		SegmentCount:       segTestMPEGTSSegCount,
		SegmentMinDuration: segTestSegmentMinDur,
		Tracks:             []*gohlslib.Track{video, aac},
	}
	require.NoError(t, m.Start())
	defer m.Close()

	// interleave video with AAC AUs starting at 0.6s, an origin unrelated to
	// the 2s video cuts
	const aacStartTS = int64(26460) // 0.6s in 44.1kHz units

	type ev struct {
		track int
		ntp   time.Time
		pts   int64
		index int
	}
	events := make([]ev, 0, segTestVideoFrames+segTestAUAACCount)

	for f := range segTestVideoFrames {
		events = append(events, ev{
			track: 0,
			ntp:   testTime.Add(time.Duration(f) * 250 * time.Millisecond),
			pts:   int64(f) * 22500,
			index: f,
		})
	}

	for i := range segTestAUAACCount {
		pts := aacStartTS + int64(i)*mpeg4audio.SamplesPerAccessUnit
		events = append(events, ev{
			track: 1,
			ntp:   testTime.Add(segTickDuration(pts, 44100)),
			pts:   pts,
			index: i,
		})
	}

	slices.SortFunc(events, func(a, b ev) int {
		secs := func(e ev) float64 {
			if e.track == 0 {
				return float64(e.pts) / 90000
			}
			return float64(e.pts) / 44100
		}
		x, y := secs(a), secs(b)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		case a.track < b.track:
			return -1
		case a.track > b.track:
			return 1
		default:
			return 0
		}
	})

	for _, e := range events {
		var err error
		if e.track == 0 {
			var au [][]byte
			if e.index%segTestIDRPeriod == 0 {
				au = [][]byte{testH264SPS, {0x08}, {0x05}}
			} else {
				au = [][]byte{{0x01}}
			}
			err = m.WriteH264(video, e.ntp, e.pts, au)
		} else {
			err = m.WriteMPEG4Audio(aac, e.ntp, e.pts, [][]byte{segAACPayload(e.index)})
		}
		require.NoError(t, err)
	}

	playlist, _, err := doRequest(m, "main_stream.m3u8")
	require.NoError(t, err)
	require.Contains(t, string(playlist), "#EXT-X-TARGETDURATION:2\n")
	require.Len(t, regexp.MustCompile(`#EXTINF:2\.00000,`).FindAll(playlist, -1), segTestMPEGTSSegCount)

	segmentURIs := regexp.MustCompile(`[0-9a-f]+_main_seg[0-9]+\.ts`).FindAllString(string(playlist), -1)
	require.Len(t, segmentURIs, segTestMPEGTSSegCount)

	globalAUIndices := make([]int, 0, segTestMPEGTSSegCount*90)
	globalAudioPTS := make([]int64, 0, segTestMPEGTSSegCount*90)
	perSegmentAudioCount := make([]int, 0, len(segmentURIs))
	idrPerSegment := make([]int, 0, len(segmentURIs))

	for segIdx, uri := range segmentURIs {
		segData, _, err2 := doRequest(m, uri)
		require.NoError(t, err2)

		r := &mcmpegts.Reader{R: bytes.NewReader(segData)}
		require.NoError(t, r.Initialize())

		var videoTrack, audioTrack *mcmpegts.Track
		for _, tr := range r.Tracks() {
			if _, ok := tr.Codec.(*mccodecs.H264); ok {
				videoTrack = tr
			}
			if _, ok := tr.Codec.(*mccodecs.MPEG4Audio); ok {
				audioTrack = tr
			}
		}
		require.NotNil(t, videoTrack)
		require.NotNil(t, audioTrack)

		var (
			videoPTS  []int64
			idrPTS    []int64
			audioPTS  []int64
			auIndices []int
			seen      = make(map[int]struct{})
		)

		r.OnDataH264(videoTrack, func(pts, dts int64, au [][]byte) error {
			require.Equal(t, pts, dts)
			videoPTS = append(videoPTS, pts)
			for _, nalu := range au {
				if h264.NALUType(nalu[0]) == h264.NALUTypeIDR {
					idrPTS = append(idrPTS, pts)
				}
			}
			return nil
		})

		r.OnDataMPEG4Audio(audioTrack, func(pts int64, aus [][]byte) error {
			for _, au := range aus {
				// ADTS framing is stripped by the reader: the remaining AU is
				// exactly the encoded data we wrote, never split or corrupted
				require.Equal(t, segAACPayload(segAUIndex(au)), au)
				idx := segAUIndex(au)
				_, duplicated := seen[idx]
				require.False(t, duplicated, "AU %d duplicated inside segment %d", idx, segIdx)
				seen[idx] = struct{}{}
				auIndices = append(auIndices, idx)
				audioPTS = append(audioPTS, pts)
			}
			return nil
		})

		for {
			err2 = r.Read()
			if errors.Is(err2, io.EOF) {
				break
			}
			require.NoError(t, err2)
		}

		// video: 8 frames per 2s segment, exactly one IDR at its start
		require.Len(t, videoPTS, 8, "segment %d", segIdx)
		require.Equal(t, int64(segIdx*2*90000), videoPTS[0])
		require.Equal(t, int64((segIdx*2+2))*90000-22500, videoPTS[len(videoPTS)-1])
		require.Len(t, idrPTS, 1)
		require.Equal(t, int64(segIdx*2*90000), idrPTS[0])
		idrPerSegment = append(idrPerSegment, len(idrPTS))

		// audio AUs inside the segment are a contiguous index run
		segAssertContiguousIndices(t, auIndices, "mpegts segment "+strconv.Itoa(segIdx))

		// PTS is non-decreasing and follows one AAC frame per AU; integer
		// conversion 44.1kHz -> 90kHz alternates between the two nearest steps
		frameStep := int64(1024 * 90000 / 44100) // 2089
		for i := 1; i < len(audioPTS); i++ {
			delta := audioPTS[i] - audioPTS[i-1]
			require.Contains(t, []int64{frameStep, frameStep + 1}, delta,
				"audio PTS cadence broken inside segment %d", segIdx)
		}

		globalAUIndices = append(globalAUIndices, auIndices...)
		globalAudioPTS = append(globalAudioPTS, audioPTS...)
		perSegmentAudioCount = append(perSegmentAudioCount, len(audioPTS))
	}

	// across all segments: every AU appears exactly once, in order — no
	// repetition and no missing valid interval at the cut points
	require.NotEmpty(t, globalAUIndices)
	segAssertContiguousIndices(t, globalAUIndices, "mpegts all segments")

	// PTS stays continuous across segment boundaries too: the first AU of each
	// new segment follows the last AU of the previous one by one frame step
	frameStep := int64(1024 * 90000 / 44100)
	offset := 0
	for segIdx, count := range perSegmentAudioCount {
		require.LessOrEqual(t, offset+count, len(globalAudioPTS))
		if segIdx > 0 {
			delta := globalAudioPTS[offset] - globalAudioPTS[offset-1]
			require.Contains(t, []int64{frameStep, frameStep + 1}, delta,
				"audio PTS discontinuity at cut before segment %d", segIdx)
		}
		offset += count
	}
	require.Equal(t, 233, len(globalAudioPTS))

	// exactly one IDR per segment
	require.Equal(t, []int{1, 1, 1}, idrPerSegment)

	// the contiguous run length equals the number of muxed AUs
	require.Equal(t,
		globalAUIndices[len(globalAUIndices)-1]-globalAUIndices[0]+1,
		len(globalAUIndices))
}
