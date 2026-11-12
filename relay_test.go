package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// TestRelay checks the whole path: a broadcaster publishes a VP8 track,
// a viewer connects, and the viewer receives the relayed packets.
func TestRelay(t *testing.T) {
	api, err := newAPI(38123)
	if err != nil {
		t.Fatal(err)
	}
	h := &hub{api: api, viewers: map[*webrtc.PeerConnection]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/publish", localOnly(h.handlePublish))
	mux.HandleFunc("POST /api/watch", h.handleWatch)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	negotiate := func(pc *webrtc.PeerConnection, path string) {
		offer, _ := pc.CreateOffer(nil)
		g := webrtc.GatheringCompletePromise(pc)
		pc.SetLocalDescription(offer)
		<-g
		body, _ := json.Marshal(pc.LocalDescription())
		res, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(body))
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, err, res.Status)
		}
		var ans webrtc.SessionDescription
		json.NewDecoder(res.Body).Decode(&ans)
		if err := pc.SetRemoteDescription(ans); err != nil {
			t.Fatal(err)
		}
	}

	pub, _ := webrtc.NewPeerConnection(webrtc.Configuration{})
	defer pub.Close()
	track, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "v", "s")
	pub.AddTrack(track)
	negotiate(pub, "/api/publish")
	go func() {
		for i := 0; i < 400; i++ {
			track.WriteSample(media.Sample{Data: []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a}, Duration: 33 * time.Millisecond})
			time.Sleep(33 * time.Millisecond)
		}
	}()

	viewer, _ := webrtc.NewPeerConnection(webrtc.Configuration{})
	defer viewer.Close()
	viewer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	got := make(chan struct{})
	viewer.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for i := 0; i < 10; i++ {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
		}
		close(got)
	})
	negotiate(viewer, "/api/watch")

	select {
	case <-got:
		if n := h.viewerCount(); n != 1 {
			t.Fatalf("viewer count = %d, want 1", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("viewer received no packets")
	}
}
