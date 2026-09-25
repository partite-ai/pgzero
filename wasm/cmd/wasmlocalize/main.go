// Command wasmlocalize gives local binding to the global symbols a
// relocatable wasm object defines, except for a list of symbols to keep.
//
// Usage: wasmlocalize -keep names.txt in.o out.o
//
// Local symbols take no part in symbol resolution between objects, so the
// object can be linked into a program that defines the same names, while
// its internal references (which go through symbol indices) keep pointing
// at its own definitions. pgzero uses this to link libpq, together with its
// own frontend builds of libpgcommon and libpgport, into the backend.
//
// The binding flag lives in the first byte of each symbol's flags varint,
// so the object is patched in place (llvm-objcopy cannot do this for wasm).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/partite-ai/pgzero/wasm/internal/wasmobj"
)

func main() {
	keepFile := flag.String("keep", "", "file of symbol names to keep global (one per line; # comments)")
	flag.Parse()
	if flag.NArg() != 2 {
		log.Fatal("usage: wasmlocalize -keep names.txt in.o out.o")
	}
	keep := map[string]bool{}
	if *keepFile != "" {
		f, err := os.Open(*keepFile)
		if err != nil {
			log.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			keep[strings.Fields(line)[0]] = true
		}
		f.Close()
	}

	data, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	syms, err := wasmobj.Symbols(data)
	if err != nil {
		log.Fatal(err)
	}
	localized, kept := 0, 0
	for _, s := range syms {
		if !s.Defined() || s.Local() || s.Name == "" {
			continue
		}
		if s.Kind != wasmobj.KindFunction && s.Kind != wasmobj.KindData {
			continue
		}
		if keep[s.Name] {
			kept++
			continue
		}
		data[s.FlagsOffset] |= wasmobj.FlagBindingLocal
		localized++
	}
	if err := os.WriteFile(flag.Arg(1), data, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wasmlocalize: %d symbols localized, %d kept global\n", localized, kept)
}
