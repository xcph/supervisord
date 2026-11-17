package process

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	keeperFrameReady    byte = 1
	keeperFrameSpawn    byte = 2
	keeperFrameStarted  byte = 3
	keeperFrameSignal   byte = 4
	keeperFrameExited   byte = 5
	keeperFrameShutdown byte = 6
	keeperFrameError    byte = 7
)

const keeperFrameMax = 1 << 20

func writeKeeperFrame(w io.Writer, kind byte, payload []byte) error {
	if len(payload) > keeperFrameMax {
		return fmt.Errorf("keeper frame too large")
	}
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(payload)+1))
	hdr[4] = kind
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func readKeeperFrame(r io.Reader) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > keeperFrameMax {
		return 0, nil, fmt.Errorf("keeper frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

func encodeSpawn(dir string, argv, env []string) ([]byte, error) {
	var buf []byte
	var err error
	if buf, err = appendString(buf, dir); err != nil {
		return nil, err
	}
	if buf, err = appendStrings(buf, argv); err != nil {
		return nil, err
	}
	return appendStrings(buf, env)
}

func decodeSpawn(payload []byte) (dir string, argv, env []string, err error) {
	dir, payload, err = takeString(payload)
	if err != nil {
		return "", nil, nil, err
	}
	argv, payload, err = takeStrings(payload)
	if err != nil {
		return "", nil, nil, err
	}
	env, _, err = takeStrings(payload)
	return dir, argv, env, err
}

func appendStrings(buf []byte, items []string) ([]byte, error) {
	var nbuf [4]byte
	binary.BigEndian.PutUint32(nbuf[:], uint32(len(items)))
	buf = append(buf, nbuf[:]...)
	for _, item := range items {
		var err error
		buf, err = appendString(buf, item)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func appendString(buf []byte, s string) ([]byte, error) {
	if len(s) > keeperFrameMax {
		return nil, fmt.Errorf("keeper string too large")
	}
	var nbuf [4]byte
	binary.BigEndian.PutUint32(nbuf[:], uint32(len(s)))
	buf = append(buf, nbuf[:]...)
	return append(buf, s...), nil
}

func takeStrings(payload []byte) ([]string, []byte, error) {
	if len(payload) < 4 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	n := binary.BigEndian.Uint32(payload[:4])
	payload = payload[4:]
	out := make([]string, 0, n)
	for i := uint32(0); i < n; i++ {
		s, rest, err := takeString(payload)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, s)
		payload = rest
	}
	return out, payload, nil
}

func takeString(payload []byte) (string, []byte, error) {
	if len(payload) < 4 {
		return "", nil, io.ErrUnexpectedEOF
	}
	n := binary.BigEndian.Uint32(payload[:4])
	payload = payload[4:]
	if uint32(len(payload)) < n {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(payload[:n]), payload[n:], nil
}
