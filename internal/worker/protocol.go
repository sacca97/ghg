package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ProtocolVersion   = 1
	MaxFrameBytes     = 1 << 20
	MaxAggregateBytes = 64 << 20
	MaxRequestIDBytes = 128
	MaxSessionIDBytes = 64
)

const (
	TypeAttach            = "attach"
	TypeAttached          = "attached"
	TypeSnapshot          = "snapshot"
	TypeEvent             = "event"
	TypeCommand           = "command"
	TypeAck               = "ack"
	TypeDetachAck         = "detach_ack"
	TypeAlreadyControlled = "already_controlled"
	TypeError             = "error"
)

const (
	EventPlanDelta = "plan_delta"
)

const (
	CommandDetach          = "detach"
	CommandCancel          = "cancel"
	CommandInput           = "input"
	CommandApprove         = "approve"
	CommandConfigure       = "configure"
	CommandCompact         = "compact"
	CommandStop            = "stop"
	CommandPing            = "ping"
	CommandLSPStatus       = "lsp_status"
	CommandMCPStatus       = "mcp_status"
	CommandMCPReconnect    = "mcp_reconnect"
	CommandMCPEnable       = "mcp_enable"
	CommandMCPDisable      = "mcp_disable"
	CommandContextDoctor   = "context_doctor"
	CommandRewind          = "rewind"
	CommandCompactRetry    = "compact_retry"
	CommandGoal            = "goal"
	CommandGoalFromContext = "goal_from_context"
	// CommandChdir retargets the worker process at the TUI's new working
	// directory: the worker owns the tools and sandbox, so a TUI-side chdir
	// alone would leave it reading and editing the original workspace.
	CommandChdir = "chdir"
	// CommandAppend appends a local user-role context message to the
	// worker-owned conversation (or steers the running turn) — the `!` shell
	// escape output has to reach the model that actually answers next.
	CommandAppend = "append"
)

var (
	ErrFrameTooLarge     = errors.New("worker protocol frame exceeds limit")
	ErrAggregateTooLarge = errors.New("worker protocol aggregate exceeds limit")
	ErrProtocol          = errors.New("invalid worker protocol frame")
	ErrUnknownVersion    = errors.New("unknown worker protocol version")
	ErrUnknownType       = errors.New("unknown worker protocol type")
)

// Frame is the complete wire envelope. Payload is JSON so strings can contain
// escaped newlines without changing the line framing.
type Frame struct {
	Version   int             `json:"version"`
	SessionID string          `json:"session_id"`
	Seq       uint64          `json:"seq,omitempty"`
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type AttachRequest struct{}

type CommandRequest struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type SnapshotEnvelope struct {
	State json.RawMessage `json:"state"`
}

type EventEnvelope struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

type ErrorPayload struct {
	Message string `json:"message"`
}

// Decoder applies both the per-frame and lifetime input limits. The lifetime
// limit is intentionally generous for a long-lived stream, while still
// bounding an abusive connection that never sends useful commands.
type Decoder struct {
	reader         *bufio.Reader
	frameLimit     int
	aggregateLimit int64
	total          int64
}

func NewDecoder(r io.Reader) *Decoder {
	return NewDecoderWithLimits(r, MaxFrameBytes, MaxAggregateBytes)
}

func NewDecoderWithLimits(r io.Reader, frameLimit int, aggregateLimit int64) *Decoder {
	if frameLimit <= 0 {
		frameLimit = MaxFrameBytes
	}
	if aggregateLimit <= 0 {
		aggregateLimit = MaxAggregateBytes
	}
	return &Decoder{
		reader:         bufio.NewReaderSize(r, 32<<10),
		frameLimit:     frameLimit,
		aggregateLimit: aggregateLimit,
	}
}

func (d *Decoder) Read() (Frame, error) {
	line, err := d.readLine()
	if err != nil {
		return Frame{}, err
	}
	d.total += int64(len(line))
	if d.total > d.aggregateLimit {
		return Frame{}, ErrAggregateTooLarge
	}

	var frame Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	if err := validateFrame(frame); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func (d *Decoder) readLine() ([]byte, error) {
	var line []byte
	for {
		part, err := d.reader.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > d.frameLimit {
			return nil, ErrFrameTooLarge
		}
		if err == nil {
			if len(line) == 1 {
				return nil, ErrProtocol
			}
			return bytes.TrimSuffix(line, []byte{'\n'}), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("%w: incomplete frame", ErrProtocol)
		}
		return nil, err
	}
}

func WriteFrame(w io.Writer, frame Frame) error {
	line, err := encodeFrame(frame)
	if err != nil {
		return err
	}
	n, err := w.Write(line)
	if err != nil {
		return err
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	return nil
}

func encodeFrame(frame Frame) ([]byte, error) {
	if err := validateFrame(frame); err != nil {
		return nil, err
	}
	line, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("marshal worker frame: %w", err)
	}
	line = append(line, '\n')
	if len(line) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	return line, nil
}

func marshalPayload(value any) (json.RawMessage, error) {
	if value == nil {
		return json.RawMessage("null"), nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal worker payload: %w", err)
	}
	return payload, nil
}

func validateFrame(frame Frame) error {
	if frame.Version != ProtocolVersion {
		return fmt.Errorf("%w: %d", ErrUnknownVersion, frame.Version)
	}
	if frame.SessionID == "" || len(frame.SessionID) > MaxSessionIDBytes {
		return fmt.Errorf("%w: invalid session id", ErrProtocol)
	}
	if len(frame.RequestID) > MaxRequestIDBytes {
		return fmt.Errorf("%w: request id too long", ErrProtocol)
	}
	if !knownType(frame.Type) {
		return fmt.Errorf("%w: %q", ErrUnknownType, frame.Type)
	}
	if frame.Type == TypeCommand && frame.RequestID == "" {
		return fmt.Errorf("%w: command request id is required", ErrProtocol)
	}
	if len(frame.Payload) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	return nil
}

func knownType(kind string) bool {
	switch kind {
	case TypeAttach, TypeAttached, TypeSnapshot, TypeEvent, TypeCommand,
		TypeAck, TypeDetachAck, TypeAlreadyControlled, TypeError:
		return true
	default:
		return false
	}
}

func knownCommand(name string) bool {
	switch name {
	case CommandDetach, CommandCancel, CommandInput, CommandApprove,
		CommandConfigure, CommandCompact,
		CommandStop, CommandPing, CommandLSPStatus, CommandMCPStatus,
		CommandMCPReconnect, CommandMCPEnable, CommandMCPDisable, CommandContextDoctor, CommandRewind,
		CommandCompactRetry, CommandGoal, CommandGoalFromContext,
		CommandChdir, CommandAppend:
		return true
	default:
		return false
	}
}
