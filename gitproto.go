package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// zlibWriterPool reuses zlib compressors across packs. Creating a fresh
// zlib writer costs ~380 us on this VM (measured: fresh 378 us/obj vs
// pooled 6.9 us/obj for a 600 B object) because the deflate state
// allocates and zeroes ~224 KiB of tables; a pack carries one writer per
// object, so fresh writers dominated the whole clone workload.
var zlibWriterPool = &sync.Pool{New: func() any {
	zw, _ := zlib.NewWriterLevel(&bytes.Buffer{}, flate.BestSpeed)
	return zw
}}

// ---------------------------------------------------------------------------
// pkt-line framing (git wire format)
// ---------------------------------------------------------------------------

// pktLine renders one pkt-line: 4 hex digits (total length incl. header)
// followed by the payload. Payloads over 65516 bytes must be split by the
// caller; this helper does not split.
func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	n := len(payload) + 4
	out[0] = hexDigit(byte(n >> 12))
	out[1] = hexDigit(byte(n >> 8))
	out[2] = hexDigit(byte(n >> 4))
	out[3] = hexDigit(byte(n))
	copy(out[4:], payload)
	return out
}

func hexDigit(v byte) byte {
	v &= 0x0f
	if v < 10 {
		return '0' + v
	}
	return 'a' + v - 10
}

func pktString(s string) []byte { return pktLine([]byte(s)) }

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

// pktKind classifies one parsed pkt-line.
type pktKind int

const (
	kindData pktKind = iota
	kindFlush
	kindDelim
)

type pkt struct {
	kind    pktKind
	payload []byte // data payload only (kind == kindData)
}

// parsePktLines splits a whole body into pkt-lines. A trailing partial line
// or a bad length is an error; callers treat that as a malformed request.
func parsePktLines(b []byte) ([]pkt, error) {
	var out []pkt
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("short pkt-line header")
		}
		n, err := strconv.ParseUint(string(b[:4]), 16, 32)
		if err != nil {
			return nil, errors.New("bad pkt-line length")
		}
		switch n {
		case 0:
			out = append(out, pkt{kind: kindFlush})
			b = b[4:]
		case 1:
			out = append(out, pkt{kind: kindDelim})
			b = b[4:]
		case 2, 3:
			return nil, errors.New("pkt-line length too small")
		default:
			if int(n) > len(b) {
				return nil, errors.New("pkt-line overruns body")
			}
			out = append(out, pkt{kind: kindData, payload: b[4:n]})
			b = b[n:]
		}
	}
	return out, nil
}

// splitPackFromCommands parses a receive-pack body: pkt-lines up to the
// first flush are commands; everything after is the pack (possibly empty).
func splitPackFromCommands(body []byte) (cmds []string, pack []byte, err error) {
	for len(body) > 0 {
		if len(body) < 4 {
			return nil, nil, errors.New("short pkt-line header")
		}
		n, err := strconv.ParseUint(string(body[:4]), 16, 32)
		if err != nil {
			return nil, nil, errors.New("bad pkt-line length")
		}
		if n == 0 {
			return cmds, body[4:], nil
		}
		if n < 4 || int(n) > len(body) {
			return nil, nil, errors.New("bad command pkt-line")
		}
		cmds = append(cmds, string(body[4:n]))
		body = body[n:]
	}
	return cmds, nil, nil
}

// ---------------------------------------------------------------------------
// Pack reading (with OFS_DELTA / REF_DELTA resolution)
// ---------------------------------------------------------------------------

type packObject struct {
	typ  ObjType
	data []byte
}

// inflateCounted inflates one zlib stream starting at pack[pos:end] and
// reports the inflated bytes plus how many input bytes the stream consumed
// (header + deflate data + Adler-32 trailer). The bufio layer accounts for
// the decoder's read-ahead exactly.
func inflateCounted(pack []byte, pos, end int) (raw []byte, consumed int, err error) {
	src := bytes.NewReader(pack[pos:end])
	br := bufio.NewReaderSize(src, 4096)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	raw, err = io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed = (end - pos) - src.Len() - br.Buffered()
	return raw, consumed, nil
}

// applyDelta applies git delta instructions to a base object, returning the
// reconstructed object bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta varint overruns")
			}
			c := delta[pos]
			pos++
			v |= int64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size %d, want %d", srcSize, len(base))
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, tgtSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// Copy from base: offset/size bitfields, size 0 means 0x10000.
			var off, size int64
			if op&0x01 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("delta copy overruns")
				}
				off = int64(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("delta copy overruns")
				}
				off |= int64(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("delta copy overruns")
				}
				off |= int64(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("delta copy overruns")
				}
				off |= int64(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("delta copy overruns")
				}
				size = int64(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("delta copy overruns")
				}
				size |= int64(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("delta copy overruns")
				}
				size |= int64(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			if off < 0 || size < 0 || off+size > int64(len(base)) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			// Insert literal op bytes.
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta insert overruns")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if int64(len(out)) != tgtSize {
		return nil, fmt.Errorf("delta result size %d, want %d", len(out), tgtSize)
	}
	return out, nil
}

// readPack parses a packfile. resolveBase supplies objects the pack
// references but does not contain (thin packs): it is called with a base oid
// and returns its type and bytes. Returns objects keyed by oid.
func readPack(pack []byte, resolveBase func(oid string) (ObjType, []byte, bool)) (map[string]packObject, int, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, 0, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, 0, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	end := len(pack) - 20 // trailer: 20-byte pack checksum
	if end < 12 {
		return nil, 0, errors.New("pack too short")
	}
	pos := 12
	objects := make(map[string]packObject, count)
	byOffset := make(map[int]string, count)
	deltaCount := 0
	for i := 0; i < count; i++ {
		if pos >= end {
			return nil, 0, errors.New("pack truncated")
		}
		entryStart := pos
		c := pack[pos]
		pos++
		typCode := (c >> 4) & 0x7
		size := int64(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= end {
				return nil, 0, errors.New("pack truncated in size varint")
			}
			c = pack[pos]
			pos++
			size |= int64(c&0x7f) << shift
			shift += 7
		}
		var baseType ObjType
		var baseData []byte
		isDelta := false
		switch typCode {
		case 1, 2, 3, 4:
			baseType = ObjType(typCode)
		case 6: // OFS_DELTA
			isDelta = true
			deltaCount++
			if pos >= end {
				return nil, 0, errors.New("pack truncated in ofs-delta")
			}
			c = pack[pos]
			pos++
			offset := int64(c & 0x7f)
			for c&0x80 != 0 {
				if pos >= end {
					return nil, 0, errors.New("pack truncated in ofs-delta")
				}
				c = pack[pos]
				pos++
				offset = ((offset + 1) << 7) | int64(c&0x7f)
			}
			baseOff := entryStart - int(offset)
			baseOID, ok := byOffset[baseOff]
			if !ok {
				return nil, 0, fmt.Errorf("ofs-delta base at offset %d not parsed", baseOff)
			}
			base := objects[baseOID]
			baseType, baseData = base.typ, base.data
		case 7: // REF_DELTA
			isDelta = true
			deltaCount++
			if pos+20 > end {
				return nil, 0, errors.New("pack truncated in ref-delta")
			}
			baseOID := hex.EncodeToString(pack[pos : pos+20])
			pos += 20
			if base, ok := objects[baseOID]; ok {
				baseType, baseData = base.typ, base.data
			} else if resolveBase != nil {
				t, d, ok := resolveBase(baseOID)
				if !ok {
					return nil, 0, fmt.Errorf("ref-delta base %s not found", baseOID)
				}
				baseType, baseData = t, d
			} else {
				return nil, 0, fmt.Errorf("ref-delta base %s not found", baseOID)
			}
		default:
			return nil, 0, fmt.Errorf("bad pack object type %d", typCode)
		}
		raw, consumed, err := inflateCounted(pack, pos, end)
		if err != nil {
			return nil, 0, fmt.Errorf("zlib inflate failed: %w", err)
		}
		pos += consumed
		var data []byte
		if isDelta {
			data, err = applyDelta(baseData, raw)
			if err != nil {
				return nil, 0, fmt.Errorf("delta apply failed: %w", err)
			}
		} else {
			data = raw
			if int64(len(data)) != size {
				return nil, 0, fmt.Errorf("object size %d, want %d", len(data), size)
			}
		}
		oid := hashObject(baseType, data)
		objects[oid] = packObject{typ: baseType, data: data}
		byOffset[entryStart] = oid
	}
	// Verify the pack trailer checksum over all preceding bytes.
	want := sha1.Sum(pack[:end])
	if !bytes.Equal(want[:], pack[end:]) {
		return nil, 0, errors.New("pack checksum mismatch")
	}
	return objects, deltaCount, nil
}

// ---------------------------------------------------------------------------
// Pack writing (plain, undelted objects) and sideband framing
// ---------------------------------------------------------------------------

// writePack renders a packfile holding the given objects in order. Objects
// are written undelted: every client in the suite accepts plain packs, and
// it keeps the hot path allocation-free of delta search.
func writePack(objs []packObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	buf.Write(hdr[:])
	for _, o := range objs {
		// Type/size varint header: first byte (code<<4)|(size&0xF), then
		// 7-bit little-endian chunks with the high bit set except last.
		size := len(o.data)
		first := byte(o.typ)<<4 | byte(size&0x0f)
		size >>= 4
		if size == 0 {
			buf.WriteByte(first)
		} else {
			buf.WriteByte(first | 0x80)
			for {
				b := byte(size & 0x7f)
				size >>= 7
				if size == 0 {
					buf.WriteByte(b)
					break
				}
				buf.WriteByte(b | 0x80)
			}
		}
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&buf)
		zw.Write(o.data)
		zw.Close()
		zlibWriterPool.Put(zw)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// sidebandChunks wraps pack bytes into sideband pkt-lines (band 1), each
// carrying at most 32768 bytes of pack data, and appends the final flush.
func sidebandChunks(pack []byte) []byte {
	var out []byte
	const maxChunk = 32768
	for len(pack) > 0 {
		n := len(pack)
		if n > maxChunk {
			n = maxChunk
		}
		payload := make([]byte, 1+n)
		payload[0] = 1
		copy(payload[1:], pack[:n])
		out = append(out, pktLine(payload)...)
		pack = pack[n:]
	}
	out = append(out, pktFlush()...)
	return out
}

// emptyPack is a valid packfile holding zero objects (header + trailer).
func emptyPack() []byte {
	return writePack(nil)
}
