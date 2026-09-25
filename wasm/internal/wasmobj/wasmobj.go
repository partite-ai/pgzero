// Package wasmobj reads the symbol table of relocatable wasm object files
// (the "linking" custom section, see tool-conventions Linking.md), as
// produced by clang and by wasm-ld -r.
package wasmobj

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Symbol kinds.
const (
	KindFunction = 0
	KindData     = 1
	KindGlobal   = 2
	KindSection  = 3
	KindTag      = 4
	KindTable    = 5
)

// Symbol flags.
const (
	FlagBindingWeak  = 0x1
	FlagBindingLocal = 0x2
	FlagUndefined    = 0x10
	FlagExplicitName = 0x40
)

// FuncSig is a function's wasm signature.
type FuncSig struct {
	Params, Results []byte // wasm value types
}

// Symbol is an entry of the symbol table.
type Symbol struct {
	Kind  byte
	Flags uint64
	// FlagsOffset is the file offset of the first byte of the symbol's
	// flags varint (bits 0-6 of the flags live in that byte).
	FlagsOffset int
	Name        string   // empty for unnamed (undefined, no explicit name) symbols
	Func        *FuncSig // signature, for defined functions
}

// Defined reports whether the object defines the symbol.
func (s *Symbol) Defined() bool { return s.Flags&FlagUndefined == 0 }

// Local reports whether the symbol has local binding.
func (s *Symbol) Local() bool { return s.Flags&FlagBindingLocal != 0 }

type reader struct {
	b    []byte
	i    int
	base int // file offset of b[0]
}

func (r *reader) byte() byte {
	c := r.b[r.i]
	r.i++
	return c
}

func (r *reader) uleb() uint64 {
	var v uint64
	var shift uint
	for {
		c := r.byte()
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v
		}
		shift += 7
	}
}

func (r *reader) bytes(n int) []byte {
	s := r.b[r.i : r.i+n]
	r.i += n
	return s
}

func (r *reader) name() string { return string(r.bytes(int(r.uleb()))) }

func (r *reader) limits() {
	flags := r.byte()
	r.uleb()
	if flags&1 != 0 {
		r.uleb()
	}
}

func (r *reader) sub(n int) *reader {
	s := &reader{b: r.b[r.i : r.i+n], base: r.base + r.i}
	r.i += n
	return s
}

const (
	sectionCustom   = 0
	sectionType     = 1
	sectionImport   = 2
	sectionFunction = 3
	linkingSymTable = 8
)

// Symbols returns the symbol table of a relocatable wasm object.
func Symbols(data []byte) ([]Symbol, error) {
	if len(data) < 8 || !bytes.Equal(data[:4], []byte("\x00asm")) || binary.LittleEndian.Uint32(data[4:]) != 1 {
		return nil, errors.New("not a wasm module")
	}
	r := &reader{b: data, i: 8}
	var types []FuncSig
	var importedFuncs int
	var funcTypes []uint32
	var syms []Symbol
	found := false
	for r.i < len(r.b) {
		id := r.byte()
		size := int(r.uleb())
		sec := r.sub(size)
		switch id {
		case sectionType:
			n := sec.uleb()
			for range n {
				if form := sec.byte(); form != 0x60 {
					return nil, fmt.Errorf("unsupported type form %#x", form)
				}
				var f FuncSig
				f.Params = append([]byte(nil), sec.bytes(int(sec.uleb()))...)
				f.Results = append([]byte(nil), sec.bytes(int(sec.uleb()))...)
				types = append(types, f)
			}
		case sectionImport:
			n := sec.uleb()
			for range n {
				sec.name()
				sec.name()
				switch sec.byte() {
				case 0: // func
					sec.uleb()
					importedFuncs++
				case 1: // table
					sec.byte()
					sec.limits()
				case 2: // memory
					sec.limits()
				case 3: // global
					sec.byte()
					sec.byte()
				case 4: // tag
					sec.byte()
					sec.uleb()
				}
			}
		case sectionFunction:
			n := sec.uleb()
			for range n {
				funcTypes = append(funcTypes, uint32(sec.uleb()))
			}
		case sectionCustom:
			if sec.name() != "linking" {
				continue
			}
			found = true
			sec.uleb() // version
			for sec.i < len(sec.b) {
				sub := sec.byte()
				ss := sec.sub(int(sec.uleb()))
				if sub != linkingSymTable {
					continue
				}
				n := ss.uleb()
				for range n {
					var s Symbol
					s.Kind = ss.byte()
					s.FlagsOffset = ss.base + ss.i
					s.Flags = ss.uleb()
					named := s.Defined() || s.Flags&FlagExplicitName != 0
					switch s.Kind {
					case KindFunction:
						idx := ss.uleb()
						if named {
							s.Name = ss.name()
						}
						if s.Defined() {
							def := int(idx) - importedFuncs
							if def < 0 || def >= len(funcTypes) {
								return nil, fmt.Errorf("function symbol %s: bad index", s.Name)
							}
							t := types[funcTypes[def]]
							s.Func = &t
						}
					case KindData:
						s.Name = ss.name()
						if s.Defined() {
							ss.uleb()
							ss.uleb()
							ss.uleb()
						}
					case KindGlobal, KindTag, KindTable:
						ss.uleb()
						if named {
							s.Name = ss.name()
						}
					case KindSection:
						ss.uleb()
					default:
						return nil, fmt.Errorf("unknown symbol kind %d", s.Kind)
					}
					syms = append(syms, s)
				}
			}
		}
	}
	if !found {
		return nil, errors.New("no linking section (not a relocatable object?)")
	}
	return syms, nil
}
