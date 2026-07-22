package runtime

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	spoolRecordHeader = 13 // seq uint64, stream byte, payload length uint32
	spoolSegmentBytes = 256 << 10
)

var spoolVersionHeader = []byte("ASPEVT01")

// Persistence is the durable-write seam. Any adapter error permanently makes
// its Manager unhealthy so callers cannot mistake volatile state for durable
// command history.
type Persistence interface {
	PersistCommand(stateDir string, record *Command) error
	AppendEvent(stateDir, commandID string, event Event, budget int) error
}

type diskPersistence struct{}

func (diskPersistence) PersistCommand(stateDir string, record *Command) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data)+1 > metadataMax {
		return errors.New("command metadata exceeds reserved disk budget")
	}
	dir := filepath.Join(stateDir, "commands", record.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return atomicBytes(filepath.Join(dir, "metadata.json"), append(data, '\n'), 0o600)
}

func (diskPersistence) AppendEvent(stateDir, commandID string, event Event, budget int) error {
	return appendBoundedEvent(filepath.Join(stateDir, "commands", commandID, "spool"), event, budget)
}

func atomicJSON(path string, value any, mode os.FileMode) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicBytes(path, append(data, '\n'), mode)
}

func atomicBytes(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err == nil {
		d, openErr := os.Open(dir)
		if openErr != nil {
			err = openErr
		} else {
			err = d.Sync()
			if closeErr := d.Close(); err == nil {
				err = closeErr
			}
		}
	}
	return err
}

type segment struct {
	path  string
	first uint64
	size  int64
}

func segments(dir string) ([]segment, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]segment, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".bin") {
			continue
		}
		first, err := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".bin"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid spool segment %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		result = append(result, segment{path: filepath.Join(dir, entry.Name()), first: first, size: info.Size()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].first < result[j].first })
	return result, nil
}

func readEvents(dir string) ([]Event, error) {
	parts, err := segments(dir)
	if err != nil {
		return nil, err
	}
	var events []Event
	var previous uint64
	for i, part := range parts {
		file, err := os.Open(part.path)
		if err != nil {
			return nil, err
		}
		header := make([]byte, len(spoolVersionHeader))
		if _, err = io.ReadFull(file, header); err != nil || string(header) != string(spoolVersionHeader) {
			file.Close()
			return nil, fmt.Errorf("spool segment %s has unsupported version", filepath.Base(part.path))
		}
		items, validBytes, err := decodeEvents(file)
		validBytes += int64(len(spoolVersionHeader))
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filepath.Base(part.path), err)
		}
		if validBytes != part.size {
			if i != len(parts)-1 {
				return nil, fmt.Errorf("partial non-final spool segment %s", filepath.Base(part.path))
			}
			if err = os.Truncate(part.path, validBytes); err != nil {
				return nil, err
			}
		}
		for _, event := range items {
			if event.Seq <= previous {
				return nil, errors.New("spool sequence is not strictly monotonic")
			}
			previous = event.Seq
			events = append(events, event)
		}
	}
	return events, nil
}

func decodeEvents(reader io.Reader) ([]Event, int64, error) {
	buffered := bufio.NewReader(reader)
	var events []Event
	var offset int64
	for {
		var header [spoolRecordHeader]byte
		n, err := io.ReadFull(buffered, header[:])
		if errors.Is(err, io.EOF) {
			return events, offset, nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return events, offset, nil
		}
		if err != nil {
			return nil, offset, err
		}
		size := binary.BigEndian.Uint32(header[9:])
		if size > MaxEventBytes {
			return nil, offset, fmt.Errorf("payload size %d exceeds limit", size)
		}
		data := make([]byte, size)
		if _, err = io.ReadFull(buffered, data); errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return events, offset, nil
		} else if err != nil {
			return nil, offset, err
		}
		stream := "stdout"
		if header[8] == 2 {
			stream = "stderr"
		} else if header[8] != 1 {
			return nil, offset, fmt.Errorf("invalid stream %d", header[8])
		}
		offset += int64(n) + int64(size)
		events = append(events, Event{Seq: binary.BigEndian.Uint64(header[:8]), Stream: stream, Data: data})
	}
}

func encodeEvent(event Event) []byte {
	out := make([]byte, spoolRecordHeader+len(event.Data))
	binary.BigEndian.PutUint64(out[:8], event.Seq)
	out[8] = 1
	if event.Stream == "stderr" {
		out[8] = 2
	}
	binary.BigEndian.PutUint32(out[9:13], uint32(len(event.Data)))
	copy(out[13:], event.Data)
	return out
}

func encodeEvents(events []Event) []byte {
	out := append([]byte(nil), spoolVersionHeader...)
	for _, event := range events {
		out = append(out, encodeEvent(event)...)
	}
	return out
}

func appendBoundedEvent(dir string, event Event, budget int) error {
	if len(event.Data) > MaxEventBytes {
		return errors.New("event exceeds maximum size")
	}
	record := encodeEvent(event)
	if len(record)+len(spoolVersionHeader) > budget {
		return errors.New("event exceeds spool budget")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	parts, err := segments(dir)
	if err != nil {
		return err
	}
	total := int64(0)
	for _, part := range parts {
		total += part.size
	}
	rotate := len(parts) == 0 || parts[len(parts)-1].size+int64(len(record)) > spoolSegmentBytes || total+int64(len(record)) > int64(budget)
	appendBytes := int64(len(record))
	if rotate {
		appendBytes += int64(len(spoolVersionHeader))
		for total+appendBytes > int64(budget) && len(parts) > 0 {
			if err = os.Remove(parts[0].path); err != nil {
				return err
			}
			total -= parts[0].size
			parts = parts[1:]
		}
	}
	var path string
	if rotate || len(parts) == 0 {
		path = filepath.Join(dir, fmt.Sprintf("%020d.bin", event.Seq))
	} else {
		path = parts[len(parts)-1].path
	}
	flags := os.O_WRONLY | os.O_APPEND
	payload := record
	if rotate || len(parts) == 0 {
		flags |= os.O_CREATE | os.O_EXCL
		payload = append(append([]byte(nil), spoolVersionHeader...), record...)
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(payload)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil && rotate {
		directory, openErr := os.Open(dir)
		if openErr != nil {
			return openErr
		}
		writeErr = directory.Sync()
		if closeErr := directory.Close(); writeErr == nil {
			writeErr = closeErr
		}
	}
	return writeErr
}
