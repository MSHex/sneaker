package server

import (
	"bufio"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/crc64"
	"io"
	"log"
	"math/bits"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/b1naryth1ef/jambon/tacview"
)

const (
	defaultTacViewPort = 42674
	dialTimeout        = 10 * time.Second
	handshakeTimeout   = 15 * time.Second
)

// TacViewClient connects to a Tacview real-time telemetry server (the Tacview
// exporter running inside DCS) and streams parsed time frames.
//
// This replaces jambon's NewRealTimeReader/ProcessTimeFrames, which leaked the
// socket on handshake failures, had no timeouts, and stalled forever (or
// panicked) on a single malformed ACMI line.
type TacViewClient struct {
	host     string
	port     int
	password string
	name     string
}

func NewTacViewClient(host string, port int, password string, name string) *TacViewClient {
	if port == 0 {
		port = defaultTacViewPort
	}

	return &TacViewClient{host: normalizeDialHost(host), port: port, password: password, name: name}
}

// normalizeDialHost turns wildcard listen addresses (which DCSServerBot writes
// into sneaker.json when the node's listen_address is 0.0.0.0) into loopback.
// Dialing 0.0.0.0 fails on Windows.
func normalizeDialHost(host string) string {
	h := strings.TrimSpace(host)
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	switch h {
	case "", "0.0.0.0", "::", "*":
		return "127.0.0.1"
	}
	return h
}

// Start performs the handshake and returns the stream header, a channel of
// time frames, and a function that closes the connection. The channel is
// closed when the connection ends; the reason is available via the returned
// error pointer once the channel has been drained.
func (c *TacViewClient) Start(onConnected func()) (*tacview.Header, <-chan *tacview.TimeFrame, func(), *error, error) {
	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))

	conn, reader, err := c.handshake(addr, hashPassword64)
	if err == io.EOF && c.password == "" {
		err = errors.New("Tacview closed the connection during the handshake: a real-time telemetry password is probably set in Tacview's options; add it as \"password\" for this server in sneaker.json")
	} else if err == io.EOF && c.password != "" {
		// Older Tacview versions hash the password with CRC32
		conn, reader, err = c.handshake(addr, hashPassword32)
		if err == io.EOF {
			err = errors.New("connection closed during handshake (wrong realtime telemetry password?)")
		}
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if onConnected != nil {
		onConnected()
	}

	// No read deadline from here on. Tacview may hold the stream back for a
	// long time (tacviewPlaybackDelay, a paused or empty server, mission
	// still loading); TCP keep-alive detects a dead peer instead.
	acmi := newACMIReader(reader)
	header, err := acmi.readHeader()
	if err != nil {
		conn.Close()
		return nil, nil, nil, nil, fmt.Errorf("failed to read ACMI header: %w", err)
	}

	frames := make(chan *tacview.TimeFrame, 64)
	var streamErr error
	go func() {
		defer close(frames)
		for {
			tf, err := acmi.readTimeFrame()
			if err != nil {
				streamErr = err
				return
			}
			frames <- tf
		}
	}()

	return header, frames, func() { conn.Close() }, &streamErr, nil
}

func (c *TacViewClient) handshake(addr string, hashFunc func(string) string) (net.Conn, *bufio.Reader, error) {
	dialer := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, nil, err
	}

	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	reader := bufio.NewReaderSize(conn, 64*1024)

	headerProtocol, err := reader.ReadString('\n')
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimRight(headerProtocol, "\r\n") != "XtraLib.Stream.0" {
		return nil, nil, fmt.Errorf("bad header protocol: %q", headerProtocol)
	}

	headerVersion, err := reader.ReadString('\n')
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimRight(headerVersion, "\r\n") != "Tacview.RealTimeTelemetry.0" {
		return nil, nil, fmt.Errorf("bad header version: %q", headerVersion)
	}

	// Remote hostname
	if _, err = reader.ReadString('\n'); err != nil {
		return nil, nil, err
	}

	eoh, err := reader.ReadByte()
	if err != nil {
		return nil, nil, err
	}
	if eoh != '\x00' {
		return nil, nil, errors.New("bad or missing end of header")
	}

	username := c.name
	if username == "" {
		username = "sneakerserver"
	}
	hs := fmt.Sprintf("XtraLib.Stream.0\nTacview.RealTimeTelemetry.0\n%s\n%s\x00", username, hashFunc(c.password))
	if _, err = conn.Write([]byte(hs)); err != nil {
		return nil, nil, err
	}

	conn.SetDeadline(time.Time{})
	ok = true
	return conn, reader, nil
}

func hashPassword32(password string) string {
	passwordUTF16 := utf16.Encode([]rune(password))

	passwordBytes := make([]byte, 2*len(passwordUTF16))
	for i, r := range passwordUTF16 {
		passwordBytes[2*i+0] = byte(r >> 0)
		passwordBytes[2*i+1] = byte(r >> 8)
	}

	return fmt.Sprintf("%x", crc32.ChecksumIEEE(passwordBytes))
}

var crc64Table = crc64.MakeTable(crc64.ECMA)

func hashPassword64(password string) string {
	passwordUTF16 := utf16.Encode([]rune(password))

	passwordBytes := make([]byte, 2*len(passwordUTF16))
	for i, r := range passwordUTF16 {
		passwordBytes[2*i+0] = bits.Reverse8(byte(r >> 0))
		passwordBytes[2*i+1] = bits.Reverse8(byte(r >> 8))
	}

	hash := bits.Reverse64(crc64.Checksum(passwordBytes, crc64Table))
	return fmt.Sprintf("%x", hash)
}

// acmiReader is a tolerant line-based ACMI 2.x reader. Malformed lines are
// logged and skipped instead of aborting the whole stream.
type acmiReader struct {
	r *bufio.Reader

	// Offset of the time frame currently being read
	offset float64
	// A "#offset" line read while finishing the previous frame
	pendingOffset *float64
	badLines      int
}

func newACMIReader(r *bufio.Reader) *acmiReader {
	return &acmiReader{r: r}
}

// readLine returns one logical ACMI line, joining "\"-continued lines and
// stripping a UTF-8 BOM and CR/LF.
func (a *acmiReader) readLine() (string, error) {
	var sb strings.Builder
	for {
		line, err := a.r.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		line = strings.TrimPrefix(line, "\uFEFF")

		if strings.HasSuffix(line, "\\") && !strings.HasSuffix(line, "\\\\") {
			sb.WriteString(line[:len(line)-1])
			sb.WriteByte('\n')
			if err == io.EOF {
				return sb.String(), nil
			}
			continue
		}
		sb.WriteString(line)
		return sb.String(), nil
	}
}

func (a *acmiReader) readHeader() (*tacview.Header, error) {
	header := &tacview.Header{}

	// FileType / FileVersion
	for header.FileType == "" || header.FileVersion == "" {
		line, err := a.readLine()
		if err != nil {
			return nil, err
		}
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("unexpected ACMI header line: %q", line)
		}
		switch key {
		case "FileType":
			header.FileType = value
		case "FileVersion":
			header.FileVersion = value
		default:
			return nil, fmt.Errorf("unexpected ACMI header line: %q", line)
		}
	}

	// Initial (offset-less) time frame, ends at the first "#" line.
	initial, err := a.readFrameBody(0)
	if err != nil {
		return nil, err
	}
	header.InitialTimeFrame = *initial

	globalObj := initial.Get(0)
	if globalObj == nil {
		return nil, errors.New("no global object found in initial time frame")
	}
	if ref := globalObj.Get("ReferenceTime"); ref != nil {
		if t, err := time.Parse(time.RFC3339, ref.Value); err == nil {
			header.ReferenceTime = t
		}
	}
	return header, nil
}

func (a *acmiReader) readTimeFrame() (*tacview.TimeFrame, error) {
	if a.pendingOffset == nil {
		// Skip anything until the next "#" line
		for a.pendingOffset == nil {
			line, err := a.readLine()
			if err != nil {
				return nil, err
			}
			a.handleOffsetLine(line)
		}
	}
	offset := *a.pendingOffset
	a.pendingOffset = nil
	return a.readFrameBody(offset)
}

func (a *acmiReader) handleOffsetLine(line string) bool {
	if !strings.HasPrefix(line, "#") {
		return false
	}
	offset, err := strconv.ParseFloat(strings.TrimSpace(line[1:]), 64)
	if err != nil {
		a.logBadLine(line, err)
		return true
	}
	a.pendingOffset = &offset
	return true
}

// readFrameBody reads object lines until the next "#offset" line (which is
// remembered for the next frame) and returns the parsed frame.
func (a *acmiReader) readFrameBody(offset float64) (*tacview.TimeFrame, error) {
	tf := tacview.NewTimeFrame()
	tf.Offset = offset
	a.offset = offset

	for {
		line, err := a.readLine()
		if err != nil {
			if err == io.EOF && len(tf.Objects) > 0 {
				return tf, nil
			}
			return nil, err
		}
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if a.handleOffsetLine(line) {
			if a.pendingOffset != nil {
				return tf, nil
			}
			continue
		}

		obj, err := parseACMIObjectLine(line)
		if err != nil {
			a.logBadLine(line, err)
			continue
		}
		tf.Objects = append(tf.Objects, obj)
	}
}

func (a *acmiReader) logBadLine(line string, err error) {
	a.badLines++
	// Avoid flooding the log (and the DCSServerBot console) on a bad stream
	if a.badLines <= 20 || a.badLines%1000 == 0 {
		if len(line) > 200 {
			line = line[:200] + "..."
		}
		log.Printf("[acmi] skipping malformed line (%d so far): %v: %q", a.badLines, err, line)
	}
}

// parseACMIObjectLine parses "id,key=value,key=value" or "-id".
func parseACMIObjectLine(line string) (*tacview.Object, error) {
	deleted := false
	if line[0] == '-' {
		deleted = true
		line = line[1:]
	}

	idPart, rest, hasProps := strings.Cut(line, ",")
	id, err := strconv.ParseUint(strings.TrimSpace(idPart), 16, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid object id: %w", err)
	}

	obj := &tacview.Object{Id: id, Deleted: deleted, Properties: make([]*tacview.Property, 0)}
	if deleted || !hasProps {
		return obj, nil
	}

	for _, token := range splitACMIProperties(rest) {
		if token == "" {
			continue
		}
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			return nil, fmt.Errorf("invalid property %q", token)
		}
		obj.Properties = append(obj.Properties, &tacview.Property{Key: key, Value: value})
	}
	return obj, nil
}

// splitACMIProperties splits on unescaped commas and unescapes "\," and "\\".
func splitACMIProperties(s string) []string {
	if !strings.Contains(s, "\\") {
		return strings.Split(s, ",")
	}

	var out []string
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\\' && i+1 < len(s) && (s[i+1] == ',' || s[i+1] == '\\') {
			sb.WriteByte(s[i+1])
			i++
			continue
		}
		if ch == ',' {
			out = append(out, sb.String())
			sb.Reset()
			continue
		}
		sb.WriteByte(ch)
	}
	return append(out, sb.String())
}
