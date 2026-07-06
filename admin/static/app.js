// =====================================================================
// DRS Admin Dashboard — Frontend Logic
//
// This script handles:
//   1. WebSocket connection to the admin backend (proxy → DRS)
//   2. Host-list rendering with live preview thumbnails
//   3. WebRTC signaling to establish a video stream from a selected host
//   4. Control-mode input capture (mouse, keyboard, scroll)
//   5. UI state management (connect, disconnect, fullscreen)
//
// FUTURE HOOKS:
//   - Auth: send a token with the initial WS handshake
//   - Permissions: grey out "Take Control" for read-only viewers
//   - Multi-viewer: show which admin is currently controlling a host
//   - Session history sidebar tab
// =====================================================================

// ---- State ---------------------------------------------------------
let ws = null;
let myViewerID = null;
let peerConnection = null;
let selectedHostID = null;
let selectedHostName = null;
let controlMode = false;
let hosts = [];
let pendingCandidates = [];
let videoFrameCallbackId = null;
let lastFpsUpdate = 0;
let frameCount = 0;

// ---- DOM references ------------------------------------------------
const hostListEl        = document.getElementById('host-list');
const hostCountEl       = document.getElementById('host-count');
const noHostsEl         = document.getElementById('no-hosts');
const videoEl           = document.getElementById('remote-video');
const splashEl          = document.getElementById('splash');
const connHostNameEl    = document.getElementById('connected-host-name');
const connStatusEl      = document.getElementById('connection-status');
const controlBannerEl   = document.getElementById('control-banner');
const controlHostNameEl = document.getElementById('control-host-name');
const btnControl        = document.getElementById('btn-control');
const btnFullscreen     = document.getElementById('btn-fullscreen');
const btnDisconnect     = document.getElementById('btn-disconnect');
const selectResolution  = document.getElementById('select-resolution');
const selectQuality     = document.getElementById('select-quality');
const streamFpsEl       = document.getElementById('stream-fps');

// =====================================================================
// WebSocket connection
// =====================================================================

function connectWS() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${proto}//${location.host}/ws/viewer`);

    ws.onopen = () => {
        console.log('[WS] Connected to admin backend');
    };

    ws.onmessage = (event) => {
        try {
            const msg = JSON.parse(event.data);
            handleMessage(msg);
        } catch (e) {
            console.error('[WS] Bad JSON:', e);
        }
    };

    ws.onclose = () => {
        console.log('[WS] Disconnected — reconnecting in 3 s…');
        myViewerID = null;
        setTimeout(connectWS, 3000);
    };

    ws.onerror = (err) => {
        console.error('[WS] Error:', err);
    };
}

// =====================================================================
// Message dispatcher
// =====================================================================

function handleMessage(msg) {
    switch (msg.type) {
        case 'registered':
            myViewerID = msg.id;
            console.log('[WS] Registered as', myViewerID);
            break;

        case 'hosts_update':
            hosts = msg.hosts || [];
            renderHostList();
            break;

        case 'offer':
            handleOffer(msg);
            break;

        case 'candidate':
            handleRemoteCandidate(msg);
            break;

        default:
            console.log('[WS] Unhandled type:', msg.type);
    }
}

// =====================================================================
// Host list rendering
// =====================================================================

function renderHostList() {
    const count = hosts.length;
    hostCountEl.textContent = `${count} online`;

    // Show/hide empty state
    noHostsEl.classList.toggle('hidden', count > 0);

    // Keep track of existing DOM cards by their host ID
    const existingCards = {};
    hostListEl.querySelectorAll('.host-card').forEach(card => {
        existingCards[card.dataset.hostId] = card;
    });

    // Render/update cards for all online hosts
    hosts.forEach((host) => {
        let card = existingCards[host.id];

        if (!card) {
            // Create a new card if it doesn't exist
            card = document.createElement('div');
            card.className = 'host-card';
            card.dataset.hostId = host.id;
            card.addEventListener('click', () => selectHost(host.id, host.hostname));
        }

        // Apply/remove selected class
        card.classList.toggle('selected', host.id === selectedHostID);

        // Update card contents incrementally
        let previewEl = card.querySelector('.host-preview');
        if (!previewEl) {
            // Build the card internal structure on first creation
            const previewHTML = host.preview
                ? `<img src="${host.preview}" alt="Screen preview" />`
                : `<div class="no-preview">No preview available</div>`;

            card.innerHTML = `
                <div class="host-preview">${previewHTML}</div>
                <div class="host-info">
                    <span class="host-name" title="${host.hostname}">${host.hostname}</span>
                    <span class="host-status online">
                        <span class="status-circle"></span> Online
                    </span>
                </div>
            `;
        } else {
            // Update preview image src if present and changed, or switch to placeholder
            const img = previewEl.querySelector('img');
            if (host.preview) {
                if (img) {
                    if (img.src !== host.preview) {
                        img.src = host.preview;
                    }
                } else {
                    previewEl.innerHTML = `<img src="${host.preview}" alt="Screen preview" />`;
                }
            } else {
                if (img || !previewEl.querySelector('.no-preview')) {
                    previewEl.innerHTML = '<div class="no-preview">No preview available</div>';
                }
            }

            // Update hostname text if it has changed
            const nameEl = card.querySelector('.host-name');
            if (nameEl && nameEl.textContent !== host.hostname) {
                nameEl.textContent = host.hostname;
                nameEl.title = host.hostname;
            }
        }

        // Ensure the card is appended in correct order
        hostListEl.appendChild(card);

        // Remove from tracking map to prevent deletion
        delete existingCards[host.id];
    });

    // Remove any cards for hosts that are no longer online
    Object.values(existingCards).forEach(card => card.remove());
}

// =====================================================================
// Host selection & WebRTC initiation
// =====================================================================

function selectHost(hostID, hostName) {
    // Don't re-select the same host
    if (selectedHostID === hostID && peerConnection) return;

    // Tear down the previous session
    if (peerConnection) {
        disconnectHost(true); // send disconnect for old host
    }

    selectedHostID = hostID;
    selectedHostName = hostName;

    // Update UI to "connecting" state
    connHostNameEl.textContent = `Connecting to ${hostName}…`;
    connStatusEl.className = 'status-dot connecting';
    splashEl.classList.add('hidden');
    videoEl.style.display = 'block';
    btnDisconnect.disabled = false;

    // Highlight the selected card
    document.querySelectorAll('.host-card').forEach((c) =>
        c.classList.toggle('selected', c.dataset.hostId === hostID)
    );

    selectResolution.disabled = false;
    selectQuality.disabled = false;
    streamFpsEl.classList.add('hidden');
    streamFpsEl.textContent = '0 FPS';

    // Ask the DRS to relay a connection request to the target host
    wsSend({
        type: 'connect_host',
        target_id: hostID,
    });

    // Send initial settings
    sendSettings();
}

// =====================================================================
// WebRTC signaling
// =====================================================================

async function handleOffer(msg) {
    // Close any prior peer connection
    if (peerConnection) {
        peerConnection.close();
        peerConnection = null;
    }
    pendingCandidates = [];

    // FUTURE: add TURN servers here for NAT traversal
    const config = {
        iceServers: [{ urls: 'stun:stun.l.google.com:19302' }],
    };

    peerConnection = new RTCPeerConnection(config);

    // ---- Incoming media track --------------------------------------
    peerConnection.ontrack = (event) => {
        console.log('[WebRTC] Track received', event);
        if (event.streams && event.streams[0]) {
            videoEl.srcObject = event.streams[0];
        } else {
            if (!videoEl.srcObject || !(videoEl.srcObject instanceof MediaStream)) {
                videoEl.srcObject = new MediaStream();
            }
            videoEl.srcObject.addTrack(event.track);
        }

        videoEl.play().catch((err) => {
            console.warn('[WebRTC] Playback initialization error:', err);
        });

        connHostNameEl.textContent = `Connected to ${selectedHostName}`;
        connStatusEl.className = 'status-dot connected';
        btnControl.disabled = false;
        btnFullscreen.disabled = false;

        // Start FPS tracking
        streamFpsEl.classList.remove('hidden');
        streamFpsEl.textContent = '... FPS';
        frameCount = 0;
        lastFpsUpdate = 0;
        
        if (videoFrameCallbackId && videoEl.cancelVideoFrameCallback) {
            videoEl.cancelVideoFrameCallback(videoFrameCallbackId);
        }
        
        if (videoEl.requestVideoFrameCallback) {
            videoFrameCallbackId = videoEl.requestVideoFrameCallback(updateFpsCallback);
        } else {
            streamFpsEl.textContent = 'FPS N/A';
        }
    };

    // ---- Outbound ICE candidates -----------------------------------
    peerConnection.onicecandidate = (event) => {
        if (event.candidate) {
            wsSend({
                type: 'candidate',
                target_id: msg.sender_id,
                candidate: JSON.stringify(event.candidate.toJSON()),
            });
        }
    };

    // ---- Connection state changes ----------------------------------
    peerConnection.onconnectionstatechange = () => {
        const state = peerConnection.connectionState;
        console.log('[WebRTC] Connection state:', state);
        if (state === 'failed' || state === 'closed') {
            connStatusEl.className = 'status-dot disconnected';
            connHostNameEl.textContent = 'Connection lost';
        }
    };

    // ---- Set remote offer and create answer ------------------------
    try {
        await peerConnection.setRemoteDescription(
            new RTCSessionDescription({ type: 'offer', sdp: msg.sdp })
        );

        // Drain any candidates that arrived before the offer
        for (const c of pendingCandidates) {
            await peerConnection.addIceCandidate(new RTCIceCandidate(c));
        }
        pendingCandidates = [];

        const answer = await peerConnection.createAnswer();
        await peerConnection.setLocalDescription(answer);

        wsSend({
            type: 'answer',
            target_id: msg.sender_id,
            sdp: answer.sdp,
        });

        console.log('[WebRTC] Answer sent');
    } catch (e) {
        console.error('[WebRTC] Signaling error:', e);
    }
}

function handleRemoteCandidate(msg) {
    try {
        const candidate = JSON.parse(msg.candidate);

        if (peerConnection && peerConnection.remoteDescription) {
            peerConnection.addIceCandidate(new RTCIceCandidate(candidate));
        } else {
            // Buffer until remote description is set
            pendingCandidates.push(candidate);
        }
    } catch (e) {
        console.error('[WebRTC] Candidate parse error:', e);
    }
}

// =====================================================================
// Disconnect & cleanup
// =====================================================================

function disconnectHost(notify = true) {
    // Notify the host that we're leaving
    if (notify && selectedHostID) {
        wsSend({ type: 'disconnect_host', target_id: selectedHostID });
    }

    if (peerConnection) {
        peerConnection.close();
        peerConnection = null;
    }

    // Reset state
    controlMode = false;
    selectedHostID = null;
    selectedHostName = null;
    pendingCandidates = [];

    // Reset UI
    videoEl.srcObject = null;
    videoEl.style.display = 'none';
    videoEl.style.cursor = 'default';
    splashEl.classList.remove('hidden');
    connHostNameEl.textContent = 'Select a host';
    connStatusEl.className = 'status-dot';
    controlBannerEl.classList.add('hidden');

    btnControl.disabled = true;
    btnControl.classList.remove('active');
    btnControl.innerHTML = '<span class="btn-icon">⊙</span> Take Control';
    btnFullscreen.disabled = true;
    btnDisconnect.disabled = true;
    selectResolution.disabled = true;
    selectQuality.disabled = true;

    if (videoFrameCallbackId && videoEl.cancelVideoFrameCallback) {
        videoEl.cancelVideoFrameCallback(videoFrameCallbackId);
        videoFrameCallbackId = null;
    }
    lastFpsUpdate = 0;
    frameCount = 0;
    streamFpsEl.classList.add('hidden');

    // Clear card selection
    document.querySelectorAll('.host-card').forEach((c) => c.classList.remove('selected'));
}

// =====================================================================
// Control mode toggle
// =====================================================================

function toggleControl() {
    controlMode = !controlMode;

    if (controlMode) {
        btnControl.innerHTML = '<span class="btn-icon">⊙</span> Release Control';
        btnControl.classList.add('active');
        controlBannerEl.classList.remove('hidden');
        controlHostNameEl.textContent = selectedHostName;
        videoEl.style.cursor = 'crosshair';
    } else {
        btnControl.innerHTML = '<span class="btn-icon">⊙</span> Take Control';
        btnControl.classList.remove('active');
        controlBannerEl.classList.add('hidden');
        videoEl.style.cursor = 'default';
    }
}

// =====================================================================
// Mouse & keyboard input capture (sent only in control mode)
// =====================================================================

// ---- Mouse move ----------------------------------------------------
videoEl.addEventListener('mousemove', (e) => {
    if (!controlMode || !selectedHostID) return;

    const rect = videoEl.getBoundingClientRect();
    const x = (e.clientX - rect.left) / rect.width;
    const y = (e.clientY - rect.top) / rect.height;

    wsSend({
        type: 'control',
        target_id: selectedHostID,
        control: {
            action: 'mouse_move',
            x: clamp(x),
            y: clamp(y),
        },
    });
});

// ---- Mouse click ---------------------------------------------------
videoEl.addEventListener('mousedown', (e) => {
    if (!controlMode || !selectedHostID) return;
    e.preventDefault();

    const rect = videoEl.getBoundingClientRect();
    const x = (e.clientX - rect.left) / rect.width;
    const y = (e.clientY - rect.top) / rect.height;
    const buttons = ['left', 'middle', 'right'];

    wsSend({
        type: 'control',
        target_id: selectedHostID,
        control: {
            action: 'mouse_click',
            x: clamp(x),
            y: clamp(y),
            button: buttons[e.button] || 'left',
        },
    });
});

// ---- Mouse scroll --------------------------------------------------
videoEl.addEventListener('wheel', (e) => {
    if (!controlMode || !selectedHostID) return;
    e.preventDefault();

    // Normalise: positive = scroll up (matches robotgo convention)
    const scrollY = e.deltaY > 0 ? -3 : 3;

    wsSend({
        type: 'control',
        target_id: selectedHostID,
        control: {
            action: 'mouse_scroll',
            scroll_x: 0,
            scroll_y: scrollY,
        },
    });
}, { passive: false });

// ---- Keyboard ------------------------------------------------------
document.addEventListener('keydown', (e) => {
    if (!controlMode || !selectedHostID) return;
    e.preventDefault();

    const key = mapKey(e.key);
    if (!key) return;

    wsSend({
        type: 'control',
        target_id: selectedHostID,
        control: {
            action: 'key_press',
            key: key,
        },
    });
});

// ---- Prevent context menu on the video while in control mode -------
videoEl.addEventListener('contextmenu', (e) => {
    if (controlMode) e.preventDefault();
});

// =====================================================================
// Key mapping (browser key names → robotgo key names)
// =====================================================================

const KEY_MAP = {
    'enter':       'enter',
    'backspace':   'backspace',
    'tab':         'tab',
    'escape':      'escape',
    'arrowup':     'up',
    'arrowdown':   'down',
    'arrowleft':   'left',
    'arrowright':  'right',
    'delete':      'delete',
    'home':        'home',
    'end':         'end',
    'pageup':      'pageup',
    'pagedown':    'pagedown',
    ' ':           'space',
    'control':     'ctrl',
    'alt':         'alt',
    'shift':       'shift',
    'meta':        'cmd',
    'f1': 'f1', 'f2': 'f2', 'f3': 'f3', 'f4': 'f4',
    'f5': 'f5', 'f6': 'f6', 'f7': 'f7', 'f8': 'f8',
    'f9': 'f9', 'f10': 'f10', 'f11': 'f11', 'f12': 'f12',
};

function mapKey(browserKey) {
    const lower = browserKey.toLowerCase();
    if (KEY_MAP[lower]) return KEY_MAP[lower];
    // Single printable character → pass through
    if (lower.length === 1) return lower;
    return null;
}

// =====================================================================
// Button handlers
// =====================================================================

btnControl.addEventListener('click', toggleControl);

btnFullscreen.addEventListener('click', () => {
    const container = document.getElementById('video-container');
    if (container.requestFullscreen) {
        container.requestFullscreen();
    } else if (container.webkitRequestFullscreen) {
        container.webkitRequestFullscreen();
    }
});

btnDisconnect.addEventListener('click', () => {
    disconnectHost(true);
});

function sendSettings() {
    if (!selectedHostID) return;
    const width = parseInt(selectResolution.value, 10);
    const quality = parseInt(selectQuality.value, 10);
    wsSend({
        type: 'settings',
        target_id: selectedHostID,
        settings: {
            width: width,
            quality: quality
        }
    });
    console.log(`[Settings] Sent settings to host ${selectedHostID}: width=${width}, quality=${quality}`);
}

selectResolution.addEventListener('change', sendSettings);
selectQuality.addEventListener('change', sendSettings);

function updateFpsCallback(now, metadata) {
    frameCount++;
    if (!lastFpsUpdate) {
        lastFpsUpdate = now;
    }
    const elapsed = now - lastFpsUpdate;
    if (elapsed >= 1000) {
        const fps = Math.round((frameCount * 1000) / elapsed);
        streamFpsEl.textContent = `${fps} FPS`;
        frameCount = 0;
        lastFpsUpdate = now;
    }
    if (videoEl.srcObject) {
        videoFrameCallbackId = videoEl.requestVideoFrameCallback(updateFpsCallback);
    }
}

// =====================================================================
// Helpers
// =====================================================================

function wsSend(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(obj));
    }
}

function clamp(v) {
    return Math.max(0, Math.min(1, v));
}

// =====================================================================
// Boot
// =====================================================================

connectWS();
