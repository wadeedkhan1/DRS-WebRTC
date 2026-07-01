package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"math/rand"
	"os"
	"runtime/debug"
	"sync"
	"time"

	x264 "github.com/gen2brain/x264-go"
	"github.com/go-vgo/robotgo"
	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

const perfLogging = true

type Message struct {
	Type      string      `json:"type"`
	Role      string      `json:"role,omitempty"`
	SenderID  string      `json:"sender_id,omitempty"`
	TargetID  string      `json:"target_id,omitempty"`
	ID        string      `json:"id,omitempty"`
	Hostname  string      `json:"hostname,omitempty"`
	SDP       string      `json:"sdp,omitempty"`
	Candidate string      `json:"candidate,omitempty"`
	Control   *ControlCmd `json:"control,omitempty"`
	Hosts     []HostInfo  `json:"hosts,omitempty"`
	Preview   string      `json:"preview,omitempty"`
}

type ControlCmd struct {
	Action  string  `json:"action"`
	X       float64 `json:"x,omitempty"`
	Y       float64 `json:"y,omitempty"`
	Button  string  `json:"button,omitempty"`
	ScrollX float64 `json:"scroll_x,omitempty"`
	ScrollY float64 `json:"scroll_y,omitempty"`
	Key     string  `json:"key,omitempty"`
}

type HostInfo struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Status   string `json:"status"`
	Preview  string `json:"preview,omitempty"`
}

// =====================================================================
// Global state
// =====================================================================

var (
	drsURL string

	clientID string
	hostName string

	wsConn *websocket.Conn
	wsMu   sync.Mutex

	activeSession bool = true

	currentViewerID   string
	peerConnection    *webrtc.PeerConnection
	cancelStreaming   context.CancelFunc
	pendingCandidates []webrtc.ICECandidateInit
	candidateMu       sync.Mutex
	forceKeyframeChan = make(chan struct{}, 1)
)

// =====================================================================
// Entry point
// =====================================================================

func main() {
	// PERF: Raise the GC trigger threshold. Default GOGC=100 means GC
	// runs whenever heap doubles since last collection — with our
	// per-frame churn that was triggering very frequently. 400 means
	// the heap can grow 4x before a collection, trading some extra
	// memory (fine on a desktop) for far fewer, less disruptive pauses.
	debug.SetGCPercent(400)

	drsURL = os.Getenv("DRS_URL")
	if drsURL == "" {
		drsURL = "ws://2.25.187.24:8080/ws?role=host"
	}

	clientID = loadOrGenerateID()
	hostName = getHostname()

	log.Printf("Client agent starting — ID=%s  hostname=%s", clientID, hostName)

	backoff := 2 * time.Second
	for {
		err := run()
		log.Printf("Connection lost: %v — reconnecting in %v …", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// =====================================================================
// Main session lifecycle
// =====================================================================

func run() error {
	var err error
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	wsConn, _, err = dialer.Dial(drsURL, nil)
	if err != nil {
		return fmt.Errorf("dial DRS: %w", err)
	}
	defer wsConn.Close()

	regMsg := Message{Type: "register", ID: clientID, Hostname: hostName}
	data, _ := json.Marshal(regMsg)
	if err := wsConn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	_, rawMsg, err := wsConn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read register ack: %w", err)
	}
	var ack Message
	_ = json.Unmarshal(rawMsg, &ack)
	if ack.Type != "registered" {
		return fmt.Errorf("unexpected ack type: %s", ack.Type)
	}
	log.Printf("Registered with DRS server")

	previewCtx, previewCancel := context.WithCancel(context.Background())
	defer previewCancel()
	go previewLoop(previewCtx)

	for {
		_, rawMsg, err := wsConn.ReadMessage()
		if err != nil {
			stopWebRTCSession()
			return fmt.Errorf("read: %w", err)
		}

		var msg Message
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			log.Printf("Invalid JSON received: %v", err)
			continue
		}
		handleMessage(msg)
	}
}

// =====================================================================
// Message dispatcher
// =====================================================================

func handleMessage(msg Message) {
	switch msg.Type {
	case "connect_host":
		log.Printf("Viewer %s requests connection", msg.SenderID)
		go startWebRTCSession(msg.SenderID)

	case "disconnect_host":
		log.Printf("Viewer %s disconnecting", msg.SenderID)
		stopWebRTCSession()

	case "answer":
		handleAnswer(msg)

	case "candidate":
		handleCandidate(msg)

	case "control":
		handleControl(msg.Control)

	default:
		log.Printf("Unhandled message type: %s", msg.Type)
	}
}

// =====================================================================
// WebRTC session management
// =====================================================================

func startWebRTCSession(viewerID string) {
	stopWebRTCSession()
	currentViewerID = viewerID

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	var err error
	peerConnection, err = webrtc.NewPeerConnection(config)
	if err != nil {
		log.Printf("PeerConnection creation failed: %v", err)
		return
	}

	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "drs-screen",
	)
	if err != nil {
		log.Printf("Video track creation failed: %v", err)
		return
	}

	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		log.Printf("AddTrack failed: %v", err)
		return
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if peerConnection == nil {
				return
			}
			_, _, rtcpErr := rtpSender.Read(rtcpBuf)
			if rtcpErr != nil {
				return
			}
		}
	}()

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidateJSON, _ := json.Marshal(c.ToJSON())
		msg := Message{
			Type:      "candidate",
			TargetID:  viewerID,
			Candidate: string(candidateJSON),
		}
		data, _ := json.Marshal(msg)
		wsMu.Lock()
		_ = wsConn.WriteMessage(websocket.TextMessage, data)
		wsMu.Unlock()
	})

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("WebRTC state: %s", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			select {
			case forceKeyframeChan <- struct{}{}:
			default:
			}
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateClosed:
			stopWebRTCSession()
		}
	})

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		log.Printf("CreateOffer failed: %v", err)
		return
	}
	if err = peerConnection.SetLocalDescription(offer); err != nil {
		log.Printf("SetLocalDescription failed: %v", err)
		return
	}

	offerMsg := Message{
		Type:     "offer",
		TargetID: viewerID,
		SDP:      offer.SDP,
	}
	offerData, _ := json.Marshal(offerMsg)
	wsMu.Lock()
	_ = wsConn.WriteMessage(websocket.TextMessage, offerData)
	wsMu.Unlock()

	log.Printf("SDP offer sent to viewer %s", viewerID)

	ctx, cancel := context.WithCancel(context.Background())
	cancelStreaming = cancel
	go streamScreen(ctx, videoTrack)
}

func stopWebRTCSession() {
	if cancelStreaming != nil {
		cancelStreaming()
		cancelStreaming = nil
	}
	if peerConnection != nil {
		_ = peerConnection.Close()
		peerConnection = nil
	}
	candidateMu.Lock()
	pendingCandidates = nil
	candidateMu.Unlock()
	currentViewerID = ""
}

// =====================================================================
// WebRTC signaling handlers
// =====================================================================

func handleAnswer(msg Message) {
	if peerConnection == nil {
		log.Println("Received answer but no active PeerConnection")
		return
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  msg.SDP,
	}
	if err := peerConnection.SetRemoteDescription(answer); err != nil {
		log.Printf("SetRemoteDescription (answer) failed: %v", err)
		return
	}
	log.Println("Remote description (answer) set")

	candidateMu.Lock()
	for _, c := range pendingCandidates {
		if err := peerConnection.AddICECandidate(c); err != nil {
			log.Printf("AddICECandidate (buffered) failed: %v", err)
		}
	}
	pendingCandidates = nil
	candidateMu.Unlock()
}

func handleCandidate(msg Message) {
	if peerConnection == nil {
		return
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(msg.Candidate), &candidate); err != nil {
		log.Printf("ICE candidate parse error: %v", err)
		return
	}

	candidateMu.Lock()
	defer candidateMu.Unlock()

	if peerConnection.RemoteDescription() == nil {
		pendingCandidates = append(pendingCandidates, candidate)
		return
	}

	if err := peerConnection.AddICECandidate(candidate); err != nil {
		log.Printf("AddICECandidate failed: %v", err)
	}
}

// =====================================================================
// Screen capture → H.264 encoding → WebRTC track
//
// PERF: every buffer below is allocated ONCE before the loop starts and
// reused on every single frame. Nothing inside the for{} loop calls
// make()/image.NewRGBA() in the steady-state path except the unavoidable
// screenshot.CaptureRect() call (the kbinani library has no buffer-reuse
// API — it always returns a fresh *image.RGBA backed by a new GDI read).
// =====================================================================

func streamScreen(ctx context.Context, track *webrtc.TrackLocalStaticSample) {
	bounds := screenshot.GetDisplayBounds(0)
	origW := bounds.Dx()
	origH := bounds.Dy()

	width := origW
	height := origH
	if width > 1280 {
		width = 1280
		height = 1280 * origH / origW
	}
	width = width &^ 1
	height = height &^ 1

	needsScale := origW > 1280
	log.Printf("Streaming screen: original %dx%d downscaled to %dx%d for streaming", origW, origH, width, height)

	buf := &bytes.Buffer{}
	opts := &x264.Options{
		Width:     width,
		Height:    height,
		FrameRate: 15,
		Tune:      "zerolatency",
		Preset:    "veryfast",
		Profile:   "baseline",
		LogLevel:  x264.LogWarning,
	}

	enc, err := x264.NewEncoder(buf, opts)
	if err != nil {
		log.Printf("x264 encoder init failed: %v", err)
		return
	}
	defer func() {
		_ = enc.Flush()
		_ = enc.Close()
	}()

	// header is reused via append(header[:0], ...) on recreate — no realloc
	header := make([]byte, 0, 256)
	header = append(header, buf.Bytes()...)
	buf.Reset()

	// --- PERF: pre-allocated reusable buffers ------------------------
	var scaledImg *image.RGBA
	if needsScale {
		scaledImg = image.NewRGBA(image.Rect(0, 0, width, height))
	}
	ycbcrImg := x264.NewYCbCr(image.Rect(0, 0, width, height))
	// Output buffer for WriteSample — grown lazily, reused after that
	sampleBuf := make([]byte, 0, 512*1024)

	firstFrame := true
	frameCount := 0
	const fps = 15

	recreateEncoder := func() bool {
		enc.Close()
		enc, err = x264.NewEncoder(buf, opts)
		if err != nil {
			log.Printf("Encoder recreate failed: %v", err)
			return false
		}
		header = append(header[:0], buf.Bytes()...)
		buf.Reset()
		return true
	}

	ticker := time.NewTicker(time.Second / fps)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Screen streaming stopped")
			return

		case <-forceKeyframeChan:
			log.Println("Forcing keyframe on connection request...")
			if !recreateEncoder() {
				return
			}
			firstFrame = true

		case <-ticker.C:
			frameStart := time.Now()
			frameCount++

			if frameCount%60 == 0 {
				if !recreateEncoder() {
					return
				}
				firstFrame = true
			}

			t0 := time.Now()
			img, err := screenshot.CaptureRect(bounds)
			captureDur := time.Since(t0)
			if err != nil {
				log.Printf("Screen capture error: %v", err)
				continue
			}

			t1 := time.Now()
			finalImg := img
			if needsScale {
				resizeImageInto(img, scaledImg)
				finalImg = scaledImg
			}
			scaleDur := time.Since(t1)

			t2 := time.Now()
			convertRGBAtoYCbCr(finalImg, ycbcrImg)
			convertDur := time.Since(t2)

			t3 := time.Now()
			buf.Reset()
			if err := enc.Encode(ycbcrImg); err != nil {
				log.Printf("Encode error: %v", err)
				continue
			}
			encodeDur := time.Since(t3)

			if buf.Len() == 0 {
				continue
			}

			needed := buf.Len()
			if firstFrame && len(header) > 0 {
				needed += len(header)
			}
			if cap(sampleBuf) < needed {
				sampleBuf = make([]byte, 0, needed+needed/2)
			}
			sampleBuf = sampleBuf[:0]
			if firstFrame && len(header) > 0 {
				sampleBuf = append(sampleBuf, header...)
				firstFrame = false
			}
			sampleBuf = append(sampleBuf, buf.Bytes()...)

			if err := track.WriteSample(media.Sample{
				Data:     sampleBuf,
				Duration: time.Second / fps,
			}); err != nil {
				log.Printf("WriteSample error: %v", err)
				return
			}

			if perfLogging {
				log.Printf("[PERF] capture: %v | scale: %v | convert: %v | encode: %v | total: %v",
					captureDur, scaleDur, convertDur, encodeDur, time.Since(frameStart))
			}
		}
	}
}

func copyBGRAtoRGBA(src []byte, dst *image.RGBA) {
	for i := 0; i+3 < len(src); i += 4 {
		dst.Pix[i] = src[i+2]
		dst.Pix[i+1] = src[i+1]
		dst.Pix[i+2] = src[i]
		dst.Pix[i+3] = src[i+3]
	}
}

// resizeImageInto performs a fast nearest-neighbour down-scale, writing
// into a pre-allocated destination buffer instead of allocating a new
// one every call. dst must already be sized to the target dimensions.
func resizeImageInto(src *image.RGBA, dst *image.RGBA) {
	srcBounds := src.Bounds()
	srcW := srcBounds.Dx()
	srcH := srcBounds.Dy()
	dstBounds := dst.Bounds()
	newWidth := dstBounds.Dx()
	newHeight := dstBounds.Dy()

	for y := 0; y < newHeight; y++ {
		srcY := y * srcH / newHeight
		for x := 0; x < newWidth; x++ {
			srcX := x * srcW / newWidth
			srcOff := srcY*src.Stride + srcX*4
			dstOff := y*dst.Stride + x*4
			copy(dst.Pix[dstOff:dstOff+4], src.Pix[srcOff:srcOff+4])
		}
	}
}

// convertRGBAtoYCbCr performs a manual RGBA→I420 conversion using 2x2 blocks.
func convertRGBAtoYCbCr(src *image.RGBA, dst *x264.YCbCr) {
	w := src.Bounds().Dx()
	h := src.Bounds().Dy()
	srcStride := src.Stride
	yStride := dst.YStride
	cStride := dst.CStride

	for y := 0; y < h; y += 2 {
		for x := 0; x < w; x += 2 {
			cy := y / 2
			cx := x / 2

			off00 := y*srcStride + x*4
			r00 := int32(src.Pix[off00])
			g00 := int32(src.Pix[off00+1])
			b00 := int32(src.Pix[off00+2])
			dst.Y[y*yStride+x] = uint8((19595*r00 + 38470*g00 + 7471*b00) >> 16)

			dst.Cb[cy*cStride+cx] = uint8((-11059*r00 - 21709*g00 + 32768*b00 + 8388608) >> 16)
			dst.Cr[cy*cStride+cx] = uint8((32768*r00 - 27439*g00 - 5329*b00 + 8388608) >> 16)

			if x+1 < w {
				off01 := y*srcStride + (x+1)*4
				r01 := int32(src.Pix[off01])
				g01 := int32(src.Pix[off01+1])
				b01 := int32(src.Pix[off01+2])
				dst.Y[y*yStride+x+1] = uint8((19595*r01 + 38470*g01 + 7471*b01) >> 16)
			}

			if y+1 < h {
				off10 := (y+1)*srcStride + x*4
				r10 := int32(src.Pix[off10])
				g10 := int32(src.Pix[off10+1])
				b10 := int32(src.Pix[off10+2])
				dst.Y[(y+1)*yStride+x] = uint8((19595*r10 + 38470*g10 + 7471*b10) >> 16)
			}

			if x+1 < w && y+1 < h {
				off11 := (y+1)*srcStride + (x+1)*4
				r11 := int32(src.Pix[off11])
				g11 := int32(src.Pix[off11+1])
				b11 := int32(src.Pix[off11+2])
				dst.Y[(y+1)*yStride+x+1] = uint8((19595*r11 + 38470*g11 + 7471*b11) >> 16)
			}
		}
	}
}

// =====================================================================
// Remote-control command execution
// =====================================================================

func handleControl(cmd *ControlCmd) {
	if cmd == nil {
		return
	}

	if !activeSession {
		log.Println("Control command rejected — session not active (consent required)")
		return
	}

	bounds := screenshot.GetDisplayBounds(0)
	screenW := float64(bounds.Dx())
	screenH := float64(bounds.Dy())

	switch cmd.Action {
	case "mouse_move":
		absX := int(cmd.X * screenW)
		absY := int(cmd.Y * screenH)
		robotgo.Move(absX, absY)

	case "mouse_click":
		absX := int(cmd.X * screenW)
		absY := int(cmd.Y * screenH)
		robotgo.Move(absX, absY)
		btn := cmd.Button
		if btn == "" {
			btn = "left"
		}
		robotgo.Click(btn, false)

	case "mouse_scroll":
		robotgo.Scroll(int(cmd.ScrollX), int(cmd.ScrollY))

	case "key_press":
		if cmd.Key != "" {
			robotgo.KeyTap(cmd.Key)
		}

	default:
		log.Printf("Unknown control action: %s", cmd.Action)
	}
}

// =====================================================================
// Preview thumbnail loop
// =====================================================================

func previewLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// PERF: reuse the resize+encode buffers here too
	var previewBuf *image.RGBA
	jpegBuf := &bytes.Buffer{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			preview, err := capturePreview(&previewBuf, jpegBuf)
			if err != nil {
				log.Printf("Preview capture error: %v", err)
				continue
			}

			msg := Message{Type: "preview", Preview: preview}
			data, _ := json.Marshal(msg)

			wsMu.Lock()
			if wsConn != nil {
				_ = wsConn.WriteMessage(websocket.TextMessage, data)
			}
			wsMu.Unlock()
		}
	}
}

// capturePreview grabs the screen, down-samples to 320px wide, and
// returns a base64-encoded JPEG data URL. previewBuf is reused across
// calls (allocated once on first use; since this only runs every 3s the
// impact of NOT reusing it would be small, but no reason to leave it
// wasteful since the pattern is established).
func capturePreview(previewBuf **image.RGBA, jpegBuf *bytes.Buffer) (string, error) {
	bounds := screenshot.GetDisplayBounds(0)
	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return "", err
	}

	srcW := img.Bounds().Dx()
	srcH := img.Bounds().Dy()
	newHeight := 320 * srcH / srcW

	if *previewBuf == nil || (*previewBuf).Bounds().Dx() != 320 || (*previewBuf).Bounds().Dy() != newHeight {
		*previewBuf = image.NewRGBA(image.Rect(0, 0, 320, newHeight))
	}
	resizeImageInto(img, *previewBuf)

	jpegBuf.Reset()
	if err := jpeg.Encode(jpegBuf, *previewBuf, &jpeg.Options{Quality: 40}); err != nil {
		return "", err
	}

	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBuf.Bytes()), nil
}

// =====================================================================
// Identity helpers
// =====================================================================

func loadOrGenerateID() string {
	data, err := os.ReadFile(".client_id")
	if err == nil && len(data) > 0 {
		return string(data)
	}

	id := fmt.Sprintf("client_%d_%d", time.Now().UnixNano(), rand.Intn(10000))
	_ = os.WriteFile(".client_id", []byte(id), 0644)
	return id
}

func getHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "Unknown-Host"
	}
	return h
}
