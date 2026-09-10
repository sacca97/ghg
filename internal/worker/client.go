package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

type Client struct {
	conn      net.Conn
	sessionID string

	writeMu   sync.Mutex
	closeOnce sync.Once
	frames    chan Frame
	errs      chan error
	done      chan struct{}
}

// NewClient wraps an established connection for worker communication.
func NewClient(conn net.Conn, sessionID string) *Client {
	return &Client{
		conn:      conn,
		sessionID: sessionID,
		frames:    make(chan Frame, outboundCapacity),
		errs:      make(chan error, 1),
		done:      make(chan struct{}),
	}
}

func Dial(ctx context.Context, runtime Runtime) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", runtime.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("dial worker: %w", err)
	}
	c := NewClient(conn, runtime.SessionID)
	if err := c.write(Frame{
		Version:   ProtocolVersion,
		SessionID: runtime.SessionID,
		Type:      TypeAttach,
	}); err != nil {
		c.Close()
		return nil, err
	}
	// Do not report a successful dial until the worker has accepted this
	// client. In particular, a live worker may already have another
	// controller; treating the socket connection as success makes callers
	// believe they are attached when the server has already rejected them.
	deadline := time.Now().Add(10 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetReadDeadline(deadline)
	decoder := NewDecoder(conn)
	initial := make([]Frame, 0, 2)
	for len(initial) < 2 {
		frame, readErr := decoder.Read()
		if readErr != nil {
			_ = c.Close()
			return nil, fmt.Errorf("worker attach: %w", readErr)
		}
		switch frame.Type {
		case TypeAlreadyControlled, TypeError:
			_ = c.Close()
			var payload ErrorPayload
			if json.Unmarshal(frame.Payload, &payload) == nil && payload.Message != "" {
				return nil, errors.New(payload.Message)
			}
			return nil, errors.New("worker rejected attach")
		case TypeSnapshot, TypeAttached:
			initial = append(initial, frame)
		default:
			_ = c.Close()
			return nil, fmt.Errorf("worker attach: unexpected frame %q", frame.Type)
		}
	}
	if initial[0].Type != TypeSnapshot || initial[1].Type != TypeAttached {
		_ = c.Close()
		return nil, errors.New("worker attach: invalid handshake")
	}
	_ = conn.SetReadDeadline(time.Time{})
	for _, frame := range initial {
		c.frames <- frame
	}
	go c.readLoop(decoder)
	return c, nil
}

func (c *Client) Frames() <-chan Frame { return c.frames }

func (c *Client) Errors() <-chan error { return c.errs }

func (c *Client) Send(name, requestID string, payload any) error {
	if requestID == "" || len(requestID) > MaxRequestIDBytes {
		return errors.New("worker request id is invalid")
	}
	if !knownCommand(name) {
		return fmt.Errorf("unknown worker command %q", name)
	}
	raw, err := marshalPayload(payload)
	if err != nil {
		return err
	}
	return c.write(Frame{
		Version:   ProtocolVersion,
		SessionID: c.sessionID,
		Type:      TypeCommand,
		RequestID: requestID,
		Payload:   mustPayload(CommandRequest{Name: name, Payload: raw}),
	})
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.conn.Close()
	})
	return err
}

func (c *Client) write(frame Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return net.ErrClosed
	default:
	}
	return WriteFrame(c.conn, frame)
}

func (c *Client) readLoop(decoder *Decoder) {
	defer close(c.frames)
	defer close(c.errs)
	for {
		frame, err := decoder.Read()
		if err != nil {
			select {
			case <-c.done:
			default:
				if errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
					return
				}
				select {
				case c.errs <- err:
				case <-c.done:
				}
			}
			return
		}
		if frame.SessionID != c.sessionID {
			select {
			case c.errs <- fmt.Errorf("worker session mismatch"):
			case <-c.done:
			}
			return
		}
		select {
		case c.frames <- frame:
		case <-c.done:
			return
		}
	}
}
