// Package main implements the DRS (Device Remote Session) signaling server.
//
// This server acts as the central hub for:
//   - WebSocket relay between host agents and admin viewers
//   - Host registration and discovery (with live preview thumbnails)
//   - WebRTC SDP/ICE signaling relay
//   - Remote-control command forwarding
//   - Audit-trail logging (console for now; structured for future DB migration)
//
// FUTURE HOOKS:
//   - Authentication middleware (JWT/API-key validation before upgrade)
//   - Role-based permissions (read-only viewer, full-control admin, etc.)
//   - Persistent audit log storage (PostgreSQL, MongoDB, etc.)
//   - Rate limiting on control commands
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// =====================================================================
// Data Structures — shared message envelope used across all components
// =====================================================================

// Message is the universal JSON envelope for every WebSocket frame.
type Message struct {
	Type      string      `json:"type"`                // register, registered, connect_host, disconnect_host, offer, answer, candidate, control, hosts_update, preview
	Role      string      `json:"role,omitempty"`       // "host" | "viewer"
	SenderID  string      `json:"sender_id,omitempty"`  // Populated by server before forwarding
	TargetID  string      `json:"target_id,omitempty"`  // Destination client/viewer ID
	ID        string      `json:"id,omitempty"`         // Client/viewer ID
	Hostname  string      `json:"hostname,omitempty"`   // Display name of the host machine
	SDP       string      `json:"sdp,omitempty"`        // WebRTC Session Description
	Candidate string      `json:"candidate,omitempty"`  // ICE Candidate (JSON-stringified)
	Control   *ControlCmd `json:"control,omitempty"`    // Remote input event payload
	Hosts     []HostInfo  `json:"hosts,omitempty"`      // List of hosts (for hosts_update)
	Preview   string      `json:"preview,omitempty"`    // Base64-encoded JPEG thumbnail
	// FUTURE: Auth    string `json:"auth,omitempty"`    // JWT or API key for authentication
	// FUTURE: Session string `json:"session,omitempty"` // Unique session tracking ID
}

// ControlCmd represents a single remote-input event (mouse or keyboard).
type ControlCmd struct {
	Action  string  `json:"action"`             // mouse_move, mouse_click, mouse_scroll, key_press
	X       float64 `json:"x,omitempty"`        // Normalised X (0.0–1.0)
	Y       float64 `json:"y,omitempty"`        // Normalised Y (0.0–1.0)
	Button  string  `json:"button,omitempty"`   // left | right | middle
	ScrollX float64 `json:"scroll_x,omitempty"` // Horizontal scroll delta
	ScrollY float64 `json:"scroll_y,omitempty"` // Vertical scroll delta
	Key     string  `json:"key,omitempty"`      // Key name (e.g. "enter", "a")
}

// HostInfo is the per-host metadata exposed to viewers.
type HostInfo struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Status   string `json:"status"`            // "online" | "offline"
	Preview  string `json:"preview,omitempty"` // Base64 JPEG data URL
}

// =====================================================================
// Server internals
// =====================================================================

// ConnectedClient represents one active WebSocket connection (host or viewer).
type ConnectedClient struct {
	ID       string
	Hostname string
	Role     string // "host" or "viewer"
	Conn     *websocket.Conn
	Preview  string     // Latest preview image (hosts only)
	mu       sync.Mutex // Guards writes to Conn
}

// Server holds the connection registry and provides thread-safe access.
type Server struct {
	clients map[string]*ConnectedClient
	mu      sync.RWMutex
	counter int // Monotonic counter for generating unique viewer IDs
}

// WebSocket upgrader — permissive CORS for development.
// FUTURE: Lock down CheckOrigin in production.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024 * 1024, // 1 MB — large enough for base64 preview images
	WriteBufferSize: 1024 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// =====================================================================
// Audit logging
// =====================================================================

// auditLog emits a structured log line. The format is intentionally
// machine-parseable so it can later be redirected to a database writer
// without changing call sites.
//
// FUTURE: Replace with a database INSERT (or message-queue publish).
func auditLog(clientID, hostname, eventType string) {
	log.Printf("[AUDIT] %s | client_id=%s | hostname=%s | event=%s",
		time.Now().Format(time.RFC3339), clientID, hostname, eventType)
}

// =====================================================================
// Server methods
// =====================================================================

func NewServer() *Server {
	return &Server{clients: make(map[string]*ConnectedClient)}
}

// sendTo forwards a raw JSON payload to a specific connection by ID.
func (s *Server) sendTo(targetID string, data []byte) {
	s.mu.RLock()
	client, exists := s.clients[targetID]
	s.mu.RUnlock()

	if !exists {
		log.Printf("Target %s not found — message dropped", targetID)
		return
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if err := client.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Write to %s failed: %v", targetID, err)
	}
}

// broadcastHostsUpdate sends the current list of online hosts (with preview
// thumbnails) to every connected viewer.
func (s *Server) broadcastHostsUpdate() {
	s.mu.RLock()
	hosts := make([]HostInfo, 0)
	viewers := make([]*ConnectedClient, 0)
	for _, c := range s.clients {
		if c.Role == "host" {
			hosts = append(hosts, HostInfo{
				ID:       c.ID,
				Hostname: c.Hostname,
				Status:   "online",
				Preview:  c.Preview,
			})
		} else if c.Role == "viewer" {
			viewers = append(viewers, c)
		}
	}
	s.mu.RUnlock()

	msg := Message{Type: "hosts_update", Hosts: hosts}
	data, _ := json.Marshal(msg)

	for _, v := range viewers {
		v.mu.Lock()
		_ = v.Conn.WriteMessage(websocket.TextMessage, data)
		v.mu.Unlock()
	}
}

// =====================================================================
// Connection handler
// =====================================================================

func (s *Server) handleConnection(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	if role != "host" && role != "viewer" {
		http.Error(w, "Missing or invalid 'role' query parameter (host|viewer)", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	var client *ConnectedClient

	// -----------------------------------------------------------------
	// Viewer registration — server assigns a unique ID automatically.
	// -----------------------------------------------------------------
	if role == "viewer" {
		s.mu.Lock()
		s.counter++
		viewerID := fmt.Sprintf("viewer_%d", s.counter)
		client = &ConnectedClient{ID: viewerID, Role: "viewer", Conn: conn}
		s.clients[viewerID] = client
		s.mu.Unlock()

		auditLog(viewerID, "", "VIEWER_CONNECT")

		// Tell the viewer its assigned ID
		regMsg := Message{Type: "registered", ID: viewerID}
		regData, _ := json.Marshal(regMsg)
		_ = conn.WriteMessage(websocket.TextMessage, regData)

		// Immediately send the current host list
		s.broadcastHostsUpdate()
	}

	// -----------------------------------------------------------------
	// Host registration — first message must be a "register" payload.
	// -----------------------------------------------------------------
	if role == "host" {
		_, rawMsg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Host failed to send registration: %v", err)
			return
		}

		var msg Message
		if err := json.Unmarshal(rawMsg, &msg); err != nil || msg.Type != "register" {
			log.Printf("Expected 'register' from host, got: %s", string(rawMsg))
			return
		}

		s.mu.Lock()
		s.counter++
		assignedID := fmt.Sprintf("host_%d", s.counter)
		client = &ConnectedClient{
			ID: assignedID, Hostname: msg.Hostname,
			Role: "host", Conn: conn,
		}
		s.clients[assignedID] = client
		s.mu.Unlock()

		auditLog(assignedID, msg.Hostname, "HOST_CONNECT")

		// Confirm registration to the host with the assigned ID
		ack := Message{Type: "registered", ID: assignedID}
		ackData, _ := json.Marshal(ack)
		_ = conn.WriteMessage(websocket.TextMessage, ackData)

		// Notify all viewers about the new host
		s.broadcastHostsUpdate()
	}

	// -----------------------------------------------------------------
	// Cleanup on disconnect
	// -----------------------------------------------------------------
	defer func() {
		s.mu.Lock()
		if s.clients[client.ID] == client {
			delete(s.clients, client.ID)
		}
		s.mu.Unlock()

		auditLog(client.ID, client.Hostname,
			fmt.Sprintf("%s_DISCONNECT", strings.ToUpper(client.Role)))
		s.broadcastHostsUpdate()
	}()

	// -----------------------------------------------------------------
	// Main message loop — route each incoming message by type
	// -----------------------------------------------------------------
	for {
		_, rawMsg, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("Connection error (%s): %v", client.ID, err)
			}
			return
		}

		var msg Message
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			log.Printf("Invalid JSON from %s: %v", client.ID, err)
			continue
		}

		switch msg.Type {
		// ---- Host sends a screen preview thumbnail -----------------
		case "preview":
			s.mu.Lock()
			if c, ok := s.clients[client.ID]; ok {
				c.Preview = msg.Preview
			}
			s.mu.Unlock()
			s.broadcastHostsUpdate()

		// ---- Viewer requests a WebRTC session with a host ----------
		case "connect_host":
			msg.SenderID = client.ID
			auditLog(client.ID, "",
				fmt.Sprintf("CONTROL_SESSION_START target=%s", msg.TargetID))
			data, _ := json.Marshal(msg)
			s.sendTo(msg.TargetID, data)

		// ---- Viewer disconnects from a host ------------------------
		case "disconnect_host":
			msg.SenderID = client.ID
			auditLog(client.ID, "",
				fmt.Sprintf("CONTROL_SESSION_END target=%s", msg.TargetID))
			data, _ := json.Marshal(msg)
			s.sendTo(msg.TargetID, data)

		// ---- WebRTC signaling (offer / answer / ICE candidate) -----
		case "offer", "answer", "candidate":
			msg.SenderID = client.ID
			data, _ := json.Marshal(msg)
			s.sendTo(msg.TargetID, data)

		// ---- Remote-control command (mouse / keyboard) -------------
		case "control":
			msg.SenderID = client.ID
			data, _ := json.Marshal(msg)
			s.sendTo(msg.TargetID, data)

		default:
			log.Printf("Unknown message type from %s: %s", client.ID, msg.Type)
		}
	}
}

// =====================================================================
// Entry point
// =====================================================================

func main() {
	server := NewServer()

	http.HandleFunc("/ws", server.handleConnection)

	// FUTURE: REST endpoints for audit queries, host management, health checks.
	// http.HandleFunc("/api/audit", server.handleAuditQuery)
	// http.HandleFunc("/api/hosts", server.handleHostList)
	// http.HandleFunc("/health",    server.handleHealth)

	addr := ":8080"
	if envAddr := os.Getenv("DRS_ADDR"); envAddr != "" {
		addr = envAddr
	}

	log.Printf("=== DRS Signaling Server starting on %s ===", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
