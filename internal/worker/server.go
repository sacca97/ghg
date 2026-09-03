package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const (
	outboundCapacity = 128
	requestCacheSize = 256
)

type Handler interface {
	Snapshot(context.Context) (any, error)
	Command(context.Context, Command) (CommandResult, error)
	Disconnected(context.Context, bool)
}

type attachHandler interface {
	Attached(context.Context)
}

type Command struct {
	Name      string
	RequestID string
	Payload   json.RawMessage
}

type CommandResult struct {
	Payload json.RawMessage
	Detach  bool
	Close   bool
	// AfterAck runs only after a response has been written to the controlling
	// socket. Detach uses it so a disconnect before the acknowledgement cannot
	// silently authorize clientless work.
	AfterAck func()
}

type Server struct {
	runtime  Runtime
	lock     *Lock
	listener net.Listener
	handler  Handler

	mu           sync.Mutex
	controller   *peer
	attaching    bool
	seq          uint64
	requests     map[string]Frame
	requestOrder []string
	detached     bool
	closed       bool
	closeOnce    sync.Once
	connections  sync.WaitGroup
}

func NewServer(runtime Runtime, handler Handler) (*Server, error) {
	if handler == nil {
		return nil, errors.New("worker handler is nil")
	}
	lock, err := runtime.Acquire()
	if err != nil {
		return nil, err
	}
	listener, err := runtime.Listen()
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Server{
		runtime:  runtime,
		lock:     lock,
		listener: listener,
		handler:  handler,
		requests: make(map[string]Frame),
	}, nil
}

func (s *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if done := ctx.Done(); done != nil {
		go func() {
			<-done
			_ = s.Close()
		}()
	}
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.connections.Add(1)
		go func() {
			defer s.connections.Done()
			s.serveConnection(conn)
		}()
	}
}

func (s *Server) Wait() {
	s.connections.Wait()
}

func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		controller := s.controller
		s.controller = nil
		s.mu.Unlock()
		if controller != nil {
			controller.close()
		}
		if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = err
		}
		if err := s.runtime.RemoveSocket(); err != nil {
			if closeErr == nil {
				closeErr = err
			}
		}
		if err := s.lock.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}

func (s *Server) Publish(kind string, data any, important bool) (uint64, error) {
	if kind == "" {
		return 0, errors.New("worker event kind is empty")
	}
	raw, err := marshalPayload(data)
	if err != nil {
		return 0, err
	}
	eventPayload, err := marshalPayload(EventEnvelope{Kind: kind, Data: raw})
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, net.ErrClosed
	}
	s.seq++
	frame := Frame{
		Version:   ProtocolVersion,
		SessionID: s.runtime.SessionID,
		Seq:       s.seq,
		Type:      TypeEvent,
		Payload:   eventPayload,
	}
	if _, err := encodeFrame(frame); err != nil {
		return 0, err
	}
	if s.controller != nil {
		s.controller.enqueue(frame, important)
	}
	return s.seq, nil
}

func (s *Server) Detached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detached
}

func (s *Server) ControllerPresent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controller != nil
}

func (s *Server) serveConnection(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	decoder := NewDecoder(conn)
	first, err := decoder.Read()
	if err != nil {
		return
	}
	if first.Type != TypeAttach || first.SessionID != s.runtime.SessionID {
		_ = writeErrorWithDeadline(conn, s.runtime.SessionID, "attach required")
		return
	}
	if len(first.Payload) != 0 && string(first.Payload) != "null" {
		var attach AttachRequest
		dec := json.NewDecoder(bytes.NewReader(first.Payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&attach); err != nil {
			_ = writeErrorWithDeadline(conn, s.runtime.SessionID, "invalid attach payload")
			return
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	p := newPeer(conn)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		p.close()
		return
	}
	if s.controller != nil || s.attaching {
		s.mu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = WriteFrame(conn, Frame{
			Version:   ProtocolVersion,
			SessionID: s.runtime.SessionID,
			Type:      TypeAlreadyControlled,
			Payload:   mustPayload(ErrorPayload{Message: "worker already has a controlling client"}),
		})
		_ = conn.SetWriteDeadline(time.Time{})
		p.close()
		return
	}
	s.attaching = true
	s.mu.Unlock()
	if attached, ok := s.handler.(attachHandler); ok {
		attached.Attached(context.Background())
	}
	s.mu.Lock()
	if s.closed {
		s.attaching = false
		s.mu.Unlock()
		p.close()
		return
	}
	s.attaching = false
	state, snapshotSeq, snapshotErr := s.snapshotLocked(context.Background())
	if snapshotErr != nil {
		s.mu.Unlock()
		_ = writeErrorWithDeadline(conn, s.runtime.SessionID, snapshotErr.Error())
		p.close()
		s.handler.Disconnected(context.Background(), false)
		return
	}

	// Keep the server lock through the initial queueing. Publish must not be
	// able to put an event ahead of the snapshot that describes its sequence.
	s.controller = p
	s.detached = false
	p.startWriter()
	_ = p.enqueue(Frame{
		Version:   ProtocolVersion,
		SessionID: s.runtime.SessionID,
		Seq:       snapshotSeq,
		Type:      TypeSnapshot,
		Payload:   mustPayload(SnapshotEnvelope{State: state}),
	}, true)
	_ = p.enqueue(Frame{
		Version:   ProtocolVersion,
		SessionID: s.runtime.SessionID,
		Seq:       snapshotSeq,
		Type:      TypeAttached,
		Payload:   mustPayload(map[string]any{"last_seq": snapshotSeq}),
	}, true)
	s.mu.Unlock()

	for {
		frame, err := decoder.Read()
		if err != nil {
			s.clearController(p, s.Detached())
			p.close()
			return
		}
		if frame.SessionID != s.runtime.SessionID || frame.Type != TypeCommand {
			p.enqueue(errorFrame(s.runtime.SessionID, "command expected"), true)
			continue
		}
		s.handleCommand(p, frame)
	}
}

func (s *Server) snapshotLocked(ctx context.Context) (json.RawMessage, uint64, error) {
	state, err := s.handler.Snapshot(ctx)
	if err != nil {
		return nil, 0, err
	}
	raw, err := marshalPayload(state)
	if err != nil {
		return nil, 0, err
	}
	return raw, s.seq, nil
}

func (s *Server) handleCommand(p *peer, frame Frame) {
	if cached, ok := s.cached(frame.RequestID); ok {
		p.enqueue(cached, true)
		return
	}
	var request CommandRequest
	if err := json.Unmarshal(frame.Payload, &request); err != nil || !knownCommand(request.Name) {
		response := errorFrame(s.runtime.SessionID, "unknown worker command")
		response.RequestID = frame.RequestID
		s.remember(frame.RequestID, response)
		p.enqueue(response, true)
		return
	}
	result, err := s.handler.Command(context.Background(), Command{
		Name:      request.Name,
		RequestID: frame.RequestID,
		Payload:   request.Payload,
	})
	if err != nil {
		response := errorFrame(s.runtime.SessionID, err.Error())
		response.RequestID = frame.RequestID
		s.remember(frame.RequestID, response)
		p.enqueue(response, true)
		return
	}
	responseType := TypeAck
	if result.Detach {
		responseType = TypeDetachAck
	}
	response := Frame{
		Version:   ProtocolVersion,
		SessionID: s.runtime.SessionID,
		Type:      responseType,
		RequestID: frame.RequestID,
		Payload:   result.Payload,
	}
	if len(response.Payload) == 0 {
		response.Payload = json.RawMessage("null")
	}
	if result.Detach {
		if err := p.enqueueAndWait(response, true); err != nil {
			p.close()
			s.clearController(p, false)
			return
		}
		s.mu.Lock()
		s.detached = true
		s.rememberLocked(frame.RequestID, response)
		s.mu.Unlock()
		if result.AfterAck != nil {
			result.AfterAck()
		}
		return
	}
	s.remember(frame.RequestID, response)
	p.enqueue(response, true)
	if result.Close {
		p.close()
	}
}

func (s *Server) clearController(p *peer, detached bool) {
	s.mu.Lock()
	if s.controller == p {
		s.controller = nil
	}
	s.mu.Unlock()
	s.handler.Disconnected(context.Background(), detached)
}

func errorFrame(sessionID, message string) Frame {
	return Frame{
		Version:   ProtocolVersion,
		SessionID: sessionID,
		Type:      TypeError,
		Payload:   mustPayload(ErrorPayload{Message: message}),
	}
}

func writeError(w io.Writer, sessionID, message string) error {
	return WriteFrame(w, errorFrame(sessionID, message))
}

func writeErrorWithDeadline(conn net.Conn, sessionID, message string) error {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	return writeError(conn, sessionID, message)
}

type peer struct {
	conn net.Conn
	send chan outboundFrame

	mu     sync.Mutex
	closed bool
}

type outboundFrame struct {
	frame Frame
	done  chan error
}

func newPeer(conn net.Conn) *peer {
	return &peer{
		conn: conn, send: make(chan outboundFrame, outboundCapacity),
	}
}

func (p *peer) startWriter() {
	go func() {
		for item := range p.send {
			err := WriteFrame(p.conn, item.frame)
			if item.done != nil {
				item.done <- err
			}
			if err != nil {
				p.close()
				return
			}
		}
	}()
}

func (p *peer) enqueue(frame Frame, important bool) bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	select {
	case p.send <- outboundFrame{frame: frame}:
		p.mu.Unlock()
		return true
	default:
	}
	p.mu.Unlock()
	if important {
		// A slow client must not make the worker wait and must not cause a
		// lifecycle frame to disappear. It can rebuild from the next snapshot.
		p.close()
	}
	return false
}

func (p *peer) enqueueAndWait(frame Frame, important bool) error {
	done := make(chan error, 1)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return net.ErrClosed
	}
	item := outboundFrame{frame: frame, done: done}
	select {
	case p.send <- item:
		p.mu.Unlock()
	default:
		p.mu.Unlock()
		if important {
			p.close()
		}
		return errors.New("worker client queue is full")
	}
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		p.close()
		return errors.New("worker response write timed out")
	}
}

func (s *Server) cached(id string) (Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame, ok := s.requests[id]
	return frame, ok
}

func (s *Server) remember(id string, frame Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rememberLocked(id, frame)
}

func (s *Server) rememberLocked(id string, frame Frame) {
	if _, exists := s.requests[id]; exists {
		return
	}
	s.requests[id] = frame
	s.requestOrder = append(s.requestOrder, id)
	if len(s.requestOrder) > requestCacheSize {
		delete(s.requests, s.requestOrder[0])
		s.requestOrder = s.requestOrder[1:]
	}
}

func (p *peer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.send)
	p.mu.Unlock()
	_ = p.conn.Close()
}

func mustPayload(value any) json.RawMessage {
	payload, err := marshalPayload(value)
	if err != nil {
		panic(err)
	}
	return payload
}
