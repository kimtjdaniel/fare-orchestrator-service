package orchestrator

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func (b *Brain) ServeGroupEvents(w http.ResponseWriter, r *http.Request, groupID string) {
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	go discardWS(conn)

	var last int64 = -1
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	send := func() bool {
		env, rev, err := b.GroupEventsEnvelope(r.Context(), groupID)
		if err != nil {
			env = map[string]any{
				"version": 1, "type": "connection.error", "groupId": groupID,
				"message": err.Error(), "revision": 0,
			}
			rev = 0
		}
		if rev == last && env["type"] != "connection.error" {
			return true
		}
		last = rev
		raw, err := json.Marshal(env)
		if err != nil {
			return false
		}
		return writeWSText(conn, raw) == nil
	}
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if !send() {
				return
			}
		}
	}
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, fmt.Errorf("not a websocket upgrade")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing websocket key", http.StatusBadRequest)
		return nil, fmt.Errorf("missing key")
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return nil, fmt.Errorf("no hijack")
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := buf.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func writeWSText(conn net.Conn, payload []byte) error {
	n := len(payload)
	var hdr []byte
	switch {
	case n < 126:
		hdr = []byte{0x81, byte(n)}
	case n < 65536:
		hdr = []byte{0x81, 126, byte(n >> 8), byte(n)}
	default:
		hdr = make([]byte, 10)
		hdr[0], hdr[1] = 0x81, 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(hdr); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

func discardWS(conn net.Conn) {
	reader := bufio.NewReader(conn)
	buf := make([]byte, 4096)
	for {
		if _, err := reader.Read(buf); err != nil {
			if err != io.EOF {
				_ = conn.Close()
			}
			return
		}
	}
}
