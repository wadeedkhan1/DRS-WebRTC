// Package main implements the admin backend server for the DRS dashboard.
//
// Responsibilities:
//   - Serves the frontend static files (HTML/CSS/JS) at "/"
//   - Provides a WebSocket proxy at /ws/viewer that transparently bridges
//     each browser WebSocket to a fresh DRS viewer connection
//
// The admin backend is intentionally thin — it is a pure relay/proxy.
// All business logic (signaling, routing, audit) lives in the DRS server.
//
// FUTURE HOOKS:
//   - Authentication middleware (login page, session cookies)
//   - RBAC enforcement before proxying control commands
//   - HTTPS / TLS termination
//   - Admin-specific REST APIs (user management, audit viewer, etc.)
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

// =====================================================================
// Configuration
// =====================================================================

var (
	// drsWSURL is the DRS server's WebSocket endpoint for viewers.
	// Override via the DRS_URL environment variable for remote deployment.
	drsWSURL string

	// listenAddr is the address the admin dashboard listens on.
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
	CheckOrigin:     func(r *http.Request) bool { return true }, // FUTURE: restrict in production
}

// =====================================================================
// WebSocket proxy: browser ⟷ DRS
// =====================================================================

// handleViewerWS upgrades the browser connection and opens a parallel
// connection to the DRS server as a viewer. All messages are relayed
// bidirectionally without inspection or modification.
func handleViewerWS(w http.ResponseWriter, r *http.Request) {
	// 1. Upgrade the browser side
	browserConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Browser WebSocket upgrade failed: %v", err)
		return
	}
	defer browserConn.Close()

	// 2. Dial the DRS server as a new viewer
	drsConn, _, err := websocket.DefaultDialer.Dial(drsWSURL, nil)
	if err != nil {
		log.Printf("Cannot connect to DRS at %s: %v", drsWSURL, err)
		return
	}
	defer drsConn.Close()

	log.Printf("Proxy established: browser ↔ DRS (%s)", drsWSURL)

	// Channel to signal when either direction closes
	done := make(chan struct{}, 1)

	// DRS → Browser relay
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

	// Browser → DRS relay
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

	// Wait for either direction to close
	<-done
	log.Println("Proxy session ended")
}

// =====================================================================
// Entry point
// =====================================================================

func main() {
	// Determine the static files directory. Supports running from the
	// project root ("go run admin/main.go") or from within admin/.
	staticDir := "admin/static"
	if _, err := os.Stat(staticDir); os.IsNotExist(err) {
		staticDir = "static"
		if _, err := os.Stat(staticDir); os.IsNotExist(err) {
			log.Fatal("Cannot locate static files directory. " +
				"Run from the project root or from the admin/ directory.")
		}
	}

	// Serve frontend assets
	fs := http.FileServer(http.Dir(staticDir))
	http.Handle("/", fs)

	// WebSocket proxy for browser ↔ DRS communication
	http.HandleFunc("/ws/viewer", handleViewerWS)

	// FUTURE: Add authenticated REST endpoints here.
	// http.HandleFunc("/api/sessions", handleSessions)
	// http.HandleFunc("/api/users",    handleUsers)

	log.Printf("=== Admin Dashboard starting on %s (static: %s) ===", listenAddr, staticDir)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
