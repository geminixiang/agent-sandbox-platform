package runtime

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

func RoundTrip(socket string, body []byte) ([]byte, error) {
	if len(body) == 0 || len(body) > MaxRequestBytes {
		return nil, errors.New("request envelope exceeds limit")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err = conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err = conn.Write(header[:]); err != nil {
		return nil, err
	}
	if _, err = conn.Write(body); err != nil {
		return nil, err
	}
	if err = conn.SetReadDeadline(time.Now().Add(6 * time.Minute)); err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxResponseBytes {
		return nil, errors.New("invalid response envelope")
	}
	response := make([]byte, size)
	if _, err = io.ReadFull(conn, response); err != nil {
		return nil, err
	}
	var parsed Response
	if err = json.Unmarshal(response, &parsed); err != nil {
		return nil, err
	}
	return response, nil
}
