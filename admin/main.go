// Package main implements the DRS Admin as a standalone desktop application.
//
// The static dashboard files (HTML/CSS/JS) are embedded directly into the
// binary at compile time using go:embed — no separate folder needed alongside
// the .exe. The admin backend HTTP server starts on localhost:8081, and a
// native webview window opens pointing to it. No browser required.
//
// Build command:
//
//	go build -ldflags="-H windowsgui" -o admin.exe ./admin
//
// The -H windowsgui flag suppresses the black console window on Windows.
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
	webview "github.com/webview/webview_go"
)

// =====================================================================
// Embed static dashboard files into the binary at compile time.
// The "static" folder (admin/static/) is bundled inside the .exe —
// no external files needed alongside it when distributing.
// =====================================================================

//go:embed static
var embeddedStatic embed.FS

// =====================================================================
// Configuration
// =====================================================================

var (
	drsWSURL   string
	listenAddr string
)

func init() {
	drsWSURL = os.Getenv("DRS_URL")
	if drsWSURL == "" {
		drsWSURL = "ws://2.25.187.24:8080/ws?role=viewer"
	}

	listenAddr = os.Getenv("ADMIN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8081"
	}
}

// =====================================================================
// WebSocket upgrader
// =====================================================================

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024 * 1024,
	WriteBufferSize: 1024 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// =====================================================================
// WebSocket proxy: webview ⟷ DRS server
// =====================================================================

func handleViewerWS(w http.ResponseWriter, r *http.Request) {
	browserConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer browserConn.Close()

	drsConn, _, err := websocket.DefaultDialer.Dial(drsWSURL, nil)
	if err != nil {
		log.Printf("Cannot connect to DRS at %s: %v", drsWSURL, err)
		return
	}
	defer drsConn.Close()

	log.Printf("Proxy established: webview ↔ DRS (%s)", drsWSURL)

	done := make(chan struct{}, 1)

	// DRS → webview
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, msg, err := drsConn.ReadMessage()
			if err != nil {
				return
			}
			if err := browserConn.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	// webview → DRS
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, msg, err := browserConn.ReadMessage()
			if err != nil {
				return
			}
			if err := drsConn.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	<-done
	log.Println("Proxy session ended")
}

// =====================================================================
// HTTP server — serves embedded static files + WebSocket proxy
// =====================================================================

func startServer() {
	// Strip the "static" prefix from embedded paths so that
	// http://localhost:8081/index.html works rather than
	// http://localhost:8081/static/index.html
	subFS, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		log.Fatalf("Failed to sub embedded FS: %v", err)
	}

	http.Handle("/", http.FileServer(http.FS(subFS)))
	http.HandleFunc("/ws/viewer", handleViewerWS)

	log.Printf("Admin backend starting on %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}

// =====================================================================
// Entry point — start server then open native desktop window
// =====================================================================

func main() {
	// Start the HTTP server in the background
	go startServer()

	// Brief pause to let the server bind before the webview tries to load it
	time.Sleep(300 * time.Millisecond)

	// Open the native desktop window
	// Set debug=false for production builds to disable right-click devtools
	debug := true
	w := webview.New(debug)
	defer w.Destroy()

	w.SetTitle("DRS Admin Dashboard")
	w.SetSize(1280, 800, webview.HintNone)
	w.Navigate("http://localhost:8081")
	w.Run()
	// When the window is closed, the process exits — HTTP server shuts down too
}
