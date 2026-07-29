package oauthproxy

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

const (
	kiroEventPreludeSize = 12
	kiroEventMinSize     = 16
	kiroEventMaxSize     = 16 << 20
)

type kiroEventFrame struct {
	headers map[string]string
	payload []byte
}

func readKiroEventFrame(reader io.Reader) (*kiroEventFrame, error) {
	prelude := make([]byte, kiroEventPreludeSize)
	if _, err := io.ReadFull(reader, prelude); err != nil {
		return nil, err
	}
	totalLength := int(binary.BigEndian.Uint32(prelude[0:4]))
	headerLength := int(binary.BigEndian.Uint32(prelude[4:8]))
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])
	if totalLength < kiroEventMinSize || totalLength > kiroEventMaxSize {
		return nil, fmt.Errorf("invalid AWS EventStream frame length %d", totalLength)
	}
	if headerLength < 0 || kiroEventPreludeSize+headerLength+4 > totalLength {
		return nil, fmt.Errorf("invalid AWS EventStream header length %d", headerLength)
	}
	if actual := crc32.ChecksumIEEE(prelude[:8]); actual != preludeCRC {
		return nil, fmt.Errorf("AWS EventStream prelude CRC mismatch")
	}

	remainder := make([]byte, totalLength-kiroEventPreludeSize)
	if _, err := io.ReadFull(reader, remainder); err != nil {
		return nil, err
	}
	message := append(append(make([]byte, 0, totalLength), prelude...), remainder...)
	expectedCRC := binary.BigEndian.Uint32(message[totalLength-4:])
	if actual := crc32.ChecksumIEEE(message[:totalLength-4]); actual != expectedCRC {
		return nil, fmt.Errorf("AWS EventStream message CRC mismatch")
	}
	headers, err := parseKiroEventHeaders(remainder[:headerLength])
	if err != nil {
		return nil, err
	}
	payloadEnd := len(remainder) - 4
	payload := append([]byte{}, remainder[headerLength:payloadEnd]...)
	return &kiroEventFrame{headers: headers, payload: payload}, nil
}

func parseKiroEventHeaders(raw []byte) (map[string]string, error) {
	headers := make(map[string]string)
	for offset := 0; offset < len(raw); {
		nameLength := int(raw[offset])
		offset++
		if nameLength == 0 || offset+nameLength+1 > len(raw) {
			return nil, fmt.Errorf("invalid AWS EventStream header name")
		}
		name := string(raw[offset : offset+nameLength])
		offset += nameLength
		valueType := raw[offset]
		offset++
		switch valueType {
		case 0, 1:
			// Boolean headers have no value bytes.
		case 2:
			if offset+1 > len(raw) {
				return nil, io.ErrUnexpectedEOF
			}
			offset++
		case 3:
			if offset+2 > len(raw) {
				return nil, io.ErrUnexpectedEOF
			}
			offset += 2
		case 4:
			if offset+4 > len(raw) {
				return nil, io.ErrUnexpectedEOF
			}
			offset += 4
		case 5, 8:
			if offset+8 > len(raw) {
				return nil, io.ErrUnexpectedEOF
			}
			offset += 8
		case 6, 7:
			if offset+2 > len(raw) {
				return nil, io.ErrUnexpectedEOF
			}
			valueLength := int(binary.BigEndian.Uint16(raw[offset : offset+2]))
			offset += 2
			if offset+valueLength > len(raw) {
				return nil, io.ErrUnexpectedEOF
			}
			if valueType == 7 {
				headers[name] = string(raw[offset : offset+valueLength])
			}
			offset += valueLength
		case 9:
			if offset+16 > len(raw) {
				return nil, io.ErrUnexpectedEOF
			}
			offset += 16
		default:
			return nil, fmt.Errorf("unsupported AWS EventStream header type %d", valueType)
		}
	}
	return headers, nil
}
