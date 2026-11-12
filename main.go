// screen-share: stream your screen to anyone with a browser.
//
// You open http://localhost:<port>/broadcast and pick a screen. The browser
// sends it to this server over WebRTC, and the server relays the same packets
// to every viewer at http://<your-address>:<port>/.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

//go:embed web
var webFiles embed.FS

type hub struct {
	api        *webrtc.API
	maxViewers int
	links      []viewerLink

	mu        sync.Mutex
	session   uint64 // bumped every time a broadcast starts or ends
	pub       *webrtc.PeerConnection
	video     *webrtc.TrackLocalStaticRTP
	audio     *webrtc.TrackLocalStaticRTP
	videoSSRC uint32
	lastPLI   time.Time
	viewers   map[*webrtc.PeerConnection]string
}

func main() {
	port := flag.Int("port", 8080, "TCP port for the web page")
	udpPort := flag.Int("udp-port", 0, "UDP port for the video stream (default: same number as -port)")
	maxViewers := flag.Int("max-viewers", 0, "maximum simultaneous viewers (0 = unlimited)")
	noBrowser := flag.Bool("no-browser", false, "don't open the broadcast page automatically")
	flag.Parse()
	if *udpPort == 0 {
		*udpPort = *port
	}

	api, err := newAPI(*udpPort)
	if err != nil {
		log.Fatalf("could not open UDP port %d: %v", *udpPort, err)
	}
	h := &hub{
		api:        api,
		maxViewers: *maxViewers,
		links:      findViewerLinks(*port),
		viewers:    map[*webrtc.PeerConnection]string{},
	}

	static, _ := fs.Sub(webFiles, "web")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "index.html")
	})
	mux.HandleFunc("GET /broadcast", localOnly(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "broadcast.html")
	}))
	mux.HandleFunc("POST /api/publish", localOnly(h.handlePublish))
	mux.HandleFunc("POST /api/unpublish", localOnly(h.handleUnpublish))
	mux.HandleFunc("GET /api/info", localOnly(h.handleInfo))
	mux.HandleFunc("POST /api/watch", h.handleWatch)
	mux.HandleFunc("GET /api/status", h.handleStatus)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("could not open TCP port %d: %v", *port, err)
	}

	broadcastURL := fmt.Sprintf("http://localhost:%d/broadcast", *port)
	fmt.Println()
	fmt.Println("  Screen share server is running.")
	fmt.Println()
	fmt.Printf("  Start broadcasting (this PC only):  %s\n", broadcastURL)
	fmt.Println()
	fmt.Println("  Links for viewers:")
	if len(h.links) == 0 {
		fmt.Println("    (no network addresses found)")
	}
	for _, l := range h.links {
		fmt.Printf("    %-48s %s\n", l.URL, l.Note)
	}
	fmt.Println()
	fmt.Printf("  Ports to allow through firewalls: TCP %d and UDP %d\n", *port, *udpPort)
	fmt.Println("  Press Ctrl+C to stop.")
	fmt.Println()

	if !*noBrowser {
		openBrowser(broadcastURL)
	}
	log.Fatal(http.Serve(ln, mux))
}

func newAPI(udpPort int) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	videoFB := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: videoFB},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}

	// All media for every viewer goes through one UDP port, so only that
	// port has to be opened in firewalls.
	udpMux, err := ice.NewMultiUDPMuxFromPort(udpPort,
		ice.UDPMuxFromPortWithInterfaceFilter(keepInterface),
		ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4, ice.NetworkTypeUDP6),
	)
	if err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(udpMux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})

	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

// keepInterface skips virtual adapters (WSL, Hyper-V, VPNs, VMs), whose
// addresses are useless to remote viewers.
func keepInterface(name string) bool {
	n := strings.ToLower(name)
	for _, skip := range []string{"vethernet", "virtualbox", "vmware", "loopback", "wsl", "hyper-v", "bluetooth", "docker"} {
		if strings.Contains(n, skip) {
			return false
		}
	}
	return true
}

// ---- broadcaster ----

func (h *hub) handlePublish(w http.ResponseWriter, r *http.Request) {
	offer, ok := readOffer(w, r)
	if !ok {
		return
	}
	pc, err := h.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var video, audio *webrtc.TrackLocalStaticRTP
	for _, t := range pc.GetTransceivers() {
		switch {
		case t.Kind() == webrtc.RTPCodecTypeVideo && video == nil:
			video, _ = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "screen")
		case t.Kind() == webrtc.RTPCodecTypeAudio && audio == nil:
			audio, _ = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "screen")
		}
	}
	if video == nil {
		pc.Close()
		http.Error(w, "the offer has no video track", http.StatusBadRequest)
		return
	}

	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		local := audio
		if remote.Kind() == webrtc.RTPCodecTypeVideo {
			local = video
			h.mu.Lock()
			if h.pub == pc {
				h.videoSSRC = uint32(remote.SSRC())
			}
			h.mu.Unlock()
			h.requestKeyframe()
		}
		if local == nil {
			return
		}
		// Relay every packet unchanged to all viewers. Header extensions are
		// dropped because their IDs were negotiated with the broadcaster,
		// not with each viewer.
		for {
			pkt, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			pkt.Extension = false
			pkt.Extensions = nil
			_ = local.WriteRTP(pkt)
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("broadcaster: %s", s)
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			h.endSession(pc)
		}
	})

	answer, ok := answerOffer(w, pc)
	if !ok {
		return
	}

	h.mu.Lock()
	oldPub, oldViewers := h.pub, h.viewers
	h.pub, h.video, h.audio, h.videoSSRC = pc, video, audio, 0
	h.viewers = map[*webrtc.PeerConnection]string{}
	h.session++
	sess := h.session
	h.mu.Unlock()
	go closeAll(oldPub, oldViewers)

	log.Printf("broadcast started (audio: %v)", audio != nil)
	writeJSON(w, sdpReply{Type: answer.Type.String(), SDP: answer.SDP, Session: sess})
}

func (h *hub) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	pub := h.pub
	h.mu.Unlock()
	if pub != nil {
		h.endSession(pub)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *hub) endSession(pc *webrtc.PeerConnection) {
	h.mu.Lock()
	if h.pub != pc {
		h.mu.Unlock()
		go pc.Close()
		return
	}
	viewers := h.viewers
	h.pub, h.video, h.audio, h.videoSSRC = nil, nil, nil, 0
	h.viewers = map[*webrtc.PeerConnection]string{}
	h.session++
	h.mu.Unlock()
	log.Printf("broadcast ended")
	go closeAll(pc, viewers)
}

// requestKeyframe asks the broadcaster's encoder for a full frame, which a
// newly joined viewer needs before it can show anything.
func (h *hub) requestKeyframe() {
	h.mu.Lock()
	pub, ssrc := h.pub, h.videoSSRC
	if pub == nil || ssrc == 0 || time.Since(h.lastPLI) < 300*time.Millisecond {
		h.mu.Unlock()
		return
	}
	h.lastPLI = time.Now()
	h.mu.Unlock()
	_ = pub.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
}

// ---- viewers ----

func (h *hub) handleWatch(w http.ResponseWriter, r *http.Request) {
	offer, ok := readOffer(w, r)
	if !ok {
		return
	}
	h.mu.Lock()
	live, sess, video, audio, n := h.pub != nil, h.session, h.video, h.audio, len(h.viewers)
	h.mu.Unlock()
	if !live {
		http.Error(w, "The broadcast hasn't started yet.", http.StatusServiceUnavailable)
		return
	}
	if h.maxViewers > 0 && n >= h.maxViewers {
		http.Error(w, "The stream is full. Try again later.", http.StatusServiceUnavailable)
		return
	}

	pc, err := h.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, t := range []*webrtc.TrackLocalStaticRTP{video, audio} {
		if t == nil {
			continue
		}
		sender, err := pc.AddTrack(t)
		if err != nil {
			pc.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go func() {
			for {
				pkts, _, err := sender.ReadRTCP()
				if err != nil {
					return
				}
				for _, p := range pkts {
					switch p.(type) {
					case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
						h.requestKeyframe()
					}
				}
			}
		}()
	}
	who := r.RemoteAddr
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			log.Printf("viewer %s connected (%d watching)", who, h.viewerCount())
			h.requestKeyframe()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			h.removeViewer(pc)
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	answer, ok := answerOffer(w, pc)
	if !ok {
		return
	}

	h.mu.Lock()
	if h.session != sess {
		h.mu.Unlock()
		pc.Close()
		http.Error(w, "The broadcast restarted, reconnecting.", http.StatusServiceUnavailable)
		return
	}
	h.viewers[pc] = who
	h.mu.Unlock()
	writeJSON(w, sdpReply{Type: answer.Type.String(), SDP: answer.SDP, Session: sess})
}

func (h *hub) removeViewer(pc *webrtc.PeerConnection) {
	h.mu.Lock()
	who, ok := h.viewers[pc]
	delete(h.viewers, pc)
	n := len(h.viewers)
	h.mu.Unlock()
	if ok {
		log.Printf("viewer %s left (%d watching)", who, n)
	}
	go pc.Close()
}

func (h *hub) viewerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.viewers)
}

func (h *hub) handleStatus(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	writeJSON(w, map[string]any{"live": h.pub != nil, "session": h.session, "viewers": len(h.viewers)})
}

func (h *hub) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"links": h.links})
}

// ---- helpers ----

type sdpReply struct {
	Type    string `json:"type"`
	SDP     string `json:"sdp"`
	Session uint64 `json:"session"`
}

func readOffer(w http.ResponseWriter, r *http.Request) (webrtc.SessionDescription, bool) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&offer); err != nil || offer.Type != webrtc.SDPTypeOffer {
		http.Error(w, "expected an SDP offer", http.StatusBadRequest)
		return offer, false
	}
	return offer, true
}

// answerOffer creates the answer and waits for all ICE candidates, so a
// single HTTP round trip is the whole handshake.
func answerOffer(w http.ResponseWriter, pc *webrtc.PeerConnection) (*webrtc.SessionDescription, bool) {
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	<-gathered
	return pc.LocalDescription(), true
}

func closeAll(pub *webrtc.PeerConnection, viewers map[*webrtc.PeerConnection]string) {
	for v := range viewers {
		v.Close()
	}
	if pub != nil {
		pub.Close()
	}
}

func localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			http.Error(w, "Broadcasting is only allowed from the computer running the server.", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
