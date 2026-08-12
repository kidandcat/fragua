// Package odb exports a minimal ODB++ .tgz subset (placeholder for full port).
package odb

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

// Export writes a minimal ODB++ tarball that preserves the job tree skeleton.
// Full feature parity with crates/pcb-odb is incomplete; this keeps the API surface.
func Export(board *core.Board, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	files := map[string]string{
		"matrix/matrix": "STEP {\n    COL=1\n    NAME=PCB\n}\n",
		"misc/info":     fmt.Sprintf("JOB_NAME=fragua\nUNITS=MM\nGENERATED=%s\n", time.Now().UTC().Format(time.RFC3339)),
		"steps/pcb/eda/data": "UNITS=MM\n",
	}
	if board != nil && board.Outline != nil {
		files["steps/pcb/profile"] = fmt.Sprintf(
			"OB %.6f %.6f\nOS %.6f %.6f\nOS %.6f %.6f\nOS %.6f %.6f\nOS %.6f %.6f\nOE\n",
			board.Outline.Min.X.ToMM(), board.Outline.Min.Y.ToMM(),
			board.Outline.Max.X.ToMM(), board.Outline.Min.Y.ToMM(),
			board.Outline.Max.X.ToMM(), board.Outline.Max.Y.ToMM(),
			board.Outline.Min.X.ToMM(), board.Outline.Max.Y.ToMM(),
			board.Outline.Min.X.ToMM(), board.Outline.Min.Y.ToMM(),
		)
	}
	for name, body := range files {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return err
		}
	}
	return nil
}
