package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// GGUFMeta holds the subset of GGUF file metadata useful for capability detection.
type GGUFMeta struct {
	Architecture  string // general.architecture
	ContextLength uint32 // {arch}.context_length (native max from model weights)
	ChatTemplate  string // tokenizer.chat_template
}

// readGGUFMeta opens path, reads the GGUF header and metadata KV section,
// and returns the fields relevant to capability detection.
// Only the first shard needs to be read for split models.
func readGGUFMeta(path string) (*GGUFMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16) // 64 KB read buffer

	// Magic
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("read magic: %w", err)
	}
	if string(magic[:]) != "GGUF" {
		return nil, fmt.Errorf("not a GGUF file")
	}

	// Version
	var version uint32
	if err := binary.Read(r, binary.LittleEndian, &version); err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	if version < 2 {
		return nil, fmt.Errorf("GGUF version %d not supported (need v2+)", version)
	}

	// n_tensors (v2+: uint64; must be read to advance past it)
	var nTensors uint64
	if err := binary.Read(r, binary.LittleEndian, &nTensors); err != nil {
		return nil, fmt.Errorf("read n_tensors: %w", err)
	}

	// n_kv
	var nKV uint64
	if err := binary.Read(r, binary.LittleEndian, &nKV); err != nil {
		return nil, fmt.Errorf("read n_kv: %w", err)
	}

	// Stream through KV pairs, keeping only the keys we need.
	// We collect all *.context_length keys since we don't know the arch name yet.
	meta := &GGUFMeta{}
	contextByArch := map[string]uint32{}

	for i := uint64(0); i < nKV; i++ {
		key, err := ggufReadStr(r)
		if err != nil {
			return nil, fmt.Errorf("kv[%d] key: %w", i, err)
		}
		var typ uint32
		if err := binary.Read(r, binary.LittleEndian, &typ); err != nil {
			return nil, fmt.Errorf("kv[%d] type: %w", i, err)
		}

		switch {
		case key == "general.architecture" && typ == 8:
			s, err := ggufReadStr(r)
			if err != nil {
				return nil, fmt.Errorf("kv[%d] architecture: %w", i, err)
			}
			meta.Architecture = s

		case key == "tokenizer.chat_template" && typ == 8:
			s, err := ggufReadStr(r)
			if err != nil {
				return nil, fmt.Errorf("kv[%d] chat_template: %w", i, err)
			}
			meta.ChatTemplate = s

		case strings.HasSuffix(key, ".context_length") && (typ == 4 || typ == 10):
			// typ 4 = uint32, typ 10 = uint64
			arch := strings.TrimSuffix(key, ".context_length")
			if typ == 4 {
				var v uint32
				if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
					return nil, fmt.Errorf("kv[%d] context_length: %w", i, err)
				}
				contextByArch[arch] = v
			} else {
				var v uint64
				if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
					return nil, fmt.Errorf("kv[%d] context_length: %w", i, err)
				}
				contextByArch[arch] = uint32(v)
			}

		default:
			if err := ggufDiscard(r, typ); err != nil {
				return nil, fmt.Errorf("kv[%d] discard %q: %w", i, key, err)
			}
		}
	}

	if meta.Architecture != "" {
		meta.ContextLength = contextByArch[meta.Architecture]
	}

	return meta, nil
}

// ggufDiscard reads and discards one GGUF value of the given type.
func ggufDiscard(r io.Reader, typ uint32) error {
	if size := ggufScalarSize(typ); size > 0 {
		_, err := io.CopyN(io.Discard, r, int64(size))
		return err
	}
	switch typ {
	case 8: // string
		var n uint64
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			return err
		}
		_, err := io.CopyN(io.Discard, r, int64(n))
		return err
	case 9: // array
		var arrTyp uint32
		if err := binary.Read(r, binary.LittleEndian, &arrTyp); err != nil {
			return err
		}
		var arrN uint64
		if err := binary.Read(r, binary.LittleEndian, &arrN); err != nil {
			return err
		}
		// Fixed-size element arrays (e.g. token scores, token types): discard in one shot.
		if size := ggufScalarSize(arrTyp); size > 0 {
			_, err := io.CopyN(io.Discard, r, int64(arrN)*int64(size))
			return err
		}
		// Variable-size elements (strings, nested arrays): discard one by one.
		for j := uint64(0); j < arrN; j++ {
			if err := ggufDiscard(r, arrTyp); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown GGUF type %d", typ)
}

// ggufScalarSize returns the byte size of a fixed-width GGUF type, or -1 for variable-width.
func ggufScalarSize(typ uint32) int64 {
	switch typ {
	case 0, 1, 7:
		return 1 // uint8, int8, bool
	case 2, 3:
		return 2 // uint16, int16
	case 4, 5, 6:
		return 4 // uint32, int32, float32
	case 10, 11, 12:
		return 8 // uint64, int64, float64
	}
	return -1
}

// ggufReadStr reads a GGUF length-prefixed string.
func ggufReadStr(r io.Reader) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > 1<<24 {
		return "", fmt.Errorf("string length %d exceeds 16 MiB sanity limit", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// ggufReadValue reads a typed GGUF value (used only for context_length; kept for completeness).
func ggufReadValue(r io.Reader, typ uint32) (interface{}, error) {
	switch typ {
	case 0:
		var v uint8
		return v, binary.Read(r, binary.LittleEndian, &v)
	case 1:
		var v int8
		return v, binary.Read(r, binary.LittleEndian, &v)
	case 2:
		var v uint16
		return v, binary.Read(r, binary.LittleEndian, &v)
	case 3:
		var v int16
		return v, binary.Read(r, binary.LittleEndian, &v)
	case 4:
		var v uint32
		return v, binary.Read(r, binary.LittleEndian, &v)
	case 5:
		var v int32
		return v, binary.Read(r, binary.LittleEndian, &v)
	case 6:
		var bits uint32
		if err := binary.Read(r, binary.LittleEndian, &bits); err != nil {
			return nil, err
		}
		return math.Float32frombits(bits), nil
	case 7:
		var v uint8
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return v != 0, nil
	case 8:
		return ggufReadStr(r)
	case 10:
		var v uint64
		return v, binary.Read(r, binary.LittleEndian, &v)
	case 11:
		var v int64
		return v, binary.Read(r, binary.LittleEndian, &v)
	case 12:
		var bits uint64
		if err := binary.Read(r, binary.LittleEndian, &bits); err != nil {
			return nil, err
		}
		return math.Float64frombits(bits), nil
	}
	return nil, fmt.Errorf("unknown GGUF type %d", typ)
}

// GGUFHasToolCall returns true if the chat template contains tool-calling patterns.
func GGUFHasToolCall(template string) bool {
	return template != "" && (
		strings.Contains(template, "tools") ||
		strings.Contains(template, "tool_calls") ||
		strings.Contains(template, "[TOOL_CALLS]") ||
		strings.Contains(template, "<tool_call>"))
}

// GGUFHasReasoning returns true if the chat template contains reasoning/thinking patterns.
func GGUFHasReasoning(template string) bool {
	return template != "" && (
		strings.Contains(template, "<think>") ||
		strings.Contains(template, "<|think|>") ||
		strings.Contains(template, "enable_thinking") ||
		strings.Contains(template, "<|thinking|>"))
}
