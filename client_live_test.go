package gohlslib_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

// clientLiveSnapshot is a version of a live media playlist:
// the playlist window starts at mediaSequence and contains segmentIDs.
type clientLiveSnapshot struct {
	mediaSequence int
	segmentIDs    []int
	endlist       bool
}

func clientLivePlaylist(snapshot clientLiveSnapshot) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n" +
		"#EXT-X-VERSION:7\n" +
		"#EXT-X-TARGETDURATION:1\n" +
		"#EXT-X-MEDIA-SEQUENCE:" + strconv.Itoa(snapshot.mediaSequence) + "\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n")
	for _, id := range snapshot.segmentIDs {
		sb.WriteString("#EXTINF:1,\nseg" + strconv.Itoa(id) + ".mp4\n")
	}
	if snapshot.endlist {
		sb.WriteString("#EXT-X-ENDLIST\n")
	}
	return sb.String()
}

// clientLiveSegment builds a fMP4 segment whose single audio sample
// carries the segment ID as payload, so that tests can identify
// delivered segments by the data they receive.
func clientLiveSegment(t *testing.T, id int) []byte {
	t.Helper()
	var buf seekablebuffer.Buffer
	require.NoError(t, (&fmp4.Part{Tracks: []*fmp4.PartTrack{{
		ID: 1,
		Samples: []*fmp4.Sample{{
			Duration: 44100 / 100,
			Payload:  []byte{byte(id)},
		}},
	}}}).Marshal(&buf))
	return buf.Bytes()
}

// clientLiveServer serves a live stream whose playlist slides forward:
// every playlist request returns the next snapshot, and the last
// snapshot is repeated once the script is exhausted. This mirrors a
// client that lags behind a playlist that is updated by someone else.
func clientLiveServer(t *testing.T, snapshots []clientLiveSnapshot, segmentCount int) *httptest.Server {
	t.Helper()

	init, _ := clientLifecycleMedia(t, false, [][]byte{{0}})

	segments := make(map[string][]byte, segmentCount)
	for i := 0; i < segmentCount; i++ {
		segments["/seg"+strconv.Itoa(i)+".mp4"] = clientLiveSegment(t, i)
	}

	var playlistRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			n := int(playlistRequests.Add(1)) - 1
			if n >= len(snapshots) {
				n = len(snapshots) - 1
			}
			w.Header().Set("Content-Type", `application/vnd.apple.mpegurl`)
			_, _ = io.WriteString(w, clientLivePlaylist(snapshots[n]))

		case "/init.mp4":
			w.Header().Set("Content-Type", `video/mp4`)
			_, _ = w.Write(init)

		default:
			if segment, ok := segments[r.URL.Path]; ok {
				w.Header().Set("Content-Type", `video/mp4`)
				_, _ = w.Write(segment)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	t.Cleanup(server.Close)

	return server
}

type clientLiveCollector struct {
	data chan byte

	mutex      sync.Mutex
	downloaded []string
}

func (c *clientLiveCollector) downloadedSegments() []string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return append([]string(nil), c.downloaded...)
}

// clientLiveStart starts a client at a distance of one segment from the
// end of the playlist and returns a collector of received segment IDs
// and downloaded file names, together with the client termination channel.
func clientLiveStart(
	t *testing.T,
	server *httptest.Server,
	maxDistance int,
) (*clientLiveCollector, <-chan error) {
	t.Helper()

	col := &clientLiveCollector{data: make(chan byte, 32)}

	c := &gohlslib.Client{
		URI:           server.URL + "/index.m3u8",
		StartDistance: 1,
		MaxDistance:   maxDistance,
		OnDownloadSegment: func(u string) {
			col.mutex.Lock()
			defer col.mutex.Unlock()
			col.downloaded = append(col.downloaded, path.Base(u))
		},
	}
	c.OnTracks = func(tracks []*gohlslib.Track) error {
		for _, track := range tracks {
			if _, ok := track.Codec.(*codecs.MPEG4Audio); ok {
				c.OnDataMPEG4Audio(track, func(_ int64, aus [][]byte) {
					for _, au := range aus {
						col.data <- au[0]
					}
				})
			}
		}
		return nil
	}

	require.NoError(t, c.Start())
	t.Cleanup(func() { clientLifecycleClose(t, c) })

	return col, clientLifecycleWait(c)
}

// A live playlist slides forward while the client is reading it.
// The client must deliver each available segment exactly once and in
// order, must not deliver again segments that have already been consumed
// when they leave the window, and must deliver the last available segment
// before entering the end-of-stream state when the playlist ends.
func TestClientLiveSlidingWindow(t *testing.T) {
	server := clientLiveServer(t, []clientLiveSnapshot{
		{mediaSequence: 0, segmentIDs: []int{0, 1, 2}},
		{mediaSequence: 0, segmentIDs: []int{0, 1, 2, 3}},
		{mediaSequence: 0, segmentIDs: []int{0, 1, 2, 3, 4}},
		{mediaSequence: 0, segmentIDs: []int{0, 1, 2, 3, 4, 5}},
		// the window slides forward: segments 0 and 1, both behind the
		// client position, leave the playlist.
		{mediaSequence: 2, segmentIDs: []int{2, 3, 4, 5, 6}},
		// the playlist announces its end while the last segment
		// has not been consumed yet.
		{mediaSequence: 4, segmentIDs: []int{4, 5, 6, 7}, endlist: true},
	}, 8)

	col, done := clientLiveStart(t, server, 3)

	// segments must keep a continuous sequence ownership across playlist
	// updates: the client starts one segment from the live edge (segment 2)
	// and then follows the window without gaps or repetitions.
	for id := 2; id <= 7; id++ {
		require.Equal(t, byte(id), clientLifecycleReceive(t, col.data, "segment "+strconv.Itoa(id)))
	}

	// the last available segment has been delivered in order;
	// only now the client can enter the end-of-stream state.
	require.Same(t, gohlslib.ErrClientEOS, clientLifecycleReceive(t, done, "Wait2"))

	// consumed media must not be delivered again: every file has been
	// downloaded exactly once, in delivery order.
	require.Equal(t, []string{
		"init.mp4",
		"seg2.mp4", "seg3.mp4", "seg4.mp4",
		"seg5.mp4", "seg6.mp4", "seg7.mp4",
	}, col.downloadedSegments())
	require.Empty(t, col.data)
}

// A client position that falls behind the playlist window and a client
// that is temporarily waiting for new segments must produce
// distinguishable results.
func TestClientLivePositionResults(t *testing.T) {
	t.Run("behind window", func(t *testing.T) {
		server := clientLiveServer(t, []clientLiveSnapshot{
			{mediaSequence: 0, segmentIDs: []int{0, 1, 2}},
			// the window suddenly jumps ahead, leaving the client
			// position outside the allowed distance from the live edge.
			{mediaSequence: 0, segmentIDs: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}},
		}, 10)

		col, done := clientLiveStart(t, server, 3)

		require.Equal(t, byte(2), clientLifecycleReceive(t, col.data, "segment 2"))
		require.EqualError(t, clientLifecycleReceive(t, done, "Wait2"), "playback is too late")

		// segments past the allowed distance must not be downloaded.
		require.Equal(t, []string{"init.mp4", "seg2.mp4"}, col.downloadedSegments())
	})

	t.Run("waiting for next segment", func(t *testing.T) {
		server := clientLiveServer(t, []clientLiveSnapshot{
			{mediaSequence: 0, segmentIDs: []int{0, 1, 2}},
			// the playlist neither publishes new segments nor announces
			// the end of the stream.
			{mediaSequence: 0, segmentIDs: []int{0, 1, 2}},
		}, 3)

		col, done := clientLiveStart(t, server, 3)

		require.Equal(t, byte(2), clientLifecycleReceive(t, col.data, "segment 2"))
		require.EqualError(t, clientLifecycleReceive(t, done, "Wait2"),
			"next segment not found or not ready yet")

		require.Equal(t, []string{"init.mp4", "seg2.mp4"}, col.downloadedSegments())
	})
}
