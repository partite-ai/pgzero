// Command mktar writes a directory tree as a reproducible gzipped tar:
// entries in sorted order, no owner names, zero timestamps.
//
// Usage: mktar -o out.tar.gz dir
package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"
)

func main() {
	out := flag.String("o", "", "output file")
	flag.Parse()
	if *out == "" || flag.NArg() != 1 {
		log.Fatal("usage: mktar -o out.tar.gz dir")
	}
	root := flag.Arg(0)

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		log.Fatal(err)
	}
	tw := tar.NewWriter(gz)
	// WalkDir visits entries in lexical order.
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h := &tar.Header{
			Name:    filepath.ToSlash(rel),
			Mode:    int64(info.Mode().Perm()),
			ModTime: time.Unix(0, 0),
			Format:  tar.FormatPAX,
		}
		switch {
		case d.IsDir():
			h.Typeflag = tar.TypeDir
			h.Name += "/"
		case info.Mode().IsRegular():
			h.Typeflag = tar.TypeReg
			h.Size = info.Size()
		default:
			log.Printf("skipping %s: not a file or directory", p)
			return nil
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			defer src.Close()
			if _, err := io.Copy(tw, src); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		log.Fatal(err)
	}
}
